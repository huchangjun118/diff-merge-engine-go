package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestSubmitBlockedByExternalLock(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)

	// Simulate another process holding the flock.
	lockPath := filepath.Join(dir, dirLocks, lockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}()

	p := diff.Generate(baseText, "a\nB\nc\nd\ne\n", 1)
	if _, err := s.Submit(bh, p, patch.ApplyOptions{}); err == nil {
		t.Fatal("expected locked error")
	}
}
