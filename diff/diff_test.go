package diff

import (
	"strings"
	"testing"
)

func TestGenerateBasic(t *testing.T) {
	old := "line1\nline2\nline3\n"
	new := "line1\nline2-mod\nline3\nline4\n"
	p := Generate(old, new, 3)
	if len(p.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(p.Hunks))
	}
	h := p.Hunks[0]
	if h.OldStart != 1 || h.OldLines != 3 {
		t.Errorf("old range = %d,%d; want 1,3", h.OldStart, h.OldLines)
	}
	if h.NewStart != 1 || h.NewLines != 4 {
		t.Errorf("new range = %d,%d; want 1,4", h.NewStart, h.NewLines)
	}
}

func TestGenerateNoChanges(t *testing.T) {
	content := "a\nb\nc\n"
	p := Generate(content, content, 3)
	if len(p.Hunks) != 0 {
		t.Fatalf("expected 0 hunks, got %d", len(p.Hunks))
	}
}

func TestGenerateEmpty(t *testing.T) {
	p := Generate("", "a\nb\n", 3)
	if len(p.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(p.Hunks))
	}
	p2 := Generate("a\nb\n", "", 3)
	if len(p2.Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %d", len(p2.Hunks))
	}
}

func TestSplitLinesNormalization(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"a\nb\n", []string{"a", "b"}},
		{"a\r\nb\r\n", []string{"a", "b"}},
		{"a\rb\r", []string{"a", "b"}},
		{"a\nb", []string{"a", "b"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := SplitLines(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("SplitLines(%q) = %v; want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("SplitLines(%q) = %v; want %v", tt.input, got, tt.want)
				break
			}
		}
	}
}

func TestFormatPortable(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nB\nc\n"
	p := Generate(old, new, 1)
	formatted := p.Format()
	if strings.Contains(formatted, "Index:") || strings.Contains(formatted, "index ") {
		t.Error("patch should not contain index lines")
	}
	if strings.Contains(formatted, "\t") {
		t.Error("patch should not contain timestamps")
	}
}

func TestLargeFileBounded(t *testing.T) {
	// Generate a large file and ensure diff completes.
	var oldB, newB strings.Builder
	for i := 0; i < 10000; i++ {
		oldB.WriteString("line " + itoa(i) + "\n")
		if i == 5000 {
			newB.WriteString("modified line\n")
		} else {
			newB.WriteString("line " + itoa(i) + "\n")
		}
	}
	p := Generate(oldB.String(), newB.String(), 3)
	if len(p.Hunks) == 0 {
		t.Error("expected hunks for large file")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestLineEndingExpressed(t *testing.T) {
	// Patches now explicitly carry the line-ending style. The text hunks are
	// identical; only the eol directives differ.
	lf := "a\nb\nc\n"
	crlf := "a\r\nb\r\nc\r\n"
	p1 := Generate(lf, "a\nB\nc\n", 1)
	p2 := Generate(crlf, "a\r\nB\r\nc\r\n", 1)
	if p1.NewEOL != EOLLF || p2.NewEOL != EOLCRLF {
		t.Errorf("EOL = %v,%v; want LF,CRLF", p1.NewEOL, p2.NewEOL)
	}
	if !strings.Contains(p2.Format(), "# eol-new: crlf") {
		t.Errorf("CRLF patch missing eol directive:\n%s", p2.Format())
	}
	// The hunk bodies (ignoring directives) match.
	body1 := stripDirectives(p1.Format())
	body2 := stripDirectives(p2.Format())
	if body1 != body2 {
		t.Errorf("hunk bodies differ:\n%q\n%q", body1, body2)
	}
}

func stripDirectives(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "# ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestNoTrailingNewline(t *testing.T) {
	old := "a\nb\nc"
	new := "a\nB\nc"
	p := Generate(old, new, 1)
	if len(p.Hunks) == 0 {
		t.Error("expected hunks for content without trailing newline")
	}
}

func TestComplexDiff(t *testing.T) {
	// Test a more complex diff with multiple changes.
	old := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	new := "line1\nline2-mod\nline3\nline4\nline5\nline6\nline7-mod\nline8\nline9\n"
	p := Generate(old, new, 1)
	if len(p.Hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(p.Hunks))
	}
	// Verify the patch has the expected structure.
	if p.Hunks[0].OldStart != 1 || p.Hunks[0].NewStart != 1 {
		t.Errorf("first hunk starts at wrong position")
	}
}
