package engine

import (
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestEngineSubmitFlow(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := "a\nb\nc\nd\n"
	bh, err := e.Store().Put(base)
	if err != nil {
		t.Fatal(err)
	}
	p1 := diff.Generate(base, "a\nB\nc\nd\n", 1)
	p2 := diff.Generate(base, "a\nb\nc\nD\n", 1)
	if _, err := e.Submit(bh, p1, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(bh, p2, patch.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	tip, err := e.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip != "a\nB\nc\nD\n" {
		t.Fatalf("tip = %q", tip)
	}
	h, _ := e.Head()
	if h == "" {
		t.Fatal("head empty")
	}
}
