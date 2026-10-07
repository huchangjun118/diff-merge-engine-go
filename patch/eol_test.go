package patch

import (
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

func TestCRLFByteIdenticalOnLFTarget(t *testing.T) {
	old := "line1\r\nline2\r\nline3\r\n"
	new := "line1\r\nline2-mod\r\nline3\r\n"
	p := diff.Generate(old, new, 3)
	pp, err := Parse(p.Format())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Apply("line1\nline2\nline3\n", pp, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != new {
		t.Fatalf("LF-target result = %q want %q", got, new)
	}
}

func TestCRLFByteIdenticalAcrossEnvironments(t *testing.T) {
	old := "h1\r\nh2\r\nh3\r\nh4\r\n"
	new := "h1\r\nh2X\r\nh3\r\nh4Y\r\n"
	pp, err := Parse(diff.Generate(old, new, 1).Format())
	if err != nil {
		t.Fatal(err)
	}
	fromLF, err := Apply(strings.ReplaceAll(old, "\r\n", "\n"), pp, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fromCRLF, err := Apply(old, pp, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fromMixed, err := Apply("h1\nh2\r\nh3\r\nh4\n", pp, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fromLF != new || fromCRLF != new || fromMixed != new {
		t.Fatalf("results differ:\nLF:   %q\nCRLF: %q\nmix:  %q\nwant: %q",
			fromLF, fromCRLF, fromMixed, new)
	}
}

func TestCRLFReverseRoundTrip(t *testing.T) {
	old := "a\r\nb\r\n"
	new := "a\r\nB\r\nC\r\n"
	p := diff.Generate(old, new, 1)
	got, err := Apply("a\nb\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	back, err := ApplyReverse(got, p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if back != old {
		t.Fatalf("reverse = %q want %q", back, old)
	}
}

func TestLFPatchOnCRLFTargetStaysLF(t *testing.T) {
	// An LF patch must force LF output even when the target is CRLF.
	p, err := Parse(diff.Generate("a\nb\nc\n", "a\nB\nc\n", 1).Format())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Apply("a\r\nb\r\nc\r\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\nB\nc\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyEOFNewlineRemoval(t *testing.T) {
	p := diff.Generate("a\nb\nc\n", "a\nb\nc", 1)
	// Target with newline, at an offset.
	got, err := Apply("z\na\nb\nc\n", p, ApplyOptions{MaxOffset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got != "z\na\nb\nc" {
		t.Fatalf("got %q", got)
	}
	// Target already without newline must yield the same result.
	got2, err := Apply("a\nb\nc", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "a\nb\nc" {
		t.Fatalf("got2 %q", got2)
	}
}

func TestApplyEOFNewlineAddition(t *testing.T) {
	p := diff.Generate("a\nb\nc", "a\nb\nc\n", 1)
	got, err := Apply("a\nb\nc", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\nb\nc\n" {
		t.Fatalf("got %q", got)
	}
	// Target already ending with newline: result unchanged shape.
	got2, err := Apply("z\na\nb\nc\n", p, ApplyOptions{MaxOffset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "z\na\nb\nc\n" {
		t.Fatalf("got2 %q", got2)
	}
	back, err := ApplyReverse(got, p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if back != "a\nb\nc" {
		t.Fatalf("back %q", back)
	}
}

func TestApplyEOFChangeWithCRLF(t *testing.T) {
	p := diff.Generate("a\r\nb\r\n", "a\r\nb", 1)
	got, err := Apply("a\nb\n", p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\r\nb" {
		t.Fatalf("got %q", got)
	}
}

func TestParseEOLDirectives(t *testing.T) {
	text := "--- x\n+++ y\n# eol-old: crlf\n# eol-new: crlf\n@@ -1 +1 @@\n-a\n+A\n"
	p, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if p.OldEOL != diff.EOLCRLF || p.NewEOL != diff.EOLCRLF {
		t.Fatalf("eol = %v,%v", p.OldEOL, p.NewEOL)
	}
	// CRLF-separated patch text parses identically.
	crlfText := strings.ReplaceAll(text, "\n", "\r\n")
	p2, err := Parse(crlfText)
	if err != nil {
		t.Fatal(err)
	}
	if p2.OldEOL != diff.EOLCRLF || len(p2.Hunks) != 1 {
		t.Fatalf("crlf patch parse wrong: %+v", p2)
	}
}

func TestParseBadEOLDirective(t *testing.T) {
	text := "--- x\n+++ y\n# eol-new: cr\r\n@@ -1 +1 @@\n-a\n+A\n"
	_, err := Parse(text)
	if !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("want malformed, got %v", err)
	}
}

func TestParseUnknownDirective(t *testing.T) {
	text := "--- x\n+++ y\n# bogus\n@@ -1 +1 @@\n-a\n+A\n"
	_, err := Parse(text)
	if !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("want malformed, got %v", err)
	}
}

func TestParseNoNewlineMarkerRoundtrip(t *testing.T) {
	orig := diff.Generate("a\nb\nc\n", "a\nb\nc", 1)
	pp, err := Parse(orig.Format())
	if err != nil {
		t.Fatal(err)
	}
	if !pp.NewNoFinalNL || pp.OldNoFinalNL {
		t.Fatalf("flags old=%v new=%v", pp.OldNoFinalNL, pp.NewNoFinalNL)
	}
}

func TestInvalidUTF8Kinds(t *testing.T) {
	if _, err := Parse("--- a\n+++ b\n@@ -1 +1 @@\n\xff\n"); !derr.IsKind(err, derr.KindEncodingMismatch) {
		t.Fatalf("patch utf8: %v", err)
	}
	p, _ := Parse(diff.Generate("a\n", "b\n", 1).Format())
	if _, err := Apply("a\n\xff\n", p, ApplyOptions{}); !derr.IsKind(err, derr.KindEncodingMismatch) {
		t.Fatalf("target utf8: %v", err)
	}
}

func TestMalformedInputs(t *testing.T) {
	bad := []string{
		"",
		"random text\n",
		"--- a\n",
		"--- a\n+++ b\ngarbage line\n",
		"--- a\n+++ b\n@@ -1 +1 @@\n?bad\n",
		"--- a\n+++ b\n@@ -1 +1 @@\n-a\n+A\n--- c\n+++ d\n",
		"--- a\n+++ b\n@@ -1,2 +1,2 @@\n-a\n+A\n", // header promises 2 old/new lines, body has 1 each
	}
	for i, b := range bad {
		if _, err := Parse(b); !derr.IsKind(err, derr.KindMalformedPatch) {
			t.Errorf("case %d: want malformed for %q, got %v", i, b, err)
		}
	}
}

func TestApplyNilPatch(t *testing.T) {
	if _, err := Apply("a\n", nil, ApplyOptions{}); !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("got %v", err)
	}
}

func TestApplyFailureNoPartial(t *testing.T) {
	// Two hunks: the second is out of bounds. Content must come back whole.
	p := &diff.Patch{Hunks: []*diff.Hunk{
		{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}}},
		{OldStart: 50, OldLines: 2, NewStart: 50, NewLines: 2,
			Lines: []diff.Line{
				{Op: diff.OpContext, Text: "y"},
				{Op: diff.OpContext, Text: "z"},
			}},
	}}
	target := "a\nb\nc\n"
	got, err := Apply(target, p, ApplyOptions{MaxOffset: 5})
	if err == nil {
		t.Fatal("want error")
	}
	if got != target {
		t.Fatalf("content changed on failure: %q", got)
	}
	if !derr.IsKind(err, derr.KindOutOfBounds) {
		t.Fatalf("want oob, got %v", err)
	}
}

func TestApplyErrorKindsDistinct(t *testing.T) {
	target := "a\nb\nc\n"
	// Out of bounds.
	oob := &diff.Patch{Hunks: []*diff.Hunk{{
		OldStart: 100, OldLines: 1, NewStart: 100, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}}}}}
	_, err := Apply(target, oob, ApplyOptions{MaxOffset: 1})
	if !derr.IsKind(err, derr.KindOutOfBounds) {
		t.Fatalf("oob kind = %v", err)
	}
	// Context mismatch (range fits, text does not).
	nm := &diff.Patch{Hunks: []*diff.Hunk{{
		OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "zzz"}}}}}
	_, err = Apply(target, nm, ApplyOptions{MaxOffset: 1})
	if !derr.IsKind(err, derr.KindNoMatch) {
		t.Fatalf("nomatch kind = %v", err)
	}
}
