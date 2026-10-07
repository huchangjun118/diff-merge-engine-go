package patch

import (
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

func roundtripApply(t *testing.T, target, text string) string {
	t.Helper()
	p, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	got, err := Apply(target, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	return got
}

// Requirement 1: same CRLF patch applied in LF and CRLF environments gives
// byte-identical CRLF output matching the patch.
func TestCRLFPatchByteIdenticalAcrossEnvironments(t *testing.T) {
	old := "line1\r\nline2\r\nline3\r\n"
	new := "line1\r\nline2-X\r\nline3\r\nline4\r\n"
	text := diff.Generate(old, new, 3).Format()

	gotCRLF := roundtripApply(t, old, text)
	gotLF := roundtripApply(t, "line1\nline2\nline3\n", text)

	if gotCRLF != new {
		t.Errorf("CRLF env:\n got %q\nwant %q", gotCRLF, new)
	}
	if gotLF != new {
		t.Errorf("LF env:\n got %q\nwant %q", gotLF, new)
	}
	if gotCRLF != gotLF {
		t.Errorf("output depends on target newline style")
	}
}

func TestLFPatchEmitsLFOnCRLFTarget(t *testing.T) {
	text := diff.Generate("a\nb\nc\n", "a\nB\nc\n", 1).Format()
	got := roundtripApply(t, "a\r\nb\r\nc\r\n", text)
	if got != "a\nB\nc\n" {
		t.Errorf("got %q; want LF output", got)
	}
}

// Requirement 2: trailing-newline-only changes roundtrip and the final state
// is governed by the patch, not the target.
func TestTrailingNewlineOnlyRoundtrips(t *testing.T) {
	remove := diff.Generate("a\nb\nc\n", "a\nb\nc", 2).Format()
	if got := roundtripApply(t, "a\nb\nc\n", remove); got != "a\nb\nc" {
		t.Errorf("remove (nl target): got %q", got)
	}
	if got := roundtripApply(t, "a\nb\nc", remove); got != "a\nb\nc" {
		t.Errorf("remove (no-nl target): got %q", got)
	}

	add := diff.Generate("a\nb\nc", "a\nb\nc\n", 2).Format()
	if got := roundtripApply(t, "a\nb\nc", add); got != "a\nb\nc\n" {
		t.Errorf("add (no-nl target): got %q", got)
	}
	if got := roundtripApply(t, "a\nb\nc\n", add); got != "a\nb\nc\n" {
		t.Errorf("add (nl target): got %q", got)
	}

	crlfRemove := diff.Generate("a\r\nb\r\n", "a\r\nb", 2).Format()
	if got := roundtripApply(t, "a\r\nb\r\n", crlfRemove); got != "a\r\nb" {
		t.Errorf("crlf remove: got %q", got)
	}
}

func TestContentAndTrailingChangeRoundtrips(t *testing.T) {
	cases := []struct{ old, new string }{
		{"a\nb\nc\n", "a\nB\nc"},
		{"a\nb\nc", "a\nB\nc\n"},
		{"a\r\nb\r\nc\r\n", "a\r\nB\r\nc"},
	}
	for _, c := range cases {
		text := diff.Generate(c.old, c.new, 2).Format()
		if got := roundtripApply(t, c.old, text); got != c.new {
			t.Errorf("roundtrip %q -> %q gave %q\n%s", c.old, c.new, got, text)
		}
	}
}

func TestReversePreservesByteConventions(t *testing.T) {
	p := diff.Generate("a\r\nb\r\nc\r\n", "a\r\nB\r\nc", 2)
	applied, err := Apply("a\r\nb\r\nc\r\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if applied != "a\r\nB\r\nc" {
		t.Fatalf("applied = %q", applied)
	}
	restored, err := ApplyReverse(applied, p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if restored != "a\r\nb\r\nc\r\n" {
		t.Errorf("reverse = %q; want original CRLF+newline", restored)
	}
}

// Requirement 5: distinct failure kinds.
func TestInvalidUTF8PatchIsEncodingError(t *testing.T) {
	bad := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-" + "abc\xff\xfe" + "\n+ok\n"
	_, err := Parse(bad)
	if !derr.IsKind(err, derr.KindEncodingMismatch) {
		t.Fatalf("kind = %v; want KindEncodingMismatch", err)
	}
}

func TestInvalidUTF8TargetIsEncodingError(t *testing.T) {
	p := diff.Generate("a\n", "b\n", 1)
	_, err := Apply("a\xff\n", p, ApplyOptions{})
	if !derr.IsKind(err, derr.KindEncodingMismatch) {
		t.Fatalf("kind = %v; want KindEncodingMismatch", err)
	}
}

func TestMalformedPatchVariants(t *testing.T) {
	variants := []string{
		"",
		"not a patch at all\n",
		"--- a/x\n",                       // missing +++
		"--- a/x\n+++ b/x\nrandom line\n", // unexpected line
		"@@ -1 +1 @@\n context\n",         // header without file headers
		"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a\n", // count mismatch
		"--- a/x\n+++ b/x\n@@ -x +1 @@\n-a\n+b\n", // bad range
		"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n\\ something else\n",
		"# diff-merge-engine eol: old=bogus new=lf\n--- a/x\n+++ b/x\n",
	}
	for i, v := range variants {
		if _, err := Parse(v); !derr.IsKind(err, derr.KindMalformedPatch) {
			t.Errorf("variant %d: kind = %v; want KindMalformedPatch (%q)", i, err, v)
		}
	}
}

func TestOutOfBoundsDistinctFromNoMatch(t *testing.T) {
	// Header declares a line the target structurally cannot have.
	p := &diff.Patch{
		OldTrailingNewline: true,
		NewTrailingNewline: true,
		Hunks: []*diff.Hunk{{
			OldStart: 100, OldLines: 1,
			NewStart: 100, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}},
		}},
	}
	_, err := Apply("a\nb\nc\n", p, ApplyOptions{})
	if !derr.IsKind(err, derr.KindOutOfBounds) {
		t.Fatalf("kind = %v; want KindOutOfBounds", err)
	}
}

func TestContextMismatchIsNoMatchNotOutOfBounds(t *testing.T) {
	// Range fits, but the content at that location is wrong.
	p := diff.Generate("a\nb\nc\n", "a\nB\nc\n", 1)
	target := "x\ny\nz\n"
	_, err := Apply(target, p, ApplyOptions{MaxOffset: 1})
	if !derr.IsKind(err, derr.KindNoMatch) {
		t.Fatalf("kind = %v; want KindNoMatch", err)
	}
}

func TestAllFailureModesReturnOriginal(t *testing.T) {
	// No failure path may mutate/return partial content.
	target := "keep\nme\nintact\n"

	// Malformed.
	if _, err := Parse("\xff\xff bad"); err == nil {
		t.Fatal("expected parse error")
	}

	// OOB.
	pOOB := &diff.Patch{
		OldTrailingNewline: true, NewTrailingNewline: true,
		Hunks: []*diff.Hunk{{
			OldStart: 50, OldLines: 1, NewStart: 50, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpContext, Text: "keep"}},
		}},
	}
	if got, err := Apply(target, pOOB, ApplyOptions{}); err == nil || got != target {
		t.Fatalf("OOB: got=%q err=%v", got, err)
	}

	// No match.
	pNM := diff.Generate("a\nb\nc\n", "a\nB\nc\n", 1)
	if got, err := Apply(target, pNM, ApplyOptions{MaxOffset: 1}); err == nil || got != target {
		t.Fatalf("no-match: got=%q err=%v", got, err)
	}

	// Encoding.
	if got, err := Apply("a\xff\n", pNM, ApplyOptions{}); err == nil || got != "a\xff\n" {
		t.Fatalf("encoding: got=%q err=%v", got, err)
	}
}

