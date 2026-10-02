package diffmerge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func applyPatch(t *testing.T, target string, p *Patch, opts ApplyOptions) (string, *ApplyResult) {
	t.Helper()
	var buf bytes.Buffer
	res, err := Apply(context.Background(), strReader(target), p, &buf, opts)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return buf.String(), res
}

func TestApplyBasic(t *testing.T) {
	oldS := "a\nb\nc\nd\ne\n"
	newS := "a\nB\nc\nd\ne\n"
	p := genPatch(t, oldS, newS, PatchOptions{})
	got, res := applyPatch(t, oldS, p, ApplyOptions{})
	if got != newS {
		t.Fatalf("got %q want %q", got, newS)
	}
	if len(res.Hunks) != 1 || res.Hunks[0].Offset != 0 {
		t.Fatalf("unexpected result: %+v", res.Hunks)
	}
}

func TestApplyOffset(t *testing.T) {
	// hunk built against a 3-line prefix; target has a 10-line prefix instead
	p := genPatch(t, "a\nb\nc\nX\nY\nz\n", "a\nb\nc\nX\nQQ\nz\n", PatchOptions{Context: 1})
	target := strings.Repeat("lead\n", 10) + "X\nY\nz\n"
	var buf bytes.Buffer
	res, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 50})
	if err != nil {
		t.Fatalf("apply with offset: %v", err)
	}
	want := strings.Repeat("lead\n", 10) + "X\nQQ\nz\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
	if res.Hunks[0].Offset == 0 {
		t.Fatal("expected non-zero offset")
	}
}

func TestApplyOffsetExceeded(t *testing.T) {
	p := genPatch(t, "a\nb\nc\nX\nY\nz\n", "a\nb\nc\nX\nQQ\nz\n", PatchOptions{Context: 1})
	target := strings.Repeat("lead\n", 100) + "X\nY\nz\n"
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 10})
	var oe *OffsetExceededError
	if !errors.As(err, &oe) {
		t.Fatalf("want OffsetExceededError, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("no output allowed on failure")
	}
}

func TestApplyAmbiguous(t *testing.T) {
	// repeated identical blocks: a context-only hunk must not guess
	oldS := strings.Repeat("x\n", 50) + "K\nx\n"
	newS := strings.Repeat("x\n", 50) + "K\nQ\n"
	p := genPatch(t, oldS, newS, PatchOptions{Context: 3})
	// Craft target where the full hunk pattern (contexts + change) occurs twice:
	// duplicate the distinctive "x x x K x" neighbourhood.
	block := "x\nx\nx\nK\nx\n"
	target := strings.Repeat("u\n", 5) + block + strings.Repeat("v\n", 5) + block
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 200})
	var ae *AmbiguousMatchError
	if !errors.As(err, &ae) {
		t.Fatalf("want AmbiguousMatchError, got %v", err)
	}
	if len(ae.Candidates) < 2 {
		t.Fatalf("expected >=2 candidates, got %v", ae.Candidates)
	}
	if buf.Len() != 0 {
		t.Fatal("no output allowed on ambiguity")
	}
}

func TestApplyFuzz(t *testing.T) {
	// Edge context lines shifted content; fuzz drops one context line on each
	// side, and the inner pattern (c3 OLD c4 c5) must locate the hunk.
	p := genPatch(t, "c1\nc2\nc3\nOLD\nc4\nc5\nc6\n", "c1\nc2\nc3\nNEW\nc4\nc5\nc6\n", PatchOptions{})
	target := "ZZZ\nc2\nc3\nOLD\nc4\nc5\nc6\nZZZ\n"
	var buf bytes.Buffer
	if _, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{Fuzz: 1}); err != nil {
		t.Fatalf("fuzz apply: %v", err)
	}
	if buf.String() != "ZZZ\nc2\nc3\nNEW\nc4\nc5\nc6\nZZZ\n" {
		t.Fatalf("got %q", buf.String())
	}
	// without fuzz it should fail (outermost context no longer matches)
	var buf2 bytes.Buffer
	_, err := Apply(context.Background(), strReader(target), p, &buf2, ApplyOptions{MaxOffset: 5})
	if err == nil {
		t.Fatal("expected failure without fuzz")
	}
}

