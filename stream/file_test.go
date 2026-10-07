package stream

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestApplyStreamToFileSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "a\r\nb\r\nc\r\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	p := diff.Generate(original, "a\r\nB\r\nc\r\n", 1)
	if err := ApplyStreamToFile(context.Background(), path, p, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a\r\nB\r\nc\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyStreamToFileFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "a\nb\nc\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// Hunk that cannot locate in this file.
	p := &diff.Patch{NewEOL: diff.EOLLF, Hunks: []*diff.Hunk{{
		OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
		Lines: []diff.Line{{Op: diff.OpContext, Text: "zzz"}}}}}
	err := ApplyStreamToFile(context.Background(), path, p, patch.ApplyOptions{MaxOffset: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("original file modified: got %q want %q", got, original)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestApplyStreamToFileCancelled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "a\nb\n"
	os.WriteFile(path, []byte(original), 0o644)
	p := diff.Generate(original, "x\ny\n", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ApplyStreamToFile(ctx, path, p, patch.ApplyOptions{}); err == nil {
		t.Fatal("expected abort error")
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Fatalf("file changed on cancel: %q", got)
	}
}
