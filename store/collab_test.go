package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func itoaTest(n int) string { return strconv.Itoa(n) }

// Requirement 3: collaborators branch from one baseline; every patch must
// land exactly once on the latest committed version and the final state must
// be independent of arrival interleaving.
func TestCollaboratorsConcurrentDuplicateReordered(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Well-separated lines so each collaborator's hunk context never
	// intersects another's edited region: these are genuinely non-overlapping
	// changes and must merge in ANY arrival order.
	var baseB strings.Builder
	for i := 1; i <= 40; i++ {
		baseB.WriteString("line" + itoaTest(i) + "\n")
	}
	base := baseB.String()
	if _, err := s.InitBase(base); err != nil {
		t.Fatal(err)
	}

	edit := func(n int, to string) *diff.Patch {
		var nb strings.Builder
		for i := 1; i <= 40; i++ {
			if i == n {
				nb.WriteString(to + "\n")
			} else {
				nb.WriteString("line" + itoaTest(i) + "\n")
			}
		}
		return diff.Generate(base, nb.String(), 3)
	}
	patches := []*diff.Patch{edit(1, "FIRST"), edit(20, "MIDDLE"), edit(40, "LAST")}
	wantFinal := func() string {
		var b strings.Builder
		for i := 1; i <= 40; i++ {
			switch i {
			case 1:
				b.WriteString("FIRST\n")
			case 20:
				b.WriteString("MIDDLE\n")
			case 40:
				b.WriteString("LAST\n")
			default:
				b.WriteString("line" + itoaTest(i) + "\n")
			}
		}
		return b.String()
	}()

	var wg sync.WaitGroup
	var resMu sync.Mutex
	results := make([][]string, len(patches))
	for i := range results {
		results[i] = nil
	}
	// Hammer: 30 rounds x 3 patches concurrently => 90 submissions, each
	// patch effectively once.
	for round := 0; round < 30; round++ {
		for i, p := range patches {
			wg.Add(1)
			go func(i int, p *diff.Patch) {
				defer wg.Done()
				h, err := s.Submit(p, patch.ApplyOptions{MaxOffset: 50})
				resMu.Lock()
				defer resMu.Unlock()
				if err != nil {
					t.Errorf("submit %d: %v", i, err)
					return
				}
				results[i] = append(results[i], h)
			}(i, p)
		}
	}
	wg.Wait()

	// Each patch always maps to its single committed result hash.
	for i, hs := range results {
		if len(hs) == 0 {
			t.Fatalf("patch %d never applied", i)
		}
		for _, h := range hs {
			if h != hs[0] {
				t.Fatalf("patch %d returned multiple hashes", i)
			}
		}
	}

	head, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.Get(head)
	if err != nil {
		t.Fatal(err)
	}
	if final != wantFinal {
		t.Fatalf("final = %q; want %q", final, wantFinal)
	}

	hist, err := s.History()
	if err != nil {
		t.Fatal(err)
	}
	// base + exactly one commit per distinct patch.
	if len(hist) != 1+len(patches) {
		t.Fatalf("history length = %d; want %d", len(hist), 1+len(patches))
	}
	if hist[len(hist)-1] != head {
		t.Errorf("history tip differs from HEAD")
	}
}

