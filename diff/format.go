package diff

import (
	"fmt"
	"strings"
)

// directiveMarker identifies the machine-readable preamble line that carries
// byte-level text conventions ignored by standard unified-diff tools.
const directiveMarker = "# diff-merge-engine"

// noNewlineMarker is the standard unified-diff annotation emitted
// immediately after a hunk line whose file version has no final newline.
const noNewlineMarker = `\ No newline at end of file`

// DirectiveMarker returns the machine-readable preamble prefix that records
// byte-level text conventions.
func DirectiveMarker() string { return directiveMarker }

// NoNewlineMarker returns the standard unified-diff no-newline annotation.
func NoNewlineMarker() string { return noNewlineMarker }

// Format renders the patch as portable unified-diff text.
//
// The output uses only '--- '/'+++ ' file headers and '@@' hunk headers; it
// omits timestamps, index hashes and file modes so the same patch text applies
// identically in different environments.
//
// When either file version deviates from the defaults (LF terminators, final
// line terminated), a single comment-style preamble line records the exact
// conventions:
//
//	# diff-merge-engine eol: old=lf new=crlf trailing: old=yes new=no
//
// The patch text itself is always LF-encoded; the directive is the sole
// authority for the output byte layout. Missing final newlines inside hunks
// use the standard "\ No newline at end of file" marker.
func (p *Patch) Format() string {
	var b strings.Builder

	defaults := p.OldEOL == EOLLF && p.NewEOL == EOLLF && p.OldTrailingNewline && p.NewTrailingNewline
	if !defaults {
		b.WriteString(directiveMarker)
		b.WriteString(" eol: old=" + p.OldEOL.String() + " new=" + p.NewEOL.String())
		b.WriteString(" trailing: old=" + yesno(p.OldTrailingNewline) +
			" new=" + yesno(p.NewTrailingNewline) + "\n")
	}

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

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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
			b.WriteByte('\n')
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
