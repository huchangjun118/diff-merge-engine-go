// Package store provides content-addressed storage with atomic commits,
// crash recovery, linear version history and idempotent patch submission.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// Store is a content-addressed version store rooted at a directory.
//
// Versions form a single linear chain anchored at HEAD. Submissions are
// serialized in-process: every patch is merged onto the current HEAD rather
// than onto the submitter's stale base. A given patch (identified by the
// hash of its canonical text) applies at most once; repeat, late or
// out-of-order submissions return the committed result without producing a
// second change.
type Store struct {
	root string
	mu   sync.Mutex // serializes in-process operations
}

// Open opens (or creates) a store at root and replays crash recovery.
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

// InitBase establishes the initial (empty-history) version and points HEAD at
// it. It is idempotent while the base content is unchanged; re-initializing
// with different content fails with KindConflict (history is append-only).
func (s *Store) InitBase(content string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	head, err := s.readHead()
	if err != nil {
		return "", err
	}
	if head != "" {
		current, err := s.Get(head)
		if err != nil {
			return "", err
		}
		if current != content {
			return "", derr.New(derr.KindConflict, "store already initialized with different base content")
		}
		return head, nil
	}
	hash, err := s.Put(content)
	if err != nil {
		return "", err
	}
	if err := s.writeHead(hash); err != nil {
		return "", err
	}
	return hash, nil
}

// Head returns the content hash of the latest committed version. The hash is
// empty when no base has been initialized.
func (s *Store) Head() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readHead()
}

// Submit merges p onto the current HEAD ("late patches land on the newest
// committed version"), commits the result and advances HEAD.
//
// Semantics:
//
//   - The base is always the latest committed version, never the base hash
//     the submitter computed against.
//   - Submission is idempotent by canonical patch text. The same patch
//     submitted twice (including concurrent, repeated or reordered arrival)
//     returns the hash it first produced and changes nothing the second time.
//   - Every successful submission appends exactly one commit; ordering is the
//     serialization order of arrival, so the final state is uniquely
//     determined.
//   - If the patch cannot apply to HEAD (stale/conflicting change, malformed
//     or encoding failure) HEAD is left untouched and the typed error is
//     returned.
func (s *Store) Submit(p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := patch.Validate(p); err != nil {
		return "", err
	}
	patchHash := Hash(p.Format())

	head, err := s.readHead()
	if err != nil {
		return "", err
	}

	// Idempotency: walk the committed chain from HEAD. The chain is the sole
	// dedup authority, which keeps dedup consistent even across crashes.
	if resultHash, ok, err := s.findPatchInChain(head, patchHash); err != nil {
		return "", err
	} else if ok {
		return resultHash, nil
	}

	var base string
	if head != "" {
		base, err = s.Get(head)
		if err != nil {
			return "", err
		}
	}
	result, err := patch.Apply(base, p, opts)
	if err != nil {
		return "", err
	}
	resultHash, err := s.Put(result)
	if err != nil {
		return "", err
	}

	seq := s.nextSeq(head)
	commit := formatCommit(head, resultHash, patchHash, seq)
	// 1) Commit object first (an orphaned commit created by a crash is inert;
	//    it is never reachable until HEAD points at it).
	if err := s.atomicWrite(s.commitPath(resultHash), commit); err != nil {
		return "", err
	}
	// 2) Publish by moving HEAD last. A crash before this point leaves the
	//    store at the previous consistent version.
	if err := s.writeHead(resultHash); err != nil {
		return "", err
	}
	return resultHash, nil
}

// ApplyPatch applies a patch to the content identified by baseHash and stores
// the result WITHOUT advancing HEAD. It remains available for direct,
// caller-managed version graphs; the linear collaborative history is built
// with Submit. It is idempotent for the same (baseHash, patch) pair.
func (s *Store) ApplyPatch(baseHash string, p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	patchHash := Hash(p.Format())
	idemKey := baseHash + ":" + patchHash

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
	if err := s.commit(baseHash, resultHash, patchHash, idemKey); err != nil {
		return "", err
	}
	return resultHash, nil
}

// findPatchInChain walks commits from head back to the root, looking for a
// commit whose patch hash equals patchHash. It returns that commit's result
// hash.
func (s *Store) findPatchInChain(head, patchHash string) (string, bool, error) {
	cur := head
	for cur != "" {
		c, err := s.readCommit(cur)
		if err != nil {
			// The initialized base has no commit object. If its blob exists,
			// it is the legitimate chain root and the walk ends here.
			if _, gerr := s.Get(cur); gerr == nil {
				return "", false, nil
			}
			return "", false, err
		}
		if c.patchHash == patchHash {
			return cur, true, nil
		}
		cur = c.parent
	}
	return "", false, nil
}

func (s *Store) nextSeq(head string) int64 {
	if head == "" {
		return 1
	}
	c, err := s.readCommit(head)
	if err != nil {
		// HEAD is the uncommitted initialized base.
		if _, gerr := s.Get(head); gerr == nil {
			return 1
		}
		return 1
	}
	return c.seq + 1
}

