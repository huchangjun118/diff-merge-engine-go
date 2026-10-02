package diff

import (
	"fmt"
	"strings"
)

// Format renders the patch as a portable unified diff.
//
// The output uses only '--- '/'+++ ' file headers and '@@' hunk headers;
// it omits timestamps, index hashes and file modes so the same patch text
// applies identically in different environments.
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
