package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
)

// Layout file/directory names.
const (
	dirBlobs   = "blobs"
	dirCommits = "commits"
	dirChanges = "changes"
	dirLocks   = "locks"
	dirTmp     = "tmp"
	fileHEAD   = "HEAD"
	lockName   = "store.lock"
)

// headState is the durable, atomically-updated tip of the version graph.
type headState struct {
	head string // content hash of the current tip; "" when uninitialized
	seq  int    // sequence number of the current tip
}

// Branch manages the linear, deterministic commit chain on top of a store.
type Branch struct {
	s     *Store
	mu    sync.Mutex
	flock *os.File
}

// branch returns the Branch handle, taking an inter-process lock for the
// duration the handle is open. Call Close when done.
func (s *Store) branch() (*Branch, error) {
	if err := os.MkdirAll(filepath.Join(s.root, dirLocks), 0o755); err != nil {
		return nil, derr.Wrap(derr.KindCorrupt, "cannot create lock dir", err)
	}
	lockPath := filepath.Join(s.root, dirLocks, lockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, derr.Wrap(derr.KindLocked, "cannot open lock file", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, derr.Wrap(derr.KindLocked, "store is locked by another process", err)
	}
	return &Branch{s: s, flock: f}, nil
}

func (b *Branch) Close() {
	if b.flock != nil {
		unlockFile(b.flock)
		b.flock.Close()
		b.flock = nil
	}
}

// readHEAD loads the persisted tip. A missing HEAD means "uninitialized".
func (b *Branch) readHEAD() (headState, error) {
	data, err := os.ReadFile(filepath.Join(b.s.root, fileHEAD))
	if os.IsNotExist(err) {
		return headState{}, nil
	}
	if err != nil {
		return headState{}, derr.Wrap(derr.KindCorrupt, "cannot read HEAD", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	var st headState
	if len(fields) == 0 {
		return st, nil
	}
	st.head = fields[0]
	if len(fields) >= 2 {
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return headState{}, derr.Wrap(derr.KindCorrupt, "corrupt HEAD sequence", err)
		}
		st.seq = n
	}
	if !b.s.Has(st.head) {
		return headState{}, derr.New(derr.KindCorrupt, "HEAD points at missing blob: "+st.head)
	}
	return st, nil
}

// writeHEAD persists the tip atomically.
func (b *Branch) writeHEAD(st headState) error {
	return b.s.atomicWrite(filepath.Join(b.s.root, fileHEAD),
		fmt.Sprintf("%s %d\n", st.head, st.seq))
}

// commitRecord is the parsed content of a commits/<result-hash> file.
type commitRecord struct {
	seq      int
	parent   string
	base     string
	result   string
	patch    string
	changeID string
}

func (b *Branch) writeCommit(rec commitRecord) error {
	content := fmt.Sprintf(
		"seq=%d\nparent=%s\nbase=%s\nresult=%s\npatch=%s\nchange=%s\n",
		rec.seq, rec.parent, rec.base, rec.result, rec.patch, rec.changeID)
	path := filepath.Join(b.s.root, dirCommits, rec.result)
	return b.s.atomicWrite(path, content)
}

func parseCommit(content, result string) (commitRecord, error) {
	var rec commitRecord
	rec.result = result
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			return rec, derr.New(derr.KindCorrupt, "bad commit record line: "+line)
		}
		switch kv[0] {
		case "seq":
			n, err := strconv.Atoi(kv[1])
			if err != nil {
				return rec, derr.Wrap(derr.KindCorrupt, "bad seq", err)
			}
			rec.seq = n
		case "parent":
			rec.parent = kv[1]
		case "base":
			rec.base = kv[1]
		case "result":
			rec.result = kv[1]
		case "patch":
			rec.patch = kv[1]
		case "change":
			rec.changeID = kv[1]
		}
	}
	if rec.result != result {
		return rec, derr.New(derr.KindCorrupt, "commit record result mismatch")
	}
	return rec, nil
}

// markChangeIntegrated records that a change ID has been integrated, mapping
// it to the tip hash produced.
func (b *Branch) markChangeIntegrated(changeID, tipHash string) error {
	path := filepath.Join(b.s.root, dirChanges, changeID)
	return b.s.atomicWrite(path, tipHash+"\n")
}

func (b *Branch) changeIntegrated(changeID string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(b.s.root, dirChanges, changeID))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// recoverHEAD rebuilds HEAD from the commit chain after a crash or a lost
// pointer. It only considers published chain commits: a commit whose parent
// blob is missing cannot have been reachable from a committed HEAD, so a
// higher-seq orphan left by a crash just before the HEAD update is ignored.
// Recovery therefore always lands on a fully committed, previously published
// version, never on a half-finished transaction.
func (b *Branch) recoverHEAD() error {
	dir := filepath.Join(b.s.root, dirCommits)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return derr.Wrap(derr.KindCorrupt, "cannot read commits", err)
	}
	recs := map[string]commitRecord{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), "apply-") || strings.HasPrefix(e.Name(), "idem-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // torn commit file
		}
		rec, err := parseCommit(string(data), e.Name())
		if err != nil {
			continue // corrupt record
		}
		if !b.s.Has(rec.result) {
			continue // result blob missing; the commit never completed
		}
		recs[rec.result] = rec
	}

	// A chain commit is published only if every parent link resolves to
	// either another chain commit or an existing baseline blob.
	published := func(rec commitRecord) bool {
		seen := map[string]bool{}
		for {
			if seen[rec.result] {
				return false
			}
			seen[rec.result] = true
			parent, isCommit := recs[rec.parent]
			if !isCommit {
				return rec.parent != "" && b.s.Has(rec.parent)
			}
			rec = parent
		}
	}

	var best string
	for _, rec := range recs {
		if !published(rec) {
			continue
		}
		if best == "" || rec.seq > recs[best].seq ||
			(rec.seq == recs[best].seq && rec.result < best) {
			best = rec.result
		}
	}
	if best == "" {
		return nil // no published version; leave HEAD absent
	}
	r := recs[best]
	return b.writeHEAD(headState{head: best, seq: r.seq})
}