type commitRecord struct {
	parent     string
	resultHash string
	patchHash  string
	seq        int64
}

func formatCommit(parent, resultHash, patchHash string, seq int64) string {
	return fmt.Sprintf("parent=%s\nresult=%s\npatch=%s\nseq=%d\n", parent, resultHash, patchHash, seq)
}

func parseCommit(text string) (commitRecord, error) {
	var c commitRecord
	c.seq = -1
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			return c, derr.New(derr.KindCorrupt, "malformed commit line: "+line)
		}
		switch kv[0] {
		case "parent":
			c.parent = kv[1]
		case "result":
			c.resultHash = kv[1]
		case "patch":
			c.patchHash = kv[1]
		case "seq":
			n, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return c, derr.Wrap(derr.KindCorrupt, "bad commit seq", err)
			}
			c.seq = n
		}
	}
	return c, nil
}

func (s *Store) readCommit(resultHash string) (commitRecord, error) {
	data, err := os.ReadFile(s.commitPath(resultHash))
	if err != nil {
		if os.IsNotExist(err) {
			return commitRecord{}, derr.New(derr.KindCorrupt, "commit missing: "+resultHash)
		}
		return commitRecord{}, derr.Wrap(derr.KindCorrupt, "cannot read commit", err)
	}
	c, err := parseCommit(string(data))
	if err != nil {
		return c, err
	}
	if c.resultHash != resultHash || c.patchHash == "" || c.seq < 0 {
		return c, derr.New(derr.KindCorrupt, "commit content inconsistent: "+resultHash)
	}
	if _, err := s.Get(resultHash); err != nil {
		return c, err
	}
	return c, nil
}

// commit records an off-chain result for the non-linear ApplyPatch API. It is
// intentionally not written to commits/ (which is reserved for Submit's
// HEAD chain); the idempotency mapping is the sole record, keyed by
// (baseHash, patchHash).
func (s *Store) commit(baseHash, resultHash, patchHash, idemKey string) error {
	_ = baseHash
	_ = patchHash
	return s.atomicWrite(s.idemPath(idemKey), resultHash)
}

// History returns result hashes on the committed chain in chronological
// order, starting at the initialized base.
func (s *Store) History() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := s.readHead()
	if err != nil {
		return nil, err
	}
	var rev []string
	cur := head
	for cur != "" {
		rev = append(rev, cur)
		c, err := s.readCommit(cur)
		if err != nil {
			// The initialized base has no commit object; it terminates the
			// chain.
			if derr.IsKind(err, derr.KindCorrupt) {
				if _, gerr := s.Get(cur); gerr == nil {
					break
				}
			}
			return nil, err
		}
		cur = c.parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// Recover rolls back incomplete operations after a crash: orphaned temp
// files are removed and the HEAD chain is verified so the store either opens
// on a fully committed version or reports corruption rather than a
// half-written state.
func (s *Store) Recover() error {
	tmpDir := filepath.Join(s.root, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot read tmp", err)
	}
	for _, e := range entries {
		os.Remove(filepath.Join(tmpDir, e.Name()))
	}
	// Stray sibling temp files from ApplyStreamToFile are outside tmp; they
	// live at the caller's destination paths and are left to callers.

	head, err := s.readHead()
	if err != nil {
		return err
	}
	if head == "" {
		return nil
	}
	// Validate the reachable chain. The root (initialized base) may have no
	// commit object; every other node must have a valid commit and blob.
	cur := head
	depth := 0
	for cur != "" {
		c, err := s.readCommit(cur)
		if err != nil {
			if _, gerr := s.Get(cur); gerr == nil {
				break // base without commit object: legitimate chain root
			}
			return derr.Wrap(derr.KindCorrupt, "HEAD chain broken at "+cur, err)
		}
		if c.seq < 1 {
			return derr.New(derr.KindCorrupt, "commit with invalid seq: "+cur)
		}
		cur = c.parent
		depth++
		if depth > 1_000_000 {
			return derr.New(derr.KindCorrupt, "commit chain cycle or too deep")
		}
	}
	return nil
}

func (s *Store) headPath() string {
	return filepath.Join(s.root, "HEAD")
}

func (s *Store) readHead() (string, error) {
	data, err := os.ReadFile(s.headPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", derr.Wrap(derr.KindCorrupt, "cannot read HEAD", err)
	}
	h := strings.TrimSpace(string(data))
	if h == "" {
		return "", derr.New(derr.KindCorrupt, "HEAD is empty")
	}
	if len(h) != 64 {
		return "", derr.New(derr.KindCorrupt, "HEAD is not a valid hash")
	}
	return h, nil
}

func (s *Store) writeHead(hash string) error {
	return s.atomicWrite(s.headPath(), hash+"\n")
}

// atomicWrite writes content to a temp file, fsyncs and renames it into place.
func (s *Store) atomicWrite(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "obj-*")
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