func TestApplyWhitespaceAndEOL(t *testing.T) {
	p := genPatch(t, "a \nb\r\nc\n", "a \nB\r\nc\n", PatchOptions{})
	target := "a\nb\nc\n"
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{
		Compare: LineCompareOptions{NormalizeEOL: true, IgnoreTrailingWhitespace: true},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// inserted content comes verbatim from the patch (CRLF preserved)
	if buf.String() != "a\nB\r\nc\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestApplyBounds(t *testing.T) {
	p := genPatch(t, "a\nb\nc\n", "a\nB\nc\n", PatchOptions{})
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader("totally\ndifferent\n"), p, &buf, ApplyOptions{MaxOffset: 20})
	var be *BoundsError
	if !errors.As(err, &be) {
		t.Fatalf("want BoundsError, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("no partial output")
	}
}

func TestApplyReverseRoundTrip(t *testing.T) {
	cases := [][2]string{
		{"a\nb\nc\n", "a\nB\nc\n"},
		{"a\nb", "a\nB"},
		{"a\nb\n", "a\nb"},
		{"a\nb", "a\nb\n"},
		{"x\n", "x\ny\n"},
		{"x\ny\n", "x\n"},
	}
	for i, c := range cases {
		p := genPatch(t, c[0], c[1], PatchOptions{})
		fwd, _ := applyPatch(t, c[0], p, ApplyOptions{})
		if fwd != c[1] {
			t.Fatalf("case %d fwd %q != %q", i, fwd, c[1])
		}
		var back bytes.Buffer
		if _, err := ApplyReverse(context.Background(), strReader(fwd), p, &back, ApplyOptions{}); err != nil {
			t.Fatalf("case %d reverse: %v", i, err)
		}
		if back.String() != c[0] {
			t.Fatalf("case %d reverse got %q want %q", i, back.String(), c[0])
		}
	}
}

func TestApplyReverseFailureKeepsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("wrong\ncontent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := genPatch(t, "a\nb\nc\n", "a\nB\nc\n", PatchOptions{})
	orig, _ := os.ReadFile(path)
	_, err := ApplyToFile(context.Background(), path, p, true, ApplyOptions{MaxOffset: 5})
	if err == nil {
		t.Fatal("expected reverse apply to fail on unrelated content")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, orig) {
		t.Fatalf("file changed on failed reverse: %q", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".diffmerge-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}

func TestApplyAtomicFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	content := "a\nb\nc\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p := genPatch(t, content, "a\nB\nc\n", PatchOptions{})
	if _, err := ApplyToFile(context.Background(), path, p, false, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a\nB\nc\n" {
		t.Fatalf("got %q", got)
	}
	// undo via reverse
	if _, err := ApplyToFile(context.Background(), path, p, true, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != content {
		t.Fatalf("reverse got %q", got)
	}
}

func TestApplyCancellationLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("l\n", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	p := genPatch(t, "a\n", "b\n", PatchOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ApplyToFile(ctx, path, p, false, ApplyOptions{MaxOffset: 5})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	orig, _ := os.ReadFile(path)
	if len(orig) != 200 {
		t.Fatal("file corrupted after cancellation")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".diffmerge-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files: %v", matches)
	}
}

func TestApplyEncodingError(t *testing.T) {
	p := genPatch(t, "a\nb\n", "a\nB\n", PatchOptions{})
	var buf bytes.Buffer
	_, err := Apply(context.Background(), bytes.NewReader([]byte("a\n\xff\xfe\n")), p, &buf,
		ApplyOptions{RequireUTF8: true})
	var ee *EncodingError
	if !errors.As(err, &ee) {
		t.Fatalf("want EncodingError, got %v", err)
	}
}

func TestApplyInsertAndDelete(t *testing.T) {
	cases := [][2]string{
		{"", "a\n"},
		{"a\n", ""},
		{"a\nb\nc\n", "a\nx\ny\nc\n"},
		{"a\nb\nc\nd\n", "b\nc\n"},
	}
	for i, c := range cases {
		p := genPatch(t, c[0], c[1], PatchOptions{})
		got, _ := applyPatch(t, c[0], p, ApplyOptions{})
		if got != c[1] {
			t.Fatalf("case %d: %q != %q", i, got, c[1])
		}
		// reverse
		var back bytes.Buffer
		if _, err := ApplyReverse(context.Background(), strReader(got), p, &back, ApplyOptions{}); err != nil {
			t.Fatalf("case %d reverse: %v", i, err)
		}
		if back.String() != c[0] {
			t.Fatalf("case %d back %q != %q", i, back.String(), c[0])
		}
	}
}

func TestApplyMultipleHunksInOrder(t *testing.T) {
	oldS := "h1\nA\nmid\nmid2\nB\nh2\n"
	newS := "h1\nAA\nmid\nmid2\nBB\nh2\n"
	p := genPatch(t, oldS, newS, PatchOptions{Context: 1})
	got, _ := applyPatch(t, oldS, p, ApplyOptions{})
	if got != newS {
		t.Fatalf("got %q want %q", got, newS)
	}
}

func TestApplyWhitespaceWithSpill(t *testing.T) {
	// Patch built against content with trailing spaces/CRLF; target has
	// normalized whitespace/EOL, and is large enough to force disk spill.
	var oldB, newB, tgtB strings.Builder
	for i := 0; i < 3000; i++ {
		oldB.WriteString("filler \r\n")
		newB.WriteString("filler \r\n")
		tgtB.WriteString("filler\n")
	}
	oldB.WriteString("CHANGE_ME\r\n")
	newB.WriteString("CHANGED\r\n")
	tgtB.WriteString("CHANGE_ME\n")
	p := genPatch(t, oldB.String(), newB.String(), PatchOptions{Context: 1})
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader(tgtB.String()), p, &buf, ApplyOptions{
		Compare:        LineCompareOptions{NormalizeEOL: true, IgnoreTrailingWhitespace: true},
		MaxMemoryBytes: 64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "CHANGED\r\n") {
		t.Fatalf("change not applied verbatim; tail=%q", buf.String()[len(buf.String())-20:])
	}
	if strings.Count(buf.String(), "filler") != 3000 {
		t.Fatal("target filler lines lost")
	}
}

func TestApplyAppendNewlineEdge(t *testing.T) {
	// Applying an append hunk to a file whose prefix shifted but suffix is
	// unique should locate by context and preserve EOF newline semantics.
	p := genPatch(t, "unique-prefix\nlast\n", "unique-prefix\nlast\nextra\n", PatchOptions{Context: 2})
	target := strings.Repeat("shift\n", 50) + "unique-prefix\nlast\n"
	var buf bytes.Buffer
	res, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 200})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("shift\n", 50) + "unique-prefix\nlast\nextra\n"
	if buf.String() != want {
		t.Fatalf("got %q", buf.String())
	}
	if res.Hunks[0].Offset != 50 {
		t.Fatalf("offset=%d want 50", res.Hunks[0].Offset)
	}
}

func TestApplyUnlimitedOffset(t *testing.T) {
	p := genPatch(t, "needle-X\ntail\n", "needle-X\nTAIL\n", PatchOptions{Context: 1})
	target := strings.Repeat("z\n", 5000) + "needle-X\ntail\n"
	var buf bytes.Buffer
	if _, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: -1}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "needle-X\nTAIL\n") {
		t.Fatal("unlimited offset apply failed")
	}
}

