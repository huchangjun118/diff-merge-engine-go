package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestEngineDiffApplyReverse(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	old := "a\nb\nc\nd\n"
	new := "a\nB\nc\nd\ne\n"
	p := e.Diff(old, new, 3)
	applied, err := e.Apply(old, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if applied != new {
		t.Errorf("Apply = %q; want %q", applied, new)
	}
	restored, err := e.ApplyReverse(applied, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyReverse failed: %v", err)
	}
	if restored != old {
		t.Errorf("Restore = %q; want %q", restored, old)
	}
}

func TestEngineMerge(t *testing.T) {
	e, _ := New(t.TempDir())
	base := "a\nb\nc\n"
	ours := "a\nB\nc\n"
	theirs := "a\nb\nC\n"
	result := e.Merge(base, ours, theirs)
	if !result.Clean {
		t.Fatalf("expected clean merge: %v", result.Conflicts)
	}
	want := "a\nB\nC\n"
	if result.String() != want {
		t.Errorf("Merge = %q; want %q", result.String(), want)
	}
}

func TestEngineApplyStream(t *testing.T) {
	e, _ := New(t.TempDir())
	base := "a\nb\nc\n"
	p := e.Diff(base, "a\nB\nc\n", 1)
	var out strings.Builder
	err := e.ApplyStream(context.Background(), strings.NewReader(base), &out, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyStream failed: %v", err)
	}
	if out.String() != "a\nB\nc\n" {
		t.Errorf("Stream = %q", out.String())
	}
}

func TestEngineStore(t *testing.T) {
	e, _ := New(t.TempDir())
	base := "a\nb\nc\n"
	baseHash, err := e.Store().Put(base)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	p := e.Diff(base, "a\nB\nc\n", 1)
	resultHash, err := e.Store().ApplyPatch(baseHash, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyPatch failed: %v", err)
	}
	result, err := e.Store().Get(resultHash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if result != "a\nB\nc\n" {
		t.Errorf("Result = %q", result)
	}
}

func TestEngineMalformedPatch(t *testing.T) {
	_, err := patch.Parse("not a patch")
	if err == nil {
		t.Fatal("expected error")
	}
	if !derr.IsKind(err, derr.KindMalformedPatch) {
		t.Errorf("kind = %v; want KindMalformedPatch", err)
	}
}
