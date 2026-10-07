package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestPutGet(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	content := "hello\nworld\n"
	hash, err := s.Put(content)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got != content {
		t.Errorf("Get = %q; want %q", got, content)
	}
}

func TestApplyPatchIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\n"
	baseHash, _ := s.Put(base)
	p := diff.Generate(base, "a\nB\nc\n", 1)

	// Apply twice.
	h1, err := s.ApplyPatch(baseHash, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("First ApplyPatch failed: %v", err)
	}
	h2, err := s.ApplyPatch(baseHash, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("Second ApplyPatch failed: %v", err)
	}
	if h1 != h2 {
		t.Errorf("idempotent apply gave different hashes: %s vs %s", h1, h2)
	}
}

func TestApplyPatchResult(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\n"
	baseHash, _ := s.Put(base)
	p := diff.Generate(base, "a\nB\nc\n", 1)
	resultHash, err := s.ApplyPatch(baseHash, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyPatch failed: %v", err)
	}
	result, err := s.Get(resultHash)
	if err != nil {
		t.Fatalf("Get result failed: %v", err)
	}
	if result != "a\nB\nc\n" {
		t.Errorf("Result = %q; want %q", result, "a\nB\nc\n")
	}
}

func TestConcurrentApply(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\nd\ne\n"
	baseHash, _ := s.Put(base)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine applies a patch that changes a different line.
			old := base
			// Just apply the same patch concurrently to test idempotency.
			p := diff.Generate(old, "a\nb\nc\nd\nE\n", 1)
			_, err := s.ApplyPatch(baseHash, p, patch.ApplyOptions{})
			if err != nil {
				t.Errorf("concurrent ApplyPatch %d failed: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestRecover(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	// Create an orphan temp file.
	tmpDir := filepath.Join(dir, "tmp")
	os.WriteFile(filepath.Join(tmpDir, "orphan"), []byte("data"), 0o644)
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover failed: %v", err)
	}
	entries, _ := os.ReadDir(tmpDir)
	if len(entries) != 0 {
		t.Errorf("tmp dir not cleaned: %d entries", len(entries))
	}
}

func TestHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\n"
	baseHash, err := s.InitBase(base)
	if err != nil {
		t.Fatalf("InitBase failed: %v", err)
	}
	p := diff.Generate(base, "a\nB\nc\n", 1)
	if _, err = s.Submit(p, patch.ApplyOptions{}); err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	history, err := s.History()
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("History = %v; want base+1 entry (%d)", history, len(history))
	}
	if history[0] != baseHash {
		t.Errorf("history[0] = %v; want base %v", history[0], baseHash)
	}
}

func TestConcurrentCollaborators(t *testing.T) {
	// Multiple collaborators submit patches based on the same baseline.
	// The store must handle them deterministically (idempotent for same patch).
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\nd\ne\n"
	baseHash, _ := s.Put(base)

	// Collaborator 1 changes line 2.
	p1 := diff.Generate(base, "a\nB\nc\nd\ne\n", 1)
	// Collaborator 2 changes line 4.
	p2 := diff.Generate(base, "a\nb\nc\nD\ne\n", 1)

	var wg sync.WaitGroup
	results := make([]string, 20)
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			h, err := s.ApplyPatch(baseHash, p1, patch.ApplyOptions{})
			if err != nil {
				t.Errorf("p1 apply failed: %v", err)
				return
			}
			results[i] = h
		}(i)
		go func(i int) {
			defer wg.Done()
			h, err := s.ApplyPatch(baseHash, p2, patch.ApplyOptions{})
			if err != nil {
				t.Errorf("p2 apply failed: %v", err)
				return
			}
			results[10+i] = h
		}(i)
	}
	wg.Wait()

	// All p1 applications should give the same result.
	for i := 1; i < 10; i++ {
		if results[i] != results[0] {
			t.Errorf("p1 results differ: %s vs %s", results[i], results[0])
		}
	}
	// All p2 applications should give the same result.
	for i := 11; i < 20; i++ {
		if results[i] != results[10] {
			t.Errorf("p2 results differ: %s vs %s", results[i], results[10])
		}
	}
}

func TestRecoveryAfterCrash(t *testing.T) {
	// Simulate a crash by leaving an orphan temp file, then recover.
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	// Put some content.
	base := "a\nb\nc\n"
	baseHash, _ := s.Put(base)
	// Create an orphan temp file (simulating a crash mid-write).
	tmpDir := filepath.Join(dir, "tmp")
	os.WriteFile(filepath.Join(tmpDir, "orphan-blob"), []byte("partial"), 0o644)
	// Recover.
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover failed: %v", err)
	}
	// The store should still be usable.
	got, err := s.Get(baseHash)
	if err != nil {
		t.Fatalf("Get after recovery failed: %v", err)
	}
	if got != base {
		t.Errorf("Get = %q; want %q", got, base)
	}
	// Temp dir should be clean.
	entries, _ := os.ReadDir(tmpDir)
	if len(entries) != 0 {
		t.Errorf("tmp dir not cleaned: %d entries", len(entries))
	}
}

func TestOpenRecoversOrphanTemp(t *testing.T) {
	// Opening a store with orphan temp files should recover automatically.
	dir := t.TempDir()
	// Create an orphan temp file before opening.
	tmpDir := filepath.Join(dir, "tmp")
	os.MkdirAll(tmpDir, 0o755)
	os.WriteFile(filepath.Join(tmpDir, "orphan"), []byte("data"), 0o644)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	entries, _ := os.ReadDir(tmpDir)
	if len(entries) != 0 {
		t.Errorf("tmp dir not cleaned on open: %d entries", len(entries))
	}
	_ = s
}

func TestOutOfOrderPatches(t *testing.T) {
	// Patches arriving out of order should still produce correct results
	// when applied to the correct base.
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\nd\ne\n"
	baseHash, _ := s.Put(base)

	// Patch 1: change line 2 (b -> B).
	p1 := diff.Generate(base, "a\nB\nc\nd\ne\n", 1)
	// Patch 2: also change line 2 (b -> beta), conflicting with patch 1.
	p2 := diff.Generate(base, "a\nbeta\nc\nd\ne\n", 1)

	// Apply patch 1 to the base.
	h1, err := s.ApplyPatch(baseHash, p1, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyPatch 1 failed: %v", err)
	}
	// Apply patch 2 to the base (not to h1). This should succeed because
	// patch 2 is based on the base, not on h1.
	h2, err := s.ApplyPatch(baseHash, p2, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyPatch 2 to base failed: %v", err)
	}
	// The results should differ (different changes to line 2).
	r1, _ := s.Get(h1)
	r2, _ := s.Get(h2)
	if r1 == r2 {
		t.Error("patches with different changes produced same result")
	}
	// Both should be retrievable.
	if r1 != "a\nB\nc\nd\ne\n" {
		t.Errorf("r1 = %q", r1)
	}
	if r2 != "a\nbeta\nc\nd\ne\n" {
		t.Errorf("r2 = %q", r2)
	}
}

func TestCorruptedBlobRecovery(t *testing.T) {
	// A corrupted blob should be detectable.
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	base := "a\nb\nc\n"
	baseHash, _ := s.Put(base)
	// Corrupt the blob by writing garbage.
	blobPath := filepath.Join(dir, "blobs", baseHash)
	os.WriteFile(blobPath, []byte("corrupted"), 0o644)
	_, err = s.Get(baseHash)
	if err == nil {
		t.Fatal("expected error for corrupted blob")
	}
}