func TestApplyFailureNoPartialWrite(t *testing.T) {
	// Multi-hunk patch where the second hunk is unlocatable: not one byte of
	// output may be produced even though the first hunk matches.
	oldS := "a1\nm\nb1\npad\nx\nm\nc1\n"
	newS := "a1\nM\nb1\npad\nx\nM\nc1\n"
	p := genPatch(t, oldS, newS, PatchOptions{Context: 1})
	target := "a1\nm\nb1\n" + strings.Repeat("q\n", 50) + "NO_MATCH_AT_ALL\n"
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 100})
	if err == nil {
		t.Fatal("expected apply failure")
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes before failing", buf.Len())
	}
}

func TestApplyUniqueWithinWindowEvenIfRepeatedFarAway(t *testing.T) {
	p := genPatch(t, "same\nNEEDLE\nsame\n", "same\nFOUND\nsame\n", PatchOptions{Context: 1})
	// A second identical neighbourhood exists far beyond the offset window.
	target := "same\nNEEDLE\nsame\n" + strings.Repeat("far\n", 300) + "same\nNEEDLE\nsame\n"
	var buf bytes.Buffer
	if _, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 20}); err != nil {
		t.Fatalf("unique-in-window must apply: %v", err)
	}
	if strings.Count(buf.String(), "FOUND") != 1 {
		t.Fatalf("exactly one replacement expected:\n%s", buf.String())
	}
}

func TestApplyAmbiguousWhenDeclaredShifted(t *testing.T) {
	// Declared location no longer matches, but the full pattern appears twice
	// within the window: that must be reported ambiguous rather than guessed.
	p := genPatch(t, "same\nNEEDLE\nsame\n", "same\nFOUND\nsame\n", PatchOptions{Context: 1})
	first := "same\nNEEDLE\nsame\n"
	target := "prelude line one\n" + first + strings.Repeat("mid\n", 10) + first
	var buf bytes.Buffer
	_, err := Apply(context.Background(), strReader(target), p, &buf, ApplyOptions{MaxOffset: 100})
	var ae *AmbiguousMatchError
	if !errors.As(err, &ae) || len(ae.Candidates) != 2 {
		t.Fatalf("want AmbiguousMatchError with 2 candidates, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("no output on ambiguity")
	}
}
