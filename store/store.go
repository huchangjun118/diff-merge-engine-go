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

// Open opens (or creates) a store at root. Any interrupted transaction from a
// previous process is rolled back to a consistent committed version before
// the store becomes usable.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, derr.Wrap(derr.KindCorrupt, "cannot create store root", err)
	}
	s := &Store{root: root}
	if err := s.ensureLayout(); err != nil {
		return nil, err
	}
	if err := s.recoverLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureLayout() error {
	for _, d := range []string{dirBlobs, dirCommits, dirChanges, dirLocks, dirTmp} {
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
//
// Note: this is a direct one-shot application and is intentionally separate
// from Submit, which stacks collaborator changes onto the latest tip.
func (s *Store) ApplyPatch(baseHash string, p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	patchHash := Hash(p.Format())
	idemKey := baseHash + ":" + patchHash

	if resultHash, ok := s.lookupIdem(idemKey); ok {
		if s.Has(resultHash) {
			return resultHash, nil
		}
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
	if err := s.commit(baseHash, resultHash, patchHash, idemKey); err != nil {
		return "", err
	}
	return resultHash, nil
}

// commit records a one-shot application result and its idempotency mapping.
func (s *Store) commit(baseHash, resultHash, patchHash, idemKey string) error {
	commit := fmt.Sprintf("base=%s\nresult=%s\npatch=%s\n", baseHash, resultHash, patchHash)
	if err := s.atomicWrite(filepath.Join(s.root, dirCommits, "apply-"+resultHash), commit); err != nil {
		return err
	}
	if err := s.atomicWrite(filepath.Join(s.root, dirCommits, "idem-"+Hash(idemKey)), resultHash); err != nil {
		return err
	}
	return nil
}

// History returns the content hashes on the linear committed chain, oldest
// first, by walking HEAD backwards through parent pointers.
func (s *Store) History() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.branch()
	if err != nil {
		return nil, err
	}
	defer b.Close()
	st, err := b.readHEAD()
	if derr.IsKind(err, derr.KindCorrupt) {
		if rerr := b.recoverHEAD(); rerr != nil {
			return nil, rerr
		}
		st, err = b.readHEAD()
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var chain []string
	cur := st.head
	seen := map[string]bool{}
	for cur != "" && !seen[cur] {
		seen[cur] = true
		chain = append(chain, cur)
		data, err := os.ReadFile(filepath.Join(s.root, dirCommits, cur))
		if err != nil {
			break
		}
		rec, err := parseCommit(string(data), cur)
		if err != nil {
			break
		}
		cur = rec.parent
	}
	// Reverse to oldest-first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// Recover rolls back incomplete operations after a crash: it removes orphaned
// temp files and rebuilds HEAD so it points at a fully committed version.
func (s *Store) Recover() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverLocked()
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
	return filepath.Join(s.root, dirBlobs, hash)
}

func (s *Store) lookupIdem(key string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(s.root, dirCommits, "idem-"+Hash(key)))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}
