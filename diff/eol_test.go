package diff

import (
	"strings"
	"testing"
)

// TestCRLFPatchCarriesMetadata is requirement 1 at the generation layer:
// a patch made from CRLF text must carry the CRLF style explicitly and keep
// its own body LF-encoded.
func TestCRLFPatchCarriesMetadata(t *testing.T) {
	old := "line1\r\nline2\r\nline3\r\n"
	new := "line1\r\nline2-changed\r\nline3\r\nline4\r\n"
	p := Generate(old, new, 3)
	text := p.Format()

	if strings.Contains(text, "\r") {
		t.Errorf("patch body must be LF-encoded:\n%s", text)
	}
	if !strings.Contains(text, "eol: old=crlf new=crlf") {
		t.Fatalf("patch missing CRLF directive:\n%s", text)
	}
	if p.OldEOL != EOLCRLF || p.NewEOL != EOLCRLF {
		t.Fatalf("EOL metadata = %v/%v", p.OldEOL, p.NewEOL)
	}
	if !p.OldTrailingNewline || !p.NewTrailingNewline {
		t.Fatalf("trailing metadata wrong: %v/%v", p.OldTrailingNewline, p.NewTrailingNewline)
	}
}

func TestLFPatchHasNoDirective(t *testing.T) {
	p := Generate("a\nb\nc\n", "a\nB\nc\n", 1)
	if strings.Contains(p.Format(), DirectiveMarker()) {
		t.Errorf("default LF/trailing patch must not carry a directive:\n%s", p.Format())
	}
}

func TestEOLConversionIsMetadataOnly(t *testing.T) {
	p := Generate("a\nb\nc\n", "a\r\nb\r\nc\r\n", 1)
	if len(p.Hunks) != 0 {
		t.Fatalf("EOL-only change must not emit hunks, got %d", len(p.Hunks))
	}
	if p.OldEOL != EOLLF || p.NewEOL != EOLCRLF {
		t.Fatalf("EOL metadata = %v/%v", p.OldEOL, p.NewEOL)
	}
}

func TestTrailingNewlineHunkShape(t *testing.T) {
	// Remove trailing newline only: the last line appears as a delete/add
	// pair carrying a no-newline marker on the add side.
	p := Generate("a\nb\nc\n", "a\nb\nc", 2)
	if len(p.Hunks) == 0 {
		t.Fatal("trailing-only change produced an empty diff")
	}
	text := p.Format()
	if !strings.Contains(text, NoNewlineMarker()) {
		t.Fatalf("missing no-newline marker:\n%s", text)
	}
	if p.OldTrailingNewline != true || p.NewTrailingNewline != false {
		t.Fatalf("trailing flags = %v/%v", p.OldTrailingNewline, p.NewTrailingNewline)
	}
	last := p.Hunks[len(p.Hunks)-1]
	sawAddNoNL := false
	for _, l := range last.Lines {
		if l.Op == OpAdd && l.NoNewline {
			sawAddNoNL = true
		}
	}
	if !sawAddNoNL {
		t.Errorf("expected an add line with NoNewline in final hunk")
	}

	p2 := Generate("a\nb\nc", "a\nb\nc\n", 2)
	if len(p2.Hunks) == 0 {
		t.Fatal("adding trailing newline produced an empty diff")
	}
}

func TestNoChangeStaysEmpty(t *testing.T) {
	if p := Generate("a\nb\nc\n", "a\nb\nc\n", 3); len(p.Hunks) != 0 {
		t.Errorf("identical inputs must produce no hunks, got %d", len(p.Hunks))
	}
	if p := Generate("a\nb\nc", "a\nb\nc", 3); len(p.Hunks) != 0 {
		t.Errorf("identical no-nl inputs must produce no hunks")
	}
}

func TestContentChangeWithEOFNewlineFlags(t *testing.T) {
	// A content change that also removes the final newline: the last hunk
	// reaches EOF and must carry the flag without a separate synthetic hunk.
	p := Generate("a\nb\nc\n", "a\nB\nc", 2)
	if len(p.Hunks) != 1 {
		t.Fatalf("expected single merged hunk, got %d", len(p.Hunks))
	}
	text := p.Format()
	if strings.Count(text, NoNewlineMarker()) == 0 {
		t.Errorf("expected no-newline marker:\n%s", text)
	}
}
