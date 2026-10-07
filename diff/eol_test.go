package diff

import (
	"strings"
	"testing"
)

func TestCRLFPatchRoundTripFormat(t *testing.T) {
	old := "a\r\nb\r\nc\r\n"
	new := "a\r\nB\r\nc\r\n"
	p := Generate(old, new, 1)
	text := p.Format()
	if !strings.Contains(text, "# eol-old: crlf") || !strings.Contains(text, "# eol-new: crlf") {
		t.Fatalf("missing crlf directives:\n%s", text)
	}
	// Format must be stable.
	if p.Format() != text {
		t.Fatal("Format is not deterministic")
	}
}

func TestMixedStylePatches(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\r\nB\r\nc\r\n"
	p := Generate(old, new, 1)
	if p.OldEOL != EOLLF || p.NewEOL != EOLCRLF {
		t.Fatalf("eol old=%v new=%v", p.OldEOL, p.NewEOL)
	}
	text := p.Format()
	if strings.Contains(text, "# eol-old") {
		t.Fatalf("old LF must not emit directive:\n%s", text)
	}
	if !strings.Contains(text, "# eol-new: crlf") {
		t.Fatalf("missing new crlf directive:\n%s", text)
	}
}

func TestGenerateOnlyTrailingNewlineRemoved(t *testing.T) {
	p := Generate("a\nb\nc\n", "a\nb\nc", 1)
	if len(p.Hunks) != 1 {
		t.Fatalf("expected 1 hunk for EOF-only change, got %d", len(p.Hunks))
	}
	h := p.Hunks[0]
	var hasMarker bool
	for _, l := range h.Lines {
		if l.NoNewline {
			hasMarker = true
		}
	}
	if !hasMarker {
		t.Fatalf("no no-newline marker:\n%s", p.Format())
	}
	if !p.NewNoFinalNL || p.OldNoFinalNL {
		t.Fatalf("metadata wrong: old=%v new=%v", p.OldNoFinalNL, p.NewNoFinalNL)
	}
}

func TestGenerateOnlyTrailingNewlineAdded(t *testing.T) {
	p := Generate("x", "x\n", 1)
	if len(p.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(p.Hunks))
	}
	if !p.OldNoFinalNL || p.NewNoFinalNL {
		t.Fatalf("metadata wrong: old=%v new=%v", p.OldNoFinalNL, p.NewNoFinalNL)
	}
	text := p.Format()
	if !strings.Contains(text, "\\ No newline at end of file") {
		t.Fatalf("missing marker:\n%s", text)
	}
}

func TestGenerateIdenticalNoChange(t *testing.T) {
	for _, s := range []string{"a\nb\n", "a\r\nb\r\n", "a", "", "abc\n", "abc"} {
		p := Generate(s, s, 3)
		if len(p.Hunks) != 0 {
			t.Fatalf("Generate(%q) produced hunks: %d", s, len(p.Hunks))
		}
	}
}

func TestDetectEOL(t *testing.T) {
	cases := map[string]EOL{
		"":              EOLDefault,
		"abc":           EOLDefault,
		"a\nb\n":        EOLLF,
		"a\r\nb\r\n":    EOLCRLF,
		"a\rb\r":        EOLCRLF,
		"abc\ndef\r\n":  EOLLF,
		"abc\r\ndef\n":  EOLCRLF,
	}
	for in, want := range cases {
		if got := DetectEOL(in); got != want {
			t.Errorf("DetectEOL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNoFinalNewline(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"", false}, {"a", true}, {"a\n", false}, {"a\r", false},
		{"a\r\n", false}, {"a\nb", true}, {"a\nb\n", false},
	}
	for _, c := range cases {
		if got := NoFinalNewline(c.s); got != c.want {
			t.Errorf("NoFinalNewline(%q) = %v want %v", c.s, got, c.want)
		}
	}
}

func TestRenderLines(t *testing.T) {
	if got := RenderLines([]string{"a", "b"}, EOLCRLF, false); got != "a\r\nb\r\n" {
		t.Errorf("crlf = %q", got)
	}
	if got := RenderLines([]string{"a", "b"}, EOLLF, true); got != "a\nb" {
		t.Errorf("no-final = %q", got)
	}
	if got := RenderLines(nil, EOLLF, false); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got := RenderLines([]string{"x"}, EOLDefault, false); got != "x\n" {
		t.Errorf("default = %q", got)
	}
}
