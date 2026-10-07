package diff

import (
	"strings"
)

// SplitLines splits content into lines, normalizing all line endings to '\n'.
// A trailing newline does not produce an extra empty line.
func SplitLines(content string) []string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// JoinLines joins lines with '\n', appending a trailing newline.
// Deprecated: use Render with explicit EOL/trailing state for byte-exact
// output. Retained for callers that operate on LF-only content.
func JoinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// Render joins lines using eol and appends a final terminator only when
// trailing is true. An empty line slice always renders as the empty string.
func Render(lines []string, eol EOL, trailing bool) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, eol.Terminator())
	if trailing {
		s += eol.Terminator()
	}
	return s
}

// fileInfo describes the line structure and byte-level text conventions of
// one file version.
type fileInfo struct {
	lines    []string
	eol      EOL
	trailing bool
}

// inspect decomposes content into logical lines and records the line-ending
// style and the end-of-file newline state. CRLF anywhere selects EOLCRLF; a
// lone CR is treated as a terminator for line splitting but does not select
// EOLCRLF by itself.
func inspect(content string) fileInfo {
	fi := fileInfo{eol: EOLLF, trailing: false}
	if content == "" {
		return fi
	}
	if strings.Contains(content, "\r\n") {
		fi.eol = EOLCRLF
	}
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	fi.trailing = strings.HasSuffix(norm, "\n")
	fi.lines = SplitLines(content)
	return fi
}

// Generate produces a Patch between oldContent and newContent.
// contextLines controls how many unchanged lines surround each hunk.
//
// The patch fully captures the byte-level text conventions of both versions:
// their EOL styles (OldEOL/NewEOL) and whether the final line is terminated
// (OldTrailingNewline/NewTrailingNewline). A change limited to the trailing
// newline produces a dedicated final hunk using the standard
// "\ No newline at end of file" markers; a change limited to the EOL style
// produces a metadata-only patch (no hunks).
func Generate(oldContent, newContent string, contextLines int) *Patch {
	oldFI := inspect(oldContent)
	newFI := inspect(newContent)

	p := &Patch{
		OldEOL:             oldFI.eol,
		NewEOL:             newFI.eol,
		OldTrailingNewline: oldFI.trailing,
		NewTrailingNewline: newFI.trailing,
	}

	ops := myers(oldFI.lines, newFI.lines)
	p.Hunks = buildHunks(oldFI.lines, newFI.lines, ops, contextLines)

	oldNN := !oldFI.trailing && len(oldFI.lines) > 0
	newNN := !newFI.trailing && len(newFI.lines) > 0

	if len(p.Hunks) == 0 {
		// Identical logical content. A trailing-newline-only change is the
		// sole content-level difference worth a hunk.
		if oldNN != newNN && len(oldFI.lines) > 0 {
			p.Hunks = append(p.Hunks, eofHunk(oldFI.lines, newFI.lines, contextLines, oldNN, newNN))
		}
		return p
	}

	last := p.Hunks[len(p.Hunks)-1]
	if hunkCoversEOF(last, len(oldFI.lines), len(newFI.lines)) {
		finalizeEOF(last, oldNN, newNN)
	} else if oldNN != newNN {
		p.Hunks = append(p.Hunks, eofHunk(oldFI.lines, newFI.lines, contextLines, oldNN, newNN))
	}
	return p
}

// hunkCoversEOF reports whether h reaches the final old/new line as given by
// the declared counts.
func hunkCoversEOF(h *Hunk, nOld, nNew int) bool {
	oldEnd := h.OldStart - 1 + h.OldLines
	newEnd := h.NewStart - 1 + h.NewLines
	if h.OldLines == 0 {
		oldEnd = h.OldStart // pure insertion point
	}
	if h.NewLines == 0 {
		newEnd = h.NewStart
	}
	return oldEnd == nOld && newEnd == nNew
}

// eofHunk builds a trailing-newline-only hunk over the identical tail of both
// versions: up to context shared lines followed by a delete/add pair of the
// final line carrying per-side no-newline flags.
func eofHunk(oldLines, newLines []string, context int, oldNN, newNN bool) *Hunk {
	n := len(oldLines)
	if n == 0 {
		return nil
	}
	tail := context
	if tail > n-1 {
		tail = n - 1
	}
	h := &Hunk{}
	for i := n - 1 - tail; i < n-1; i++ {
		h.Lines = append(h.Lines, Line{Op: OpContext, Text: oldLines[i]})
	}
	last := oldLines[n-1]
	h.Lines = append(h.Lines,
		Line{Op: OpDelete, Text: last, NoNewline: oldNN},
		Line{Op: OpAdd, Text: last, NoNewline: newNN},
	)
	h.OldLines = tail + 1
	h.NewLines = tail + 1
	h.OldStart = n - tail
	h.NewStart = n - tail
	return h
}

