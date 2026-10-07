package stream

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestStreamMidHunkFailureZeroBytes(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	// First hunk matches; second references a range beyond the file.
	p := &diff.Patch{NewEOL: diff.EOLLF, Hunks: []*diff.Hunk{
		{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpContext, Text: "a"}}},
		{OldStart: 9, OldLines: 1, NewStart: 9, NewLines: 1,
			Lines: []diff.Line{{Op: diff.OpContext, Text: "q"}}},
	}}
	var out strings.Builder
	err := ApplyStream(context.Background(), strings.NewReader(base), &out, p, patch.ApplyOptions{MaxOffset: 2})
	if err == nil {
		t.Fatal("expected error")
	}
	if out.Len() != 0 {
		t.Fatalf("output received %d bytes after failure: %q", out.Len(), out.String())
	}
	if !derr.IsKind(err, derr.KindOutOfBounds) && !derr.IsKind(err, derr.KindNoMatch) {
		t.Fatalf("unexpected kind: %v", err)
	}
}

func TestStreamContextMismatchZeroBytes(t *testing.T) {
	base := strings.Repeat("line\n", 1000)
	p := &diff.Patch{NewEOL: diff.EOLLF, Hunks: []*diff.Hunk{{
		OldStart: 900, OldLines: 1, NewStart: 900, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "definitely-not-present"}}}}}
	var out strings.Builder
	err := ApplyStream(context.Background(), strings.NewReader(base), &out, p, patch.ApplyOptions{MaxOffset: 10})
	if err == nil {
		t.Fatal("expected error")
	}
	if out.Len() != 0 {
		t.Fatalf("output has %d bytes", out.Len())
	}
}

func TestStreamCancellationZeroBytes(t *testing.T) {
	base := strings.Repeat("line\n", 200000)
	// Hunk near the end forces substantial processing before location.
	p := &diff.Patch{NewEOL: diff.EOLLF, Hunks: []*diff.Hunk{{
		OldStart: 199999, OldLines: 1, NewStart: 199999, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "line"}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	err := ApplyStream(ctx, strings.NewReader(base), &out, p, patch.ApplyOptions{})
	if !derr.IsKind(err, derr.KindAborted) {
		t.Fatalf("want aborted, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output has %d bytes after cancel", out.Len())
	}
}

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) { return 0, errors.New("disk full") }

func TestStreamCommitWriterFailure(t *testing.T) {
	p := diff.Generate("a\n", "b\n", 1)
	err := ApplyStream(context.Background(), strings.NewReader("a\n"), &failingWriter{}, p, patch.ApplyOptions{})
	if err == nil {
		t.Fatal("expected write error")
	}
}

func TestStreamCRLFOutput(t *testing.T) {
	p := diff.Generate("a\r\nb\r\nc\r\n", "a\r\nB\r\nc\r\n", 1)
	var out strings.Builder
	if err := ApplyStream(context.Background(), strings.NewReader("a\nb\nc\n"), &out, p, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a\r\nB\r\nc\r\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestStreamEOFNewlineState(t *testing.T) {
	p := diff.Generate("a\nb\nc\n", "a\nb\nc", 1)
	var out strings.Builder
	if err := ApplyStream(context.Background(), strings.NewReader("a\nb\nc\n"), &out, p, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "a\nb\nc" {
		t.Fatalf("got %q", out.String())
	}
}

func TestStreamEmptyPatchKeepsContent(t *testing.T) {
	base := "a\nb\nc\n"
	var out strings.Builder
	if err := ApplyStream(context.Background(), strings.NewReader(base), &out, &diff.Patch{}, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != base {
		t.Fatalf("got %q", out.String())
	}
}

func TestStreamInvalidUTF8(t *testing.T) {
	p := diff.Generate("a\n", "b\n", 1)
	var out strings.Builder
	err := ApplyStream(context.Background(), strings.NewReader("a\n\xff\n"), &out, p, patch.ApplyOptions{})
	if !derr.IsKind(err, derr.KindEncodingMismatch) {
		t.Fatalf("want encoding error, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output has %d bytes", out.Len())
	}
}

func TestStreamLargeSpoolFailureZeroBytes(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200000; i++ {
		sb.WriteString("line\n")
	}
	base := sb.String()
	p := &diff.Patch{NewEOL: diff.EOLLF, Hunks: []*diff.Hunk{{
		OldStart: 190000, OldLines: 1, NewStart: 190000, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "missing"}}}}}
	var out strings.Builder
	err := ApplyStream(context.Background(), strings.NewReader(base), &out, p, patch.ApplyOptions{MaxOffset: 5})
	if err == nil {
		t.Fatal("expected error")
	}
	if out.Len() != 0 {
		t.Fatalf("output has %d bytes", out.Len())
	}
}
