package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func mustOpen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func baseline(t *testing.T, s *Store) string {
	t.Helper()
	base := "a\nb\nc\nd\ne\n"
	h, err := s.Put(base)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const baseText = "a\nb\nc\nd\ne\n"

func pAt(base, old, neu string) *diff.Patch {
	return diff.Generate(base, neu, 1)
}

func submitTip(t *testing.T, s *Store, bh string, p *diff.Patch) {
	t.Helper()
	if _, err := s.Submit(bh, p, patch.ApplyOptions{}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

func TestSubmitLateArrivalsStackOnTip(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	pB := pAt(baseText, "", "a\nB\nc\nd\ne\n")
	pD := pAt(baseText, "", "a\nb\nc\nD\ne\n")

	submitTip(t, s, bh, pB)
	submitTip(t, s, bh, pD) // arrives later, based on old baseline
	tip, err := s.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip != "a\nB\nc\nD\ne\n" {
		t.Fatalf("tip = %q", tip)
	}
}

func TestSubmitOrderIndependent(t *testing.T) {
	orders := [][]int{
		{1, 2}, {2, 1}, {1, 2, 1}, {2, 1, 2},
		{1, 1, 2, 2}, {2, 2, 1, 1}, {1, 2, 2, 1},
	}
	want := "a\nB\nc\nD\ne\n"
	for n, order := range orders {
		t.Run(fmt.Sprintf("order%d", n), func(t *testing.T) {
			dir := t.TempDir()
			s := mustOpen(t, dir)
			bh, _ := s.Put(baseText)
			ps := map[int]*diff.Patch{
				1: pAt(baseText, "", "a\nB\nc\nd\ne\n"),
				2: pAt(baseText, "", "a\nb\nc\nD\ne\n"),
			}
			for _, k := range order {
				if _, err := s.Submit(bh, ps[k], patch.ApplyOptions{}); err != nil {
					t.Fatalf("submit %d: %v", k, err)
				}
			}
			tip, _ := s.Tip()
			if tip != want {
				t.Fatalf("order %v -> %q want %q", order, tip, want)
			}
			hist, _ := s.History()
			if len(hist) != 3 {
				t.Fatalf("order %v history len = %d, want 3", order, len(hist))
			}
		})
	}
}

func TestSubmitDuplicates(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	pB := pAt(baseText, "", "a\nB\nc\nd\ne\n")
	r1, err := s.Submit(bh, pB, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Reapplied {
		t.Fatal("first submit should report reapplied")
	}
	r2, err := s.Submit(bh, pB, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Reapplied {
		t.Fatal("duplicate submit must not reapply")
	}
	if r1.Head != r2.Head {
		t.Fatal("duplicate submit moved head")
	}
	tip, _ := s.Tip()
	if tip != "a\nB\nc\nd\ne\n" {
		t.Fatalf("tip = %q", tip)
	}
	hist, _ := s.History()
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (base+1)", len(hist))
	}
}

func TestSubmitConcurrentUniqueResult(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	changes := []string{
		"A\nb\nc\nd\ne\n",
		"a\nB\nc\nd\ne\n",
		"a\nb\nC\nd\ne\n",
		"a\nb\nc\nD\ne\n",
		"a\nb\nc\nd\nE\n",
	}
	var ps []*diff.Patch
	for _, c := range changes {
		ps = append(ps, pAt(baseText, "", c))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for round := 0; round < 3; round++ {
		for _, p := range ps {
			wg.Add(1)
			p := p
			go func() {
				defer wg.Done()
				if _, err := s.Submit(bh, p, patch.ApplyOptions{}); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected submit error: %v", err)
	}
	tip, _ := s.Tip()
	if tip != "A\nB\nC\nD\nE\n" {
		t.Fatalf("tip = %q", tip)
	}
	hist, _ := s.History()
	if len(hist) != 6 {
		t.Fatalf("history = %d, want 6 (base + 5 changes)", len(hist))
	}
}

func TestSubmitConcurrentSamePatch(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	p := pAt(baseText, "", "a\nB\nc\nd\ne\n")
	var wg sync.WaitGroup
	heads := make(map[string]int)
	var mu sync.Mutex
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Submit(bh, p, patch.ApplyOptions{})
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			mu.Lock()
			heads[r.Head]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(heads) != 1 {
		t.Fatalf("multiple heads from identical submissions: %v", heads)
	}
	hist, _ := s.History()
	if len(hist) != 2 {
		t.Fatalf("history = %d, want 2", len(hist))
	}
}

func TestSubmitConflictDoesNotMoveTip(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	pX := pAt(baseText, "", "a\nX\nc\nd\ne\n")
	pY := pAt(baseText, "", "a\nY\nc\nd\ne\n")
	submitTip(t, s, bh, pX)
	_, err := s.Submit(bh, pY, patch.ApplyOptions{})
	if !derr.IsKind(err, derr.KindConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	tip, _ := s.Tip()
	if tip != "a\nX\nc\nd\ne\n" {
		t.Fatalf("tip moved after conflict: %q", tip)
	}
	// After conflict, a non-conflicting later change still stacks.
	pD := pAt(baseText, "", "a\nb\nc\nD\ne\n")
	if _, err := s.Submit(bh, pD, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	tip2, _ := s.Tip()
	if tip2 != "a\nX\nc\nD\ne\n" {
		t.Fatalf("tip2 = %q", tip2)
	}
}

func TestSubmitIdenticalChangeIsClean(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	p1 := pAt(baseText, "", "a\nB\nc\nd\ne\n")
	p2 := pAt(baseText, "", "a\nB\nc\nd\ne\n")
	submitTip(t, s, bh, p1)
	r, err := s.Submit(bh, p2, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Reapplied {
		t.Fatal("identical change should not be reapplied")
	}
}

func TestSubmitUnitializedHead(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	if h, err := s.Head(); err != nil || h != "" {
		t.Fatalf("empty head = %q, %v", h, err)
	}
	if tip, err := s.Tip(); err != nil || tip != "" {
		t.Fatalf("empty tip = %q, %v", tip, err)
	}
	bh, _ := s.Put(baseText)
	submitTip(t, s, bh, pAt(baseText, "", "a\nB\nc\nd\ne\n"))
	h, _ := s.Head()
	if h == "" {
		t.Fatal("head not initialized")
	}
}

func TestRecoverLostHEAD(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	submitTip(t, s, bh, pAt(baseText, "", "a\nB\nc\nd\ne\n"))
	tip, _ := s.Tip()

	if err := os.Remove(filepath.Join(dir, "HEAD")); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpen(t, dir)
	tip2, err := s2.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip2 != tip {
		t.Fatalf("recovered tip = %q want %q", tip2, tip)
	}
}

func TestRecoverCorruptHEAD(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	submitTip(t, s, bh, pAt(baseText, "", "a\nB\nc\nd\ne\n"))
	want := "a\nB\nc\nd\ne\n"

	os.WriteFile(filepath.Join(dir, "HEAD"), []byte("not-a-hash 1\n"), 0o644)
	s2 := mustOpen(t, dir)
	tip, err := s2.Tip()
	if err != nil {
		t.Fatalf("corrupt HEAD should recover, got %v", err)
	}
	if tip != want {
		t.Fatalf("tip = %q want %q", tip, want)
	}
}

func TestRecoverOrphanTempAndContinue(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	submitTip(t, s, bh, pAt(baseText, "", "a\nB\nc\nd\ne\n"))
	// Orphan temp from a crashed transaction.
	os.WriteFile(filepath.Join(dir, dirTmp, "blob-orphan"), []byte("partial"), 0o644)
	s2 := mustOpen(t, dir)
	entries, _ := os.ReadDir(filepath.Join(dir, dirTmp))
	if len(entries) != 0 {
		t.Fatalf("tmp not cleaned: %d entries", len(entries))
	}
	// A later change stacks on the recovered tip.
	submitTip(t, s2, bh, pAt(baseText, "", "a\nb\nc\nd\nE\n"))
	tip, _ := s2.Tip()
	if tip != "a\nB\nc\nd\nE\n" {
		t.Fatalf("tip = %q", tip)
	}
}

func TestSubmitCRLFConvergence(t *testing.T) {
	// Two collaborators on a CRLF baseline, different lines, any order.
	crlfBase := "a\r\nb\r\nc\r\n"
	ps := []*diff.Patch{
		diff.Generate(crlfBase, "a\r\nB\r\nc\r\n", 1),
		diff.Generate(crlfBase, "a\r\nb\r\nC\r\n", 1),
	}
	for _, order := range [][]int{{0, 1}, {1, 0}} {
		dir := t.TempDir()
		s := mustOpen(t, dir)
		bh, _ := s.Put(crlfBase)
		for _, k := range order {
			if _, err := s.Submit(bh, ps[k], patch.ApplyOptions{}); err != nil {
				t.Fatalf("order %v: %v", order, err)
			}
		}
		tip, _ := s.Tip()
		if tip != "a\r\nB\r\nC\r\n" {
			t.Fatalf("order %v tip = %q", order, tip)
		}
	}
}

func TestRecoverIgnoresUnpublishedOrphanCommit(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	bh, _ := s.Put(baseText)
	submitTip(t, s, bh, pAt(baseText, "", "a\nB\nc\nd\ne\n"))
	want := "a\nB\nc\nd\ne\n"

	// Forge a higher-seq commit whose parent blob is missing: this models a
	// transaction that crashed after writing the commit but whose parent link
	// is dangling (never reachable from a published HEAD).
	orphanContent := "a\nb\nc\nd\nF\n"
	orphanHash, _ := s.Put(orphanContent)
	orphanRec := fmt.Sprintf("seq=99\nparent=%s\nbase=%s\nresult=%s\npatch=x\nchange=orphan\n",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", bh, orphanHash)
	os.WriteFile(filepath.Join(dir, dirCommits, orphanHash), []byte(orphanRec), 0o644)
	os.Remove(filepath.Join(dir, "HEAD"))

	s2 := mustOpen(t, dir)
	tip, err := s2.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip != want {
		t.Fatalf("recovery selected orphan: tip=%q want %q", tip, want)
	}
}
