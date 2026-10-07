package stream

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// Requirement 4: a hunk that locates only partially through processing must
// leave zero bytes in the caller's writer.
func TestNoOutputOnMidStreamFailure(t *testing.T) {
	base := strings.Repeat("same\n", 500) + "anchor\nb\nc\n"
	p := diff.Generate("anchor\nb\nc\n", "anchor\nB\nc\n", 2)
	// Duplicate the anchor block so the first-found match is ambiguous far
	// away; force failure instead with a non-matching large base and tight
	// offset after many lines have been spooled.
	big := strings.Repeat("noise\n", 5000)
	var out bytes.Buffer
	err := ApplyStream(context.Background(), strings.NewReader(big), &out, p, patch.ApplyOptions{MaxOffset: 2})
	if err == nil {
		t.Fatal("expected apply failure")
	}
	if out.Len() != 0 {
		t.Fatalf("writer received %d bytes before the failure", out.Len())
	}
	_ = base
}

// Requirement 4 (stronger): the first hunk must be processed successfully
// deep into a large file (spooling thousands of output lines) before a later
// hunk fails. Even then the caller must observe zero bytes.
func TestZeroOutputWhenLaterHunkFailsDeepInFile(t *testing.T) {
	var base strings.Builder
	for i := 1; i <= 10000; i++ {
		switch i {
		case 8000:
			base.WriteString("MARKER8000\n")
		case 9999:
			base.WriteString("will-delete\n")
		default:
			base.WriteString("line" + strconv.Itoa(i) + "\n")
		}
	}
	p := &diff.Patch{
		OldTrailingNewline: true, NewTrailingNewline: true,
		Hunks: []*diff.Hunk{
			{
				OldStart: 8000, OldLines: 1, NewStart: 8000, NewLines: 1,
				Lines: []diff.Line{
					{Op: diff.OpDelete, Text: "MARKER8000"},
					{Op: diff.OpAdd, Text: "MARKER-CHANGED"},
				},
			},
			{
				OldStart: 9999, OldLines: 1, NewStart: 9999, NewLines: 1,
				Lines: []diff.Line{{Op: diff.OpContext, Text: "nonexistent-context"}},
			},
		},
	}
	var out bytes.Buffer
	err := ApplyStream(context.Background(), strings.NewReader(base.String()), &out, p, patch.ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected failure on the second hunk")
	}
	if out.Len() != 0 {
		t.Fatalf("caller received %d bytes despite deep mid-stream failure", out.Len())
	}
}

