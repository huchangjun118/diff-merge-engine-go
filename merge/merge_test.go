package merge

import (
	"testing"
)

func TestMergeClean(t *testing.T) {
	base := "a\nb\nc\nd\n"
	ours := "a\nb\nc\nd\ne\n"
	theirs := "a\nB\nc\nd\n"
	result := Merge(base, ours, theirs)
	if !result.Clean {
		t.Fatalf("expected clean merge, got conflicts: %v", result.Conflicts)
	}
	want := "a\nB\nc\nd\ne\n"
	if result.String() != want {
		t.Errorf("Result = %q; want %q", result.String(), want)
	}
}

func TestMergeConflict(t *testing.T) {
	base := "a\nb\nc\nd\n"
	ours := "a\nX\nc\nd\n"
	theirs := "a\nY\nc\nd\n"
	result := Merge(base, ours, theirs)
	if result.Clean {
		t.Fatal("expected conflict")
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(result.Conflicts))
	}
	c := result.Conflicts[0]
	if c.StartLine != 2 {
		t.Errorf("StartLine = %d; want 2", c.StartLine)
	}
	if len(c.Ours) != 1 || c.Ours[0] != "X" {
		t.Errorf("Ours = %v; want [X]", c.Ours)
	}
	if len(c.Theirs) != 1 || c.Theirs[0] != "Y" {
		t.Errorf("Theirs = %v; want [Y]", c.Theirs)
	}
}

func TestMergeSameChange(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nB\nc\n"
	theirs := "a\nB\nc\n"
	result := Merge(base, ours, theirs)
	if !result.Clean {
		t.Fatalf("expected clean merge for same change, got conflicts: %v", result.Conflicts)
	}
}

func TestMergeOrderIndependent(t *testing.T) {
	base := "a\nb\nc\nd\n"
	ours := "a\nX\nc\nd\n"
	theirs := "a\nY\nc\nd\n"
	r1 := Merge(base, ours, theirs)
	r2 := Merge(base, theirs, ours)
	if r1.Clean != r2.Clean {
		t.Error("clean flag differs with order")
	}
	if len(r1.Conflicts) != len(r2.Conflicts) {
		t.Errorf("conflict count differs: %d vs %d", len(r1.Conflicts), len(r2.Conflicts))
	}
	for i := range r1.Conflicts {
		if r1.Conflicts[i].StartLine != r2.Conflicts[i].StartLine {
			t.Errorf("conflict %d start line differs", i)
		}
	}
}

func TestMergeNoChanges(t *testing.T) {
	base := "a\nb\nc\n"
	result := Merge(base, base, base)
	if !result.Clean {
		t.Fatal("expected clean merge for no changes")
	}
	if result.String() != base {
		t.Errorf("Result = %q; want %q", result.String(), base)
	}
}

func TestMergeAdjacentChanges(t *testing.T) {
	// Ours changes line 1, theirs changes line 3 (adjacent but non-overlapping).
	base := "a\nb\nc\nd\n"
	ours := "A\nb\nc\nd\n"
	theirs := "a\nb\nC\nd\n"
	result := Merge(base, ours, theirs)
	if !result.Clean {
		t.Fatalf("expected clean merge for adjacent changes, got conflicts: %v", result.Conflicts)
	}
	want := "A\nb\nC\nd\n"
	if result.String() != want {
		t.Errorf("Result = %q; want %q", result.String(), want)
	}
}

func TestConflictMarkersStructured(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nX\nc\n"
	theirs := "a\nY\nc\n"
	result := Merge(base, ours, theirs)
	s := result.String()
	if !contains(s, "<<<<<<< ours") {
		t.Error("missing ours marker")
	}
	if !contains(s, "=======") {
		t.Error("missing separator")
	}
	if !contains(s, ">>>>>>> theirs") {
		t.Error("missing theirs marker")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestMergeCRLFPreserved(t *testing.T) {
	// Both sides keep CRLF; merged output must be CRLF byte-exact.
	base := "a\r\nb\r\nc\r\n"
	ours := "a\r\nB\r\nc\r\n"
	theirs := "a\r\nb\r\nC\r\n"
	r := Merge(base, ours, theirs)
	if !r.Clean {
		t.Fatalf("expected clean merge, conflicts=%v", r.Conflicts)
	}
	want := "a\r\nB\r\nC\r\n"
	if r.String() != want {
		t.Errorf("merge = %q; want %q", r.String(), want)
	}
}

func TestMergeTrailingNewlinePreserved(t *testing.T) {
	base := "a\nb\nc"
	ours := "a\nB\nc"
	theirs := "a\nb\nC"
	r := Merge(base, ours, theirs)
	got := r.String()
	if got != "a\nB\nC" {
		t.Errorf("merge without trailing newline = %q", got)
	}
}

func TestMergeOneSideAddsTrailingNewline(t *testing.T) {
	base := "a\nb\nc"
	ours := "a\nB\nc"
	theirs := "a\nb\nc\n"
	r := Merge(base, ours, theirs)
	got := r.String()
	// Side changed different regions; theirs supplied the tail newline.
	if !r.Clean {
		t.Fatalf("expected clean, conflicts=%v", r.Conflicts)
	}
	if got != "a\nB\nc\n" {
		t.Errorf("merge = %q; want newline adopted", got)
	}
}
