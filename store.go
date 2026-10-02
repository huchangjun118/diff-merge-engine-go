package diffmerge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Commit identifies one committed version of the managed content.
type Commit struct {
	ID        string `json:"id"`
	Parent    string `json:"parent"`
	AuthorID  string `json:"author"`
	PatchID   string `json:"patch_id"`
	BaseID    string `json:"base_id"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
	Conflict  bool   `json:"conflict"`
}

// SubmitRequest is one collaborator submission.
type SubmitRequest struct {
	Patch    *Patch
	PatchID  string // "" derives from Patch
	BaseID   string // "" means the current HEAD
	AuthorID string
}

// SubmitResult reports what happened to a submission.
type SubmitResult struct {
	Commit       *Commit
	HadConflicts bool
	Duplicate    bool
	ConflictInfo []ConflictInfo
}

// StoreOptions configures a collaboration store.
type StoreOptions struct {
	Apply                  ApplyOptions
	Merge                  MergeOptions
	KeepConflictsInHistory bool
}

const (
	blobsDir   = "blobs"
	journalDir = "journal"
	tmpDir     = "tmp"
	lockName   = "lock"
	headName   = "HEAD"
	blobMode   = 0o600
	dirMode    = 0o700
)

// Store is an append-only, crash-safe collaboration store.
type Store struct {
	dir       string
	opts      StoreOptions
	mu        sync.Mutex
	lockF     *os.File
	commits   []*Commit
	byID      map[string]*Commit
	patchSeen map[string]string // patchID -> commit ID
	head      string
	closed    bool
}

// OpenStore opens (or creates) a store and recovers it to a consistent state.
func OpenStore(ctx context.Context, dir string, opts StoreOptions) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, blobsDir), dirMode); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, journalDir), dirMode); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, tmpDir), dirMode); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, lockName)
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, blobMode)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &StateError{Msg: "store is locked by another process"}
		}
		return nil, err
	}
	s := &Store{
		dir:       dir,
		opts:      opts,
		lockF:     lf,
		byID:      map[string]*Commit{},
		patchSeen: map[string]string{},
	}
	fail := func(e error) (*Store, error) {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		return nil, e
	}
	// Remove any leftover temp files from a crashed process.
	if err := removeTmpDirContents(dir); err != nil {
		return fail(err)
	}
	if err := s.replay(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

func removeTmpDirContents(dir string) error {
	entries, err := os.ReadDir(filepath.Join(dir, tmpDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, tmpDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the store and its file lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.lockF != nil {
		_ = syscall.Flock(int(s.lockF.Fd()), syscall.LOCK_UN)
		err = s.lockF.Close()
	}
	return err
}

// blobPath returns the content-addressed path of a blob ("ab/cdef...").
func blobPath(dir, id string) string {
	if len(id) < 4 {
		return filepath.Join(dir, blobsDir, id)
	}
	return filepath.Join(dir, blobsDir, id[:2], id[2:])
}

// writeBlobAtomically writes content to the content-addressed blob path using
// a temp file + fsync + rename. Idempotent: an existing identical blob is a
// no-op; a hash collision with different content is rejected.
func (s *Store) writeBlobAtomically(ctx context.Context, r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, tmpDir), "blob-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	n, err := io.Copy(mw, r)
	if err != nil {
		cleanup()
		return "", 0, err
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	id := hex.EncodeToString(h.Sum(nil))
	dst := blobPath(s.dir, id)
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, err
	}
	if err := fsyncDir(filepath.Dir(dst)); err != nil {
		return "", 0, err
	}
	return id, n, nil
}

func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Init seeds an empty store with the first content version.
func (s *Store) Init(ctx context.Context, content io.Reader, author string) (*Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head != "" {
		return nil, &StateError{Msg: "store already initialised"}
	}
	id, size, err := s.writeBlobAtomically(ctx, content)
	if err != nil {
		return nil, err
	}
	c := &Commit{
		ID:        id,
		Parent:    "",
		AuthorID:  author,
		PatchID:   "",
		BaseID:    "",
		Size:      size,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.appendCommit(c); err != nil {
		return nil, err
	}
	return c, nil
}

// journalEntry is one durable record in the write-ahead journal.
type journalEntry struct {
	Type   string  `json:"type"` // "commit"
	Commit *Commit `json:"commit"`
}

// journalFiles returns segment files in replay order (name sort == time order).
func (s *Store) journalFiles() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, journalDir))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// replay rebuilds in-memory state from journal segments. A truncated trailing
// record is ignored (torn write); all referenced blobs are verified.
// replay rebuilds state up to the last fully committed, verifiable version.
// A torn/partial trailing journal record is ignored; a record whose blob is
// missing or fails checksum halts replay (later commits cannot be trusted),
// leaving the store at the previous good commit. Failure to verify even the
// root commit is returned as an error.
func (s *Store) replay(ctx context.Context) error {
	names, err := s.journalFiles()
	if err != nil {
		return err
	}
	var good int
	for _, name := range names {
		f, err := os.Open(filepath.Join(s.dir, journalDir, name))
		if err != nil {
			return err
		}
		br := newJournalReader(f)
		segmentGood := 0
		failed := false
		for {
			var e journalEntry
			if err := br.Decode(&e); err != nil {
				// io.EOF (clean end) or a torn trailing record both stop the
				// segment; a torn record simply is not committed.
				break
			}
			if e.Type == "commit" && e.Commit != nil {
				if err := s.verifyBlob(e.Commit.ID, e.Commit.Size); err != nil {
					// Root commit cannot be recovered to an earlier version.
					if good == 0 && segmentGood == 0 {
						_ = f.Close()
						return err
					}
					failed = true
					break
				}
				s.applyCommitToState(e.Commit)
				segmentGood++
			}
		}
		_ = f.Close()
		if failed {
			// Keep the state accumulated from earlier, intact segments.
			break
		}
		good += segmentGood
	}
	return nil
}

// journalReader wraps a JSON stream that may be truncated.
type journalReader struct {
	dec *json.Decoder
}

func newJournalReader(r io.Reader) *journalReader {
	d := json.NewDecoder(r)
	return &journalReader{dec: d}
}

func (j *journalReader) Decode(v any) error { return j.dec.Decode(v) }

// verifyBlob checks blob existence, size and checksum.
func (s *Store) verifyBlob(id string, wantSize int64) error {
	path := blobPath(s.dir, id)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &NotFoundError{What: "blob " + id}
		}
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != wantSize {
		return &ChecksumError{Want: id, Got: "size mismatch"}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != id {
		return &ChecksumError{Want: id, Got: got}
	}
	return nil
}

func (s *Store) applyCommitToState(c *Commit) {
	if _, ok := s.byID[c.ID]; !ok {
		s.byID[c.ID] = c
		s.commits = append(s.commits, c)
		if c.PatchID != "" {
			s.patchSeen[c.PatchID] = c.ID
		}
	}
	s.head = c.ID
}

// appendCommit durably appends a commit: journal record fsynced, then HEAD
// updated and fsynced. Either both are durable or neither is.
func (s *Store) appendCommit(c *Commit) error {
	if err := os.MkdirAll(filepath.Join(s.dir, journalDir), dirMode); err != nil {
		return err
	}
	names, err := s.journalFiles()
	if err != nil {
		return err
	}
	seq := len(names)
	var seg string
	var f *os.File
	for {
		seg = filepath.Join(s.dir, journalDir, fmt.Sprintf("%020d-%s-%d.log", seq, shortID(c.ID), time.Now().UnixNano()))
		f, err = os.OpenFile(seg, os.O_CREATE|os.O_WRONLY|os.O_EXCL, blobMode)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return err
		}
		seq++
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(journalEntry{Type: "commit", Commit: c}); err != nil {
		_ = f.Close()
		_ = os.Remove(seg)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(seg)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// HEAD pointer file (atomically replaced)
	headPath := filepath.Join(s.dir, headName)
	tmp, err := os.CreateTemp(filepath.Join(s.dir, tmpDir), "head-*")
	if err != nil {
		return err
	}
	tn := tmp.Name()
	if _, err := tmp.WriteString(c.ID + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tn)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tn)
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tn, headPath); err != nil {
		_ = os.Remove(tn)
		return err
	}
	if err := fsyncDir(s.dir); err != nil {
		return err
	}
	s.applyCommitToState(c)
	return nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Submit applies one collaborator patch on top of the latest version.
func (s *Store) Submit(ctx context.Context, req SubmitRequest) (*SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, &StateError{Msg: "store closed"}
	}
	if s.head == "" {
		return nil, &StateError{Msg: "store not initialised"}
	}
	patchID := req.PatchID
	if patchID == "" && req.Patch != nil {
		patchID = req.Patch.ID()
	}
	if patchID == "" {
		return nil, &MalformedPatchError{Msg: "empty patch id"}
	}
	// Idempotent de-duplication: identical patch already submitted.
	if existing, ok := s.patchSeen[patchID]; ok {
		return &SubmitResult{Commit: s.byID[existing], Duplicate: true}, nil
	}

	baseID := req.BaseID
	if baseID == "" {
		baseID = s.head
	}
	base, err := s.commitBytes(baseID)
	if err != nil {
		return nil, err
	}
	head, err := s.commitBytes(s.head)
	if err != nil {
		return nil, err
	}

	// Rebase the collaborator change by three-way merging
	// (submitter base, base+patch, current HEAD). This never needs to locate
	// the patch in HEAD and produces an order-independent result.
	newContent, err := applyPatchToBytes(ctx, base, req.Patch, s.opts.Apply)
	if err != nil {
		return nil, err
	}
	var merged bytes.Buffer
	_, mErr := Merge3(ctx, bytes.NewReader(base), bytes.NewReader(newContent), bytes.NewReader(head), &merged, s.opts.Merge)
	hadConflict := false
	var conflicts []ConflictInfo
	var finalContent []byte
	if mErr != nil {
		var ce *ConflictError
		if errors.As(mErr, &ce) {
			hadConflict = true
			conflicts = ce.Conflicts
			if !s.opts.KeepConflictsInHistory {
				return &SubmitResult{HadConflicts: true, ConflictInfo: conflicts}, nil
			}
			finalContent = merged.Bytes()
		} else {
			return nil, mErr
		}
	} else {
		finalContent = merged.Bytes()
	}

	blobID, size, err := s.writeBlobAtomically(ctx, bytes.NewReader(finalContent))
	if err != nil {
		return nil, err
	}
	c := &Commit{
		ID:        blobID,
		Parent:    s.head,
		AuthorID:  req.AuthorID,
		PatchID:   patchID,
		BaseID:    baseID,
		Size:      size,
		CreatedAt: time.Now().Unix(),
		Conflict:  hadConflict,
	}
	if err := s.appendCommit(c); err != nil {
		return nil, err
	}
	return &SubmitResult{Commit: c, HadConflicts: hadConflict, ConflictInfo: conflicts}, nil
}

// commitBytes returns one version's full content.
func (s *Store) commitBytes(id string) ([]byte, error) {
	if _, ok := s.byID[id]; !ok {
		return nil, &NotFoundError{What: "commit " + id}
	}
	f, err := os.Open(blobPath(s.dir, id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func applyPatchToBytes(ctx context.Context, base []byte, p *Patch, opts ApplyOptions) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := Apply(ctx, bytes.NewReader(base), p, &buf, opts); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// HEAD returns the current committed version.
func (s *Store) HEAD(ctx context.Context) (*Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head == "" {
		return nil, &NotFoundError{What: "HEAD"}
	}
	c := *s.byID[s.head]
	return &c, nil
}

// GetCommit returns commit metadata by ID.
func (s *Store) GetCommit(ctx context.Context, id string) (*Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok {
		return nil, &NotFoundError{What: "commit " + id}
	}
	cp := *c
	return &cp, nil
}

// Content streams one committed version.
func (s *Store) Content(ctx context.Context, id string, w io.Writer) error {
	s.mu.Lock()
	c, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		return &NotFoundError{What: "commit " + id}
	}
	if err := s.verifyBlob(c.ID, c.Size); err != nil {
		return err
	}
	f, err := os.Open(blobPath(s.dir, id))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// History returns commits from root to HEAD in order.
func (s *Store) History(ctx context.Context) ([]*Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Commit, len(s.commits))
	for i, c := range s.commits {
		cp := *c
		out[i] = &cp
	}
	return out, nil
}

// Recover replays the journal, discards torn writes and verifies all blobs.
func (s *Store) Recover(ctx context.Context) (*Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reset state and rebuild from scratch.
	s.commits = nil
	s.byID = map[string]*Commit{}
	s.patchSeen = map[string]string{}
	s.head = ""
	if err := removeTmpDirContents(s.dir); err != nil {
		return nil, err
	}
	if err := s.replay(ctx); err != nil {
		return nil, err
	}
	if s.head == "" {
		return nil, nil
	}
	c := *s.byID[s.head]
	return &c, nil
}
