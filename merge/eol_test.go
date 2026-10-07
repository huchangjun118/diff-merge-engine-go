package merge

import (
	"testing"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

func TestMergeTrailingNewlineFollowSide(t *testing.T) {
	// Base has newline; theirs removes it.
	r := Merge("a\nb\n", "a\nb\n", "a\nb")
	if !r.Clean {
		t.Fatalf("expected clean, conflicts=%v", r.Conflicts)
	}
	if r.TrailingNewline {
		t.Fatal("merged result should not end with newline")
	}
	if r.String() != "a\nb" {
		t.Fatalf("got %q", r.String())
	}
}

func TestMergeTrailingNewlineKeptWhenUnchanged(t *testing.T) {
	r := Merge("a\nb\n", "a\nB\n", "a\nb\n")
	if !r.Clean {
		t.Fatalf("expected clean: %v", r.Conflicts)
	}
	if !r.TrailingNewline {
		t.Fatal("trailing newline should be kept")
	}
}

func TestMergeCRLFStyle(t *testing.T) {
	r := Merge("a\r\nb\r\n", "a\r\nB\r\n", "a\r\nb\r\n")
	if !r.Clean {
		t.Fatalf("expected clean: %v", r.Conflicts)
	}
	if r.TrailingEOL != diff.EOLCRLF {
		t.Fatalf("eol = %v, want CRLF", r.TrailingEOL)
	}
	if got := r.String(); got != "a\r\nB\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestMergeRenderExplicitEOL(t *testing.T) {
	r := Merge("a\nb\n", "a\nB\n", "a\nb\n")
	if got := r.Render(2); got != "a\r\nB\r\n" {
		t.Fatalf("crlf render = %q", got)
	}
}
