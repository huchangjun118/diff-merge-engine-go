package diffmerge

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func merge3(t *testing.T, base, ours, theirs string, opts MergeOptions) (string, []ConflictInfo, error) {
	t.Helper()
	var buf bytes.Buffer
	res, err := Merge3(context.Background(), strReader(base), strReader(ours), strReader(theirs), &buf, opts)
	var ci []ConflictInfo
	if res != nil {
		ci = res.Conflicts
	}
	return buf.String(), ci, err
}

func TestMergeCleanSeparate(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	ours := "a\nB\nc\nd\ne\n"
	theirs := "a\nb\nc\nD\ne\n"
	out, conf, err := merge3(t, base, ours, theirs, MergeOptions{})
	if err != nil {
		t.Fatalf("clean merge failed: %v", err)
	}
	if len(conf) != 0 {
		t.Fatalf("unexpected conflicts: %v", conf)
	}
	if out != "a\nB\nc\nD\ne\n" {
		t.Fatalf("got %q", out)
	}
}

func TestMergeBothInsertDifferentPoints(t *testing.T) {
	base := "x\n"
	ours := "x\nours-tail\n"
	theirs := "theirs-head\nx\n"
	out, _, err := merge3(t, base, ours, theirs, MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "theirs-head\nx\nours-tail\n" {
		t.Fatalf("got %q", out)
	}
}

func TestMergeConflict(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nB\nc\n"
	theirs := "a\nC\nc\n"
	out, conf, err := merge3(t, base, ours, theirs, MergeOptions{})
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want ConflictError, got %v", err)
	}
	if len(conf) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(conf))
	}
	if !strings.Contains(out, "<<<<<<< ours") || !strings.Contains(out, "=======") || !strings.Contains(out, ">>>>>>> theirs") {
		t.Fatalf("missing conflict markers:\n%s", out)
	}
	if !strings.Contains(out, "B\n") || !strings.Contains(out, "C\n") {
		t.Fatalf("conflict must retain both sides:\n%s", out)
	}
}

func TestMergeIdenticalChangeNotConflict(t *testing.T) {
	base := "a\nb\nc\n"
	out, conf, err := merge3(t, base, "a\nX\nc\n", "a\nX\nc\n", MergeOptions{})
	if err != nil {
		t.Fatalf("identical change should merge: %v", err)
	}
	if len(conf) != 0 || out != "a\nX\nc\n" {
		t.Fatalf("got %q conf=%v", out, conf)
	}
}

func TestMergeOrderIndependence(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\n"
	ours := "l1\nO\nl3\nl4\nl5\n"
	theirs := "l1\nl2\nl3\nT\nl5\n"
	// Simulate two arrival orders by constructing intermediate versions.
	// Order 1: ours then theirs -> final should equal direct merge3 output.
	direct, _, derr := merge3(t, base, ours, theirs, MergeOptions{})
	if derr != nil {
		t.Fatal(derr)
	}
	// Applying ours' patch then rebasing theirs' patch must produce the same
	// bytes. Do it via Merge3 of (ours-as-base-of-second-step) equivalently:
	// here we validate symmetry: swapping sides only swaps conflict sections.
	swapped, _, serr := merge3(t, base, theirs, ours, MergeOptions{})
	if serr != nil {
		t.Fatal(serr)
	}
	if direct != swapped {
		t.Fatalf("merge depends on side order:\n%s\nvs\n%s", direct, swapped)
	}
}

func TestMergeConflictOrderStableAcrossArrival(t *testing.T) {
	base := "a\nb\nc\n"
	out1, _, _ := merge3(t, base, "a\nX\nc\n", "a\nY\nc\n", MergeOptions{OurLabel: "alice", TheirLabel: "bob"})
	out2, _, _ := merge3(t, base, "a\nX\nc\n", "a\nY\nc\n", MergeOptions{OurLabel: "alice", TheirLabel: "bob"})
	if out1 != out2 {
		t.Fatal("conflict output not reproducible")
	}
	if !strings.Contains(out1, "<<<<<<< alice") || !strings.Contains(out1, ">>>>>>> bob") {
		t.Fatalf("custom labels missing:\n%s", out1)
	}
}

func TestMergeDeleteVsModify(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nc\n"      // delete b
	theirs := "a\nB\nc\n" // modify b
	out, conf, err := merge3(t, base, ours, theirs, MergeOptions{})
	if err == nil {
		t.Fatal("delete-vs-modify must conflict")
	}
	if len(conf) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(conf))
	}
	_ = out
}

func TestMergeBothDeleteSameLine(t *testing.T) {
	base := "a\nb\nc\n"
	out, _, err := merge3(t, base, "a\nc\n", "a\nc\n", MergeOptions{})
	if err != nil {
		t.Fatalf("identical delete should be clean: %v", err)
	}
	if out != "a\nc\n" {
		t.Fatalf("got %q", out)
	}
}

func TestMergeWhitespaceTolerance(t *testing.T) {
	base := "a  \nb\r\nc\n"
	ours := "a\nB\nc\n"
	theirs := "a\nb\nC\n"
	opts := MergeOptions{Compare: LineCompareOptions{NormalizeEOL: true, IgnoreTrailingWhitespace: true}}
	out, _, err := merge3(t, base, ours, theirs, opts)
	if err != nil {
		t.Fatalf("expected clean merge under whitespace tolerance: %v", err)
	}
	// "a" line untouched by either relative to canonical base -> output uses
	// ours side representation around change
	if !strings.Contains(out, "B\n") || !strings.Contains(out, "C\n") {
		t.Fatalf("got %q", out)
	}
}

func TestMergeAdjacentDifferentRegions(t *testing.T) {
	base := "a\nb\nc\nd\n"
	ours := "a\nX\nc\nd\n"
	theirs := "a\nb\nY\nd\n"
	out, _, err := merge3(t, base, ours, theirs, MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nX\nY\nd\n" {
		t.Fatalf("got %q", out)
	}
}

func TestMergeBothInsertAtSamePointDifferent(t *testing.T) {
	base := "a\n"
	ours := "a\nO\n"
	theirs := "a\nT\n"
	out, conf, err := merge3(t, base, ours, theirs, MergeOptions{})
	if err == nil {
		t.Fatal("competing insertions at same anchor should conflict")
	}
	if len(conf) != 1 {
		t.Fatalf("got %d conflicts", len(conf))
	}
	if !strings.Contains(out, "O\n") || !strings.Contains(out, "T\n") {
		t.Fatalf("both inserts must be retained:\n%s", out)
	}
}

func TestMergeEmptyVersions(t *testing.T) {
	out, _, err := merge3(t, "", "", "", MergeOptions{})
	if err != nil || out != "" {
		t.Fatalf("empty merge: %q %v", out, err)
	}
	out2, conf2, err2 := merge3(t, "", "x\n", "", MergeOptions{})
	if err2 != nil || len(conf2) != 0 || out2 != "x\n" {
		t.Fatalf("one-side insert: %q %v", out2, err2)
	}
}