// finalizeEOF attaches no-newline flags to the final hunk, which is known to
// reach EOF on both sides. A shared context line is split into a delete/add
// pair whenever the two sides need different flags.
func finalizeEOF(h *Hunk, oldNN, newNN bool) {
	lastOld := -1
	lastNew := -1
	for i, l := range h.Lines {
		if l.Op == OpContext || l.Op == OpDelete {
			lastOld = i
		}
		if l.Op == OpContext || l.Op == OpAdd {
			lastNew = i
		}
	}
	out := make([]Line, 0, len(h.Lines)+2)
	for i, l := range h.Lines {
		isLastOld := i == lastOld
		isLastNew := i == lastNew
		if l.Op == OpContext && (isLastOld || isLastNew) && !(isLastOld && isLastNew && oldNN == newNN) {
			out = append(out, Line{Op: OpDelete, Text: l.Text, NoNewline: isLastOld && oldNN})
			out = append(out, Line{Op: OpAdd, Text: l.Text, NoNewline: isLastNew && newNN})
			continue
		}
		if l.Op == OpDelete && isLastOld {
			l.NoNewline = oldNN
		}
		if l.Op == OpAdd && isLastNew {
			l.NoNewline = newNN
		}
		if l.Op == OpContext && isLastOld && isLastNew {
			l.NoNewline = oldNN
		}
		out = append(out, l)
	}
	h.Lines = out
}

// buildHunks groups the edit script into hunks with context.
func buildHunks(oldLines, newLines []string, ops []Op, context int) []*Hunk {
	if len(ops) == 0 {
		return nil
	}

	// Materialize the full line sequence with ops.
	type entry struct {
		op   Op
		text string
	}
	var seq []entry
	oi, ni := 0, 0
	for _, op := range ops {
		switch op {
		case OpContext:
			seq = append(seq, entry{OpContext, oldLines[oi]})
			oi++
			ni++
		case OpAdd:
			seq = append(seq, entry{OpAdd, newLines[ni]})
			ni++
		case OpDelete:
			seq = append(seq, entry{OpDelete, oldLines[oi]})
			oi++
		}
	}

	// Find indices of changed entries.
	var changed []int
	for i, e := range seq {
		if e.op != OpContext {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	// Group changed entries into hunks separated by gaps larger than 2*context.
	var groups [][]int
	cur := []int{changed[0]}
	for i := 1; i < len(changed); i++ {
		if changed[i]-changed[i-1] <= 2*context+1 {
			cur = append(cur, changed[i])
		} else {
			groups = append(groups, cur)
			cur = []int{changed[i]}
		}
	}
	groups = append(groups, cur)

	var hunks []*Hunk
	for _, g := range groups {
		start := g[0] - context
		if start < 0 {
			start = 0
		}
		end := g[len(g)-1] + context
		if end >= len(seq) {
			end = len(seq) - 1
		}
		hunk := &Hunk{}
		oldCount, newCount := 0, 0
		for i := start; i <= end; i++ {
			e := seq[i]
			switch e.op {
			case OpContext:
				hunk.Lines = append(hunk.Lines, Line{Op: OpContext, Text: e.text})
				oldCount++
				newCount++
			case OpAdd:
				hunk.Lines = append(hunk.Lines, Line{Op: OpAdd, Text: e.text})
				newCount++
			case OpDelete:
				hunk.Lines = append(hunk.Lines, Line{Op: OpDelete, Text: e.text})
				oldCount++
			}
		}
		preOld, preNew := 0, 0
		for i := 0; i < start; i++ {
			switch seq[i].op {
			case OpContext:
				preOld++
				preNew++
			case OpAdd:
				preNew++
			case OpDelete:
				preOld++
			}
		}
		oldStart := preOld + 1
		newStart := preNew + 1
		if oldCount == 0 {
			if preOld == 0 {
				oldStart = 0
			} else {
				oldStart = preOld + 1
			}
		}
		if newCount == 0 {
			if preNew == 0 {
				newStart = 0
			} else {
				newStart = preNew + 1
			}
		}
		hunk.OldStart = oldStart
		hunk.OldLines = oldCount
		hunk.NewStart = newStart
		hunk.NewLines = newCount
		hunks = append(hunks, hunk)
	}
	return hunks
}