func TestValidateRejectsBadHunk(t *testing.T) {
	p := &diff.Patch{Hunks: []*diff.Hunk{{
		OldStart: 1, OldLines: 2, NewStart: 1, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}},
	}}}
	if err := Validate(p); !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("kind = %v", err)
	}
}

func TestParseHandWrittenStandardPatch(t *testing.T) {
	// Standard git-style patch with timestamps, CRLF physical encoding and
	// no-newline marker. No directive => output defaults to LF, and the
	// marker dictates the trailing state.
	raw := "--- a/f.txt\t2020-01-01\n+++ b/f.txt\t2020-01-01\n@@ -1,2 +1,2 @@\n a\n-b\n+B\n\\ No newline at end of file\n"
	crlfEncoded := strings.ReplaceAll(raw, "\n", "\r\n")
	p, err := Parse(crlfEncoded)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewEOL != diff.EOLLF {
		t.Errorf("default EOL must be LF, got %v", p.NewEOL)
	}
	last := p.Hunks[len(p.Hunks)-1]
	if !last.Lines[len(last.Lines)-1].NoNewline {
		t.Error("missing parsed no-newline flag")
	}
	got, err := Apply("a\nb\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\nB" {
		t.Errorf("hand-written patch result = %q", got)
	}
}

func TestParseRejectsAnnotationWithoutBody(t *testing.T) {
	raw := "--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n\\ No newline at end of file\n"
	if _, err := Parse(raw); !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("kind=%v", err)
	}
}

func TestParseDirectiveControlsOutput(t *testing.T) {
	raw := "# diff-merge-engine eol: old=lf new=crlf trailing: old=yes new=no\n--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-a\n+A\n"
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewEOL != diff.EOLCRLF || p.NewTrailingNewline != false {
		t.Fatalf("metadata: eol=%v trailing=%v", p.NewEOL, p.NewTrailingNewline)
	}
	got, err := Apply("a\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "A" {
		t.Errorf("directive output = %q", got)
	}
}

func TestSecondHunkOutOfBoundsRejectsWholePatch(t *testing.T) {
	target := "a\nb\nc\n"
	// First hunk is valid and would change line 1; the second references
	// beyond the file. Nothing may be applied.
	p := &diff.Patch{
		OldTrailingNewline: true, NewTrailingNewline: true,
		Hunks: []*diff.Hunk{
			{
				OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
				Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}},
			},
			{
				OldStart: 99, OldLines: 1, NewStart: 99, NewLines: 1,
				Lines: []diff.Line{{Op: diff.OpContext, Text: "z"}},
			},
		},
	}
	got, err := Apply(target, p, ApplyOptions{})
	if !derr.IsKind(err, derr.KindOutOfBounds) {
		t.Fatalf("kind = %v; want KindOutOfBounds", err)
	}
	if got != target {
		t.Fatalf("partial application occurred: %q", got)
	}
}

func TestPureInsertionAtEOFAllowed(t *testing.T) {
	p := &diff.Patch{
		OldTrailingNewline: true, NewTrailingNewline: true,
		Hunks: []*diff.Hunk{{
			OldStart: 3, OldLines: 0, NewStart: 3, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpAdd, Text: "d"}},
		}},
	}
	got, err := Apply("a\nb\nc\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\nb\nc\nd\n" {
		t.Errorf("insertion at EOF got %q", got)
	}
}

func TestMultiFilePatchRejected(t *testing.T) {
	raw := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+A\n--- a/y\n+++ b/y\n@@ -1 +1 @@\n-b\n+B\n"
	if _, err := Parse(raw); !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("kind=%v", err)
	}
}
