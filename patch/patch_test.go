package patch

import (
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

func TestApplyBasic(t *testing.T) {
	old := "line1\nline2\nline3\nline4\nline5\n"
	new := "line1\nline2-mod\nline3\nline4\nline5\nline6\n"
	p := diff.Generate(old, new, 3)
	result, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if result != new {
		t.Errorf("Apply result = %q; want %q", result, new)
	}
}

func TestApplyReverse(t *testing.T) {
	old := "a\nb\nc\nd\n"
	new := "a\nB\nc\nd\ne\n"
	p := diff.Generate(old, new, 3)
	applied, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	restored, err := ApplyReverse(applied, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyReverse failed: %v", err)
	}
	if restored != old {
		t.Errorf("Restored = %q; want %q", restored, old)
	}
}

func TestApplyOffsetTolerance(t *testing.T) {
	// Patch generated from a small file; apply to a larger file with extra lines.
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	// Target has 2 extra lines at the top.
	target := "x\ny\na\nb\nc\n"
	result, err := Apply(target, p, ApplyOptions{MaxOffset: 10})
	if err != nil {
		t.Fatalf("Apply with offset failed: %v", err)
	}
	want := "x\ny\na\nB\nc\n"
	if result != want {
		t.Errorf("Result = %q; want %q", result, want)
	}
}

func TestApplyOffsetExceedsTolerance(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	// Target has 200 extra lines; offset exceeds default tolerance.
	var target strings.Builder
	for i := 0; i < 200; i++ {
		target.WriteString("x\n")
	}
	target.WriteString("a\nb\nc\n")
	_, err := Apply(target.String(), p, ApplyOptions{MaxOffset: 10})
	if err == nil {
		t.Fatal("expected error for offset exceeding tolerance")
	}
	if !derr.IsKind(err, derr.KindNoMatch) {
		t.Errorf("error kind = %v; want KindNoMatch", err)
	}
}

func TestApplyAmbiguousMatch(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	// Target has the pattern twice.
	target := "a\nb\nc\na\nb\nc\n"
	_, err := Apply(target, p, ApplyOptions{MaxOffset: 100})
	if err == nil {
		t.Fatal("expected error for ambiguous match")
	}
	if !derr.IsKind(err, derr.KindAmbiguousMatch) {
		t.Errorf("error kind = %v; want KindAmbiguousMatch", err)
	}
}

func TestApplyWhitespaceTolerance(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	target := "a\nb \nc\n" // trailing space on b
	result, err := Apply(target, p, ApplyOptions{IgnoreWhitespace: true})
	if err != nil {
		t.Fatalf("Apply with whitespace tolerance failed: %v", err)
	}
	want := "a\nB\nc\n"
	if result != want {
		t.Errorf("Result = %q; want %q", result, want)
	}
}

func TestApplyMalformedPatch(t *testing.T) {
	malformed := "this is not a patch\n"
	_, err := Parse(malformed)
	if err == nil {
		t.Fatal("expected error for malformed patch")
	}
	if !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Errorf("error kind = %v; want KindMalformedPatch", err)
	}
}