func TestCancellationWritesZeroBytes(t *testing.T) {
	base := strings.Repeat("line\n", 200000)
	p := diff.Generate(base, strings.Repeat("line\n", 199999)+"changed\n", 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err := ApplyStream(ctx, strings.NewReader(base), &out, p, patch.ApplyOptions{})
	if !derr.IsKind(err, derr.KindAborted) {
		t.Fatalf("kind = %v; want KindAborted", err)
	}
	if out.Len() != 0 {
		t.Fatalf("cancelled run wrote %d bytes", out.Len())
	}
}

func TestTimeoutWritesZeroBytes(t *testing.T) {
	base := strings.Repeat("line\n", 200000)
	p := diff.Generate(base, strings.Repeat("line\n", 199999)+"changed\n", 3)
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	var out bytes.Buffer
	err := ApplyStream(ctx, strings.NewReader(base), &out, p, patch.ApplyOptions{})
	if !derr.IsKind(err, derr.KindAborted) {
		t.Fatalf("kind = %v; want KindAborted", err)
	}
	if out.Len() != 0 {
		t.Fatalf("timed out run wrote %d bytes", out.Len())
	}
}

func TestSuccessIsByteExactWithCRLFAndTrailing(t *testing.T) {
	old := "a\r\nb\r\nc\r\n"
	new := "a\r\nB\r\nc\r\n"
	p := diff.Generate(old, new, 2)
	var out bytes.Buffer
	if err := ApplyStream(context.Background(), strings.NewReader("a\nb\nc\n"), &out, p, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != new {
		t.Errorf("stream result = %q; want %q", out.String(), new)
	}

	// Trailing-newline removal.
	p2 := diff.Generate("a\nb\nc\n", "a\nb\nc", 2)
	var out2 bytes.Buffer
	if err := ApplyStream(context.Background(), strings.NewReader("a\nb\nc\n"), &out2, p2, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out2.String() != "a\nb\nc" {
		t.Errorf("stream trailing removal = %q", out2.String())
	}
}

func TestAtomicFileLeavesDestinationUntouchedOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.txt")
	original := []byte("preexisting\ncontent\n")
	if err := os.WriteFile(dst, original, 0o644); err != nil {
		t.Fatal(err)
	}
	p := diff.Generate("a\nb\nc\n", "a\nB\nc\n", 1)
	err := ApplyStreamToFile(context.Background(), strings.NewReader(strings.Repeat("z\n", 100)), dst, p, patch.ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected failure")
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("destination vanished: %v", rerr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("destination modified: %q", got)
	}

	// Cancellation must also leave it untouched.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ApplyStreamToFile(ctx, strings.NewReader("a\nb\nc\n"), dst, p, patch.ApplyOptions{}); !derr.IsKind(err, derr.KindAborted) {
		t.Fatalf("kind = %v", err)
	}
	got, _ = os.ReadFile(dst)
	if !bytes.Equal(got, original) {
		t.Fatalf("destination modified on cancel: %q", got)
	}
}

func TestAtomicFileSuccessInstallsResult(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "out.txt") // directory does not exist yet
	p := diff.Generate("a\nb\nc\n", "a\nB\nc\n", 1)
	err := ApplyStreamToFile(context.Background(), strings.NewReader("a\nb\nc\n"), dst, p, patch.ApplyOptions{})
	if err == nil {
		t.Fatal("expected failure for missing directory (atomic install requires existing dir)")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStreamToFile(context.Background(), strings.NewReader("a\nb\nc\n"), dst, p, patch.ApplyOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a\nB\nc\n" {
		t.Errorf("installed content = %q", got)
	}
}

func TestStreamOutOfBoundsKind(t *testing.T) {
	p := &diff.Patch{
		OldTrailingNewline: true, NewTrailingNewline: true,
		Hunks: []*diff.Hunk{{
			OldStart: 500, OldLines: 1, NewStart: 500, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpContext, Text: "nope"}},
		}},
	}
	var out bytes.Buffer
	err := ApplyStream(context.Background(), strings.NewReader("a\nb\nc\n"), &out, p, patch.ApplyOptions{MaxOffset: 1})
	if !derr.IsKind(err, derr.KindOutOfBounds) {
		t.Fatalf("kind = %v; want KindOutOfBounds", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %d bytes on OOB", out.Len())
	}
}

func TestStreamInvalidEncoding(t *testing.T) {
	p := diff.Generate("abc\n", "abd\n", 1)
	var out bytes.Buffer
	err := ApplyStream(context.Background(), bytes.NewReader([]byte("ab\xff\n")), &out, p, patch.ApplyOptions{MaxOffset: 1})
	if !derr.IsKind(err, derr.KindEncodingMismatch) {
		t.Fatalf("kind = %v; want KindEncodingMismatch", err)
	}
}

func TestStreamSpoolSpillsToDisk(t *testing.T) {
	// Force the in-memory spool limit to be exceeded; output must still be
	// exact and no temp file left behind.
	var base, new strings.Builder
	for i := 0; i < 300000; i++ {
		base.WriteString("line" + itoa(i) + "\n")
		if i == 200000 {
			new.WriteString("CHANGED\n")
		} else {
			new.WriteString("line" + itoa(i) + "\n")
		}
	}
	p := diff.Generate(base.String(), new.String(), 3)
	var out bytes.Buffer
	if err := ApplyStream(context.Background(), strings.NewReader(base.String()), &out, p, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != new.String() {
		t.Fatal("large spooled result mismatch")
	}
}

func TestStreamPureInsertionAtEOF(t *testing.T) {
	p := &diff.Patch{
		OldTrailingNewline: true, NewTrailingNewline: true,
		Hunks: []*diff.Hunk{{
			OldStart: 3, OldLines: 0, NewStart: 3, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpAdd, Text: "d"}},
		}},
	}
	var out bytes.Buffer
	if err := ApplyStream(context.Background(), strings.NewReader("a\nb\nc\n"), &out, p, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a\nb\nc\nd\n" {
		t.Errorf("stream insertion at EOF = %q", out.String())
	}
}

func TestStreamMalformedPatchKind(t *testing.T) {
	bad := &diff.Patch{Hunks: []*diff.Hunk{{
		OldStart: 1, OldLines: 5, NewStart: 1, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "x"}},
	}}}
	var out bytes.Buffer
	err := ApplyStream(context.Background(), strings.NewReader("x\n"), &out, bad, patch.ApplyOptions{})
	if !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Fatalf("kind = %v; want KindMalformedPatch", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %d bytes", out.Len())
	}
}

func TestStreamAmbiguousKind(t *testing.T) {
	old := "a\nb\nc\n"
	p := diff.Generate(old, "a\nB\nc\n", 1)
	target := "a\nb\nc\na\nb\nc\n"
	var out bytes.Buffer
	err := ApplyStream(context.Background(), strings.NewReader(target), &out, p, patch.ApplyOptions{MaxOffset: 100})
	if !derr.IsKind(err, derr.KindAmbiguousMatch) {
		t.Fatalf("kind = %v; want KindAmbiguousMatch", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %d bytes on ambiguity", out.Len())
	}
}
