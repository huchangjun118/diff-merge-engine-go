// Package store provides content-addressed storage with atomic commits,
// crash recovery and idempotent patch application.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// Store is a content-addressed version store rooted at a directory.
type Store struct {
	root string
	mu   sync.Mutex // serializes in-process operations
}

// Open opens (or creates) a store at root.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, derr.Wrap(derr.KindCorrupt, "cannot create store root", err)
	}
	s := &Store{root: root}
	if err := s.ensureLayout(); err != nil {
		return nil, err
	}
	if err := s.Recover(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureLayout() error {
	for _, d := range []string{"blobs", "commits", "locks", "tmp"} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			return derr.Wrap(derr.KindCorrupt, "cannot create store layout", err)
		}
	}
	return nil
}

// Hash returns the content hash used by the store.
func Hash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// Has reports whether a blob with the given hash exists.
func (s *Store) Has(hash string) bool {
	_, err := os.Stat(s.blobPath(hash))
	return err == nil
}

// Put stores content and returns its hash. It is a no-op if the blob exists.
func (s *Store) Put(content string) (string, error) {
	hash := Hash(content)
	if s.Has(hash) {
		return hash, nil
	}
	if err := s.atomicWrite(s.blobPath(hash), content); err != nil {
		return "", err
	}
	return hash, nil
}

// Get retrieves content by hash. It verifies the content hash to detect
// corruption.
func (s *Store) Get(hash string) (string, error) {
	data, err := os.ReadFile(s.blobPath(hash))
	if err != nil {
		if os.IsNotExist(err) {
			return "", derr.New(derr.KindCorrupt, "blob not found: "+hash)
		}
		return "", derr.Wrap(derr.KindCorrupt, "cannot read blob", err)
	}
	content := string(data)
	if Hash(content) != hash {
		return "", derr.New(derr.KindCorrupt, "blob hash mismatch: "+hash)
	}
	return content, nil
}

// ApplyPatch applies a patch to the content identified by baseHash and stores
// the result. It is idempotent: applying the same patch to the same base
// returns the previously computed result without re-applying.
func (s *Store) ApplyPatch(baseHash string, p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	patchHash := Hash(p.Format())
	idemKey := baseHash + ":" + patchHash

	// Check idempotency.
	if resultHash, ok := s.lookupIdem(idemKey); ok {
		return resultHash, nil
	}

	base, err := s.Get(baseHash)
	if err != nil {
		return "", err
	}
	result, err := patch.Apply(base, p, opts)
	if err != nil {
		return "", err
	}
	resultHash, err := s.Put(result)
	if err != nil {
		return "", err
	}
	// Record the commit and idempotency mapping atomically.
	if err := s.commit(baseHash, resultHash, patchHash, idemKey); err != nil {
		return "", err
	}
	return resultHash, nil
}

// commit records a version commit and idempotency mapping.
func (s *Store) commit(baseHash, resultHash, patchHash, idemKey string) error {
	commit := fmt.Sprintf("base=%s\nresult=%s\npatch=%s\n", baseHash, resultHash, patchHash)
	if err := s.atomicWrite(s.commitPath(resultHash), commit); err != nil {
		return err
	}
	if err := s.atomicWrite(s.idemPath(idemKey), resultHash); err != nil {
		return err
	}
	return nil
}

// History returns the list of commit hashes in chronological order.
func (s *Store) History() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "commits"))
	if err != nil {
		return nil, derr.Wrap(derr.KindCorrupt, "cannot read commits", err)
	}
	var hashes []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), "idem-") {
			hashes = append(hashes, e.Name())
		}
	}
	sort.Strings(hashes)
	return hashes, nil
}

// Recover rolls back incomplete operations after a crash. It removes
// orphaned temp files and incomplete commits.
func (s *Store) Recover() error {
	// Clean tmp directory.
	tmpDir := filepath.Join(s.root, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot read tmp", err)
	}
	for _, e := range entries {
		os.Remove(filepath.Join(tmpDir, e.Name()))
	}
	return nil
}

// atomicWrite writes content to a temp file, fsyncs and renames it into place.
func (s *Store) atomicWrite(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "blob-*")
	if err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot create temp file", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // clean up on failure

	if _, err := io.WriteString(tmp, content); err != nil {
		tmp.Close()
		return derr.Wrap(derr.KindCorrupt, "cannot write temp file", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return derr.Wrap(derr.KindCorrupt, "cannot sync temp file", err)
	}
	if err := tmp.Close(); err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot close temp file", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot rename temp file", err)
	}
	return nil
}

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.root, "blobs", hash)
}

func (s *Store) commitPath(hash string) string {
	return filepath.Join(s.root, "commits", hash)
}

func (s *Store) idemPath(key string) string {
	return filepath.Join(s.root, "commits", "idem-"+Hash(key))
}

func (s *Store) lookupIdem(key string) (string, bool) {
	data, err := os.ReadFile(s.idemPath(key))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}