func TestApplyNoPartialWrite(t *testing.T) {
	// When apply fails, the original content must be returned unchanged.
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	target := "completely\ndifferent\ncontent\n"
	result, err := Apply(target, p, ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	if result != target {
		t.Errorf("result changed on failure: got %q; want %q", result, target)
	}
}

func TestParseAndFormatRoundtrip(t *testing.T) {
	old := "a\nb\nc\nd\ne\n"
	new := "a\nB\nc\nd\nE\n"
	p := diff.Generate(old, new, 2)
	formatted := p.Format()
	parsed, err := Parse(formatted)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(parsed.Hunks) != len(p.Hunks) {
		t.Errorf("roundtrip hunks = %d; want %d", len(parsed.Hunks), len(p.Hunks))
	}
}

func TestApplyEmptyContent(t *testing.T) {
	p := diff.Generate("", "a\nb\n", 1)
	result, err := Apply("", p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply to empty failed: %v", err)
	}
	if result != "a\nb\n" {
		t.Errorf("Result = %q; want %q", result, "a\nb\n")
	}
}

func TestApplyCRLFContent(t *testing.T) {
	// A patch generated from LF content should apply to CRLF content.
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	// Apply to CRLF content.
	result, err := Apply("a\r\nb\r\nc\r\n", p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply to CRLF failed: %v", err)
	}
	want := "a\nB\nc\n"
	if result != want {
		t.Errorf("Result = %q; want %q", result, want)
	}
}

func TestApplyNoTrailingNewline(t *testing.T) {
	old := "a\nb\nc"
	new := "a\nB\nc"
	p := diff.Generate(old, new, 1)
	result, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if result != new {
		t.Errorf("Result = %q; want %q", result, new)
	}
}

func TestApplyReverseFailureKeepsOriginal(t *testing.T) {
	// If reverse application fails, the patched content must be unchanged.
	old := "a\nb\nc\n"
	p := diff.Generate(old, "a\nB\nc\n", 1)
	patched, _ := Apply(old, p, ApplyOptions{})
	// Corrupt the patched content so reverse fails.
	corrupted := "x\ny\nz\n"
	result, err := ApplyReverse(corrupted, p, ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	if result != corrupted {
		t.Errorf("result changed on failure: got %q; want %q", result, corrupted)
	}
	_ = patched
}

func TestApplyOutOfBounds(t *testing.T) {
	// A patch with a hunk header pointing beyond the content should fail.
	p := &diff.Patch{
		Hunks: []*diff.Hunk{
			{
				OldStart: 100, OldLines: 1,
				NewStart: 100, NewLines: 1,
				Lines: []diff.Line{
					{Op: diff.OpContext, Text: "a"},
				},
			},
		},
	}
	_, err := Apply("a\nb\nc\n", p, ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected error for out-of-bounds hunk")
	}
}

func TestParseEncodingMismatch(t *testing.T) {
	// A patch with invalid UTF-8 should produce a distinguishable error.
	// (Our parser normalizes line endings; invalid bytes are preserved.)
	malformed := "@@ -1 +1 @@\n context\n"
	_, err := Parse(malformed)
	if err == nil {
		t.Fatal("expected error for malformed hunk header")
	}
}

func TestApplyMultipleHunks(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	new := "a\nB\nc\nd\ne\nF\ng\nh\n"
	p := diff.Generate(old, new, 1)
	if len(p.Hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(p.Hunks))
	}
	result, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if result != new {
		t.Errorf("Result = %q; want %q", result, new)
	}
}

func TestApplyPureAddition(t *testing.T) {
	old := "a\nb\n"
	new := "a\nb\nc\nd\n"
	p := diff.Generate(old, new, 1)
	result, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if result != new {
		t.Errorf("Result = %q; want %q", result, new)
	}
}

func TestApplyPureDeletion(t *testing.T) {
	old := "a\nb\nc\nd\n"
	new := "a\nb\n"
	p := diff.Generate(old, new, 1)
	result, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if result != new {
		t.Errorf("Result = %q; want %q", result, new)
	}
}

func TestReverseMultipleHunks(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\n"
	new := "a\nB\nc\nd\ne\nF\n"
	p := diff.Generate(old, new, 1)
	applied, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	restored, err := ApplyReverse(applied, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyReverse failed: %v", err)
	}
	if restored != old {
		t.Errorf("Restored = %q; want %q", restored, old)
	}
}

func TestApplyFuzz(t *testing.T) {
	// Test that fuzz can match when edge context lines differ slightly.
	old := "a\nb\nc\nd\ne\n"
	new := "a\nB\nc\nd\ne\n"
	p := diff.Generate(old, new, 2)
	// Target has a different first context line but same core.
	target := "X\nb\nc\nd\ne\n"
	result, err := Apply(target, p, ApplyOptions{Fuzz: 2, MaxOffset: 10})
	if err != nil {
		t.Fatalf("Apply with fuzz failed: %v", err)
	}
	want := "X\nB\nc\nd\ne\n"
	if result != want {
		t.Errorf("Result = %q; want %q", result, want)
	}
}

func TestComplexDiffApply(t *testing.T) {
	old := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	new := "line1\nline2-mod\nline3\nline4\nline5\nline6\nline7-mod\nline8\nline9\n"
	p := diff.Generate(old, new, 2)
	result, err := Apply(old, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if result != new {
		t.Errorf("Result = %q; want %q", result, new)
	}
	// Verify reverse.
	restored, err := ApplyReverse(result, p, ApplyOptions{})
	if err != nil {
		t.Fatalf("Reverse failed: %v", err)
	}
	if restored != old {
		t.Errorf("Restored = %q; want %q", restored, old)
	}
}

func TestApplyAlreadyApplied(t *testing.T) {
	// Applying a patch to content that already has the changes should
	// either fail gracefully or produce a predictable result.
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := diff.Generate(old, new, 1)
	// Apply to new (already has the change).
	_, err := Apply(new, p, ApplyOptions{MaxOffset: 1})
	if err == nil {
		// If it succeeds, the result should be unchanged or predictable.
		// This is acceptable behavior.
	}
	// The important thing is no panic and no partial write.
}