// Requirement 3: late and out-of-order arrivals both chain onto HEAD.
func TestLateAndReorderedSubmissions(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	base := "a\nb\nc\nd\ne\n"
	s.InitBase(base)

	p1 := diff.Generate(base, "a\nB\nc\nd\ne\n", 1)
	p2 := diff.Generate(base, "a\nb\nc\nd\nE\n", 1)

	h1, err := s.Submit(p1, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// p2 arrives "late" after p1; it must merge onto the p1 result.
	h2, err := s.Submit(p2, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("two distinct patches produced identical result")
	}
	got, _ := s.Get(h2)
	if got != "a\nB\nc\nd\nE\n" {
		t.Fatalf("late patch did not chain onto HEAD: %q", got)
	}

	// Re-submitting both in reverse order changes nothing.
	again2, err := s.Submit(p2, patch.ApplyOptions{})
	if err != nil || again2 != h2 {
		t.Fatalf("repeat p2 = %s,%v", again2, err)
	}
	again1, err := s.Submit(p1, patch.ApplyOptions{})
	if err != nil || again1 != h1 {
		t.Fatalf("repeat p1 = %s,%v", again1, err)
	}
	head, _ := s.Head()
	if head != h2 {
		t.Fatalf("HEAD moved on duplicate submissions: %s", head)
	}
}

func TestConflictingPatchDoesNotMoveHEAD(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	base := "a\nb\nc\n"
	head0, _ := s.InitBase(base)

	p1 := diff.Generate(base, "a\nB\nc\n", 1)
	h1, err := s.Submit(p1, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A patch based on an old version that conflicts with current content.
	conflict := diff.Generate(base, "a\nbeta\nc\n", 1)
	_, err = s.Submit(conflict, patch.ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected conflicting submission to fail")
	}
	head, _ := s.Head()
	if head != h1 {
		t.Fatalf("HEAD changed after failed submission: %s vs %s (base %s)", head, h1, head0)
	}
}

// Requirement 3: a crash between writing the commit and publishing HEAD must
// roll open back to the previous consistent version.
func TestRecoverToConsistentHEAD(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	base := "a\nb\nc\n"
	_, _ = s.InitBase(base)
	p := diff.Generate(base, "a\nB\nc\n", 1)
	h, err := s.Submit(p, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate corruption: delete the blob HEAD points at.
	if err := os.Remove(filepath.Join(dir, "blobs", h)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !derr.IsKind(err, derr.KindCorrupt) {
		t.Fatalf("reopen kind = %v; want KindCorrupt", err)
	}

	// Simulate a half-written commit: HEAD exists but points at a hash whose
	// commit object and blob are absent.
	dir2 := t.TempDir()
	s2, _ := Open(dir2)
	s2.InitBase(base)
	// Manually force HEAD to a bogus hash.
	bogus := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(filepath.Join(dir2, "HEAD"), []byte(bogus+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir2); !derr.IsKind(err, derr.KindCorrupt) {
		t.Fatalf("reopen dangling HEAD kind = %v; want KindCorrupt", err)
	}
}

func TestOrphanCommitBeforeHEADIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	base := "a\nb\nc\n"
	baseHash, _ := s.InitBase(base)

	// Simulate a crash after writing the commit but before HEAD publish:
	// commit object + result blob exist, HEAD still points at the base.
	p := diff.Generate(base, "a\nB\nc\n", 1)
	result, _ := patch.Apply(base, p, patch.ApplyOptions{})
	resultHash, _ := s.Put(result)
	patchHash := Hash(p.Format())
	commit := formatCommit(baseHash, resultHash, patchHash, 1)
	if err := os.WriteFile(filepath.Join(dir, "commits", resultHash), []byte(commit), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reopen: store must be usable and HEAD still at the base.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	head, _ := s2.Head()
	if head != baseHash {
		t.Fatalf("HEAD advanced by orphan commit: %s", head)
	}
	// The same patch can now be committed cleanly.
	h, err := s2.Submit(p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("resubmit after crash recovery failed: %v", err)
	}
	if h != resultHash {
		t.Errorf("resubmit produced different hash (content should be identical)")
	}
	head, _ = s2.Head()
	if head != resultHash {
		t.Fatalf("HEAD = %s; want %s", head, resultHash)
	}
}

func TestSubmitRejectsMalformedPatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.InitBase("a\nb\n")
	bad := &diff.Patch{Hunks: []*diff.Hunk{{
		OldStart: 1, OldLines: 9, NewStart: 1, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}},
	}}}
	if _, err := s.Submit(bad, patch.ApplyOptions{}); !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("kind = %v; want KindMalformedPatch", err)
	}
}

func TestCRLFPatchCommittedByteExact(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.InitBase("a\r\nb\r\nc\r\n")
	p := diff.Generate("a\r\nb\r\nc\r\n", "a\r\nB\r\nc\r\n", 1)
	h, err := s.Submit(p, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(h)
	if got != "a\r\nB\r\nc\r\n" {
		t.Errorf("committed blob = %q", got)
	}
}
