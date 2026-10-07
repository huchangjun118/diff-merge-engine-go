package diff

import (
	"fmt"
	"strings"
)

// Directive names for line-ending styles. They appear between the file
// headers and the first hunk. A missing directive means LF, so ordinary
// LF-only patches remain byte-compatible with standard unified diff.
const (
	DirectiveEOLOld = "# eol-old:"
	DirectiveEOLNew = "# eol-new:"

	noNewlineMarker = "\\ No newline at end of file\n"
)

// Format renders the patch as a portable unified diff.
//
// The output uses only '--- '/'+++ ' file headers and '@@' hunk headers;
// it omits timestamps, index hashes and file modes so the same patch text
// applies identically in different environments.
//
// The rendered patch text itself always uses LF separators; the line-ending
// style of the referenced content is carried by '# eol-old'/'# eol-new'
// directives, and a missing final newline by the standard
// "\ No newline at end of file" marker.
func (p *Patch) Format() string {
	var b strings.Builder
	oldName := p.OldFile
	if oldName == "" {
		oldName = "a/"
	}
	newName := p.NewFile
	if newName == "" {
		newName = "b/"
	}
	b.WriteString("--- " + oldName + "\n")
	b.WriteString("+++ " + newName + "\n")
	if p.OldEOL == EOLCRLF {
		b.WriteString(DirectiveEOLOld + " crlf\n")
	}
	if p.NewEOL == EOLCRLF {
		b.WriteString(DirectiveEOLNew + " crlf\n")
	}
	for _, h := range p.Hunks {
		b.WriteString(formatHunk(h))
	}
	return b.String()
}

func formatHunk(h *Hunk) string {
	var b strings.Builder
	b.WriteString("@@ -")
	b.WriteString(coord(h.OldStart, h.OldLines))
	b.WriteString(" +")
	b.WriteString(coord(h.NewStart, h.NewLines))
	b.WriteString(" @@\n")
	for _, l := range h.Lines {
		b.WriteByte(byte(l.Op))
		b.WriteString(l.Text)
		b.WriteByte('\n')
		if l.NoNewline {
			b.WriteString(noNewlineMarker)
		}
	}
	return b.String()
}

// coord formats a hunk coordinate. Standard unified diff omits the comma
// count when the count is 1, and uses "0,0" for empty ranges.
func coord(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
