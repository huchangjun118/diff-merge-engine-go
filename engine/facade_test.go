package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

func TestSubmitAndHeadThroughEngine(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := "a\nb\nc\n"
	if _, err := e.InitBase(base); err != nil {
		t.Fatal(err)
	}
	p := diff.Generate(base, "a\nB\nc\n", 1)
	h, err := e.Submit(p, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := e.Head()
	if head != h {
		t.Fatalf("HEAD=%s want %s", head, h)
	}
	again, err := e.Submit(p, patch.ApplyOptions{})
	if err != nil || again != h {
		t.Fatalf("duplicate submit = %s,%v", again, err)
	}
}

func TestApplyStreamFileThroughEngine(t *testing.T) {
	e, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := diff.Generate("a\r\nb\r\nc\r\n", "a\r\nB\r\nc\r\n", 1)
	dst := filepath.Join(t.TempDir(), "result.txt")
	err = e.ApplyStreamFile(context.Background(), strings.NewReader("a\nb\nc\n"), dst, p, patch.ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a\r\nB\r\nc\r\n" {
		t.Errorf("engine file apply = %q", got)
	}
}
