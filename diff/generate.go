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

// DetectEOL reports the dominant line-ending style of content. Mixed content
// is classified by its first line terminator; content without any terminator
// is EOLDefault.
func DetectEOL(content string) EOL {
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		if i > 0 && content[i-1] == '\r' {
			return EOLCRLF
		}
		return EOLLF
	}
	if strings.IndexByte(content, '\r') >= 0 {
		return EOLCRLF
	}
	return EOLDefault
}

// NoFinalNewline reports whether content is non-empty and does not end with a
// line terminator (LF or CR).
func NoFinalNewline(content string) bool {
	if content == "" {
		return false
	}
	last := content[len(content)-1]
	return last != '\n' && last != '\r'
}

// JoinLines joins lines with '\n', appending a trailing newline.
func JoinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// RenderLines serializes lines using the given EOL style. If noFinalNL is
// true (and there is at least one line) the final line has no terminator.
func RenderLines(lines []string, eol EOL, noFinalNL bool) string {
	if len(lines) == 0 {
		return ""
	}
	sep := eol.Sep()
	s := strings.Join(lines, sep)
	if !noFinalNL {
		s += sep
	}
	return s
}

// Generate produces a Patch between oldContent and newContent.
// contextLines controls how many unchanged lines surround each hunk.
func Generate(oldContent, newContent string, contextLines int) *Patch {
	oldLines := SplitLines(oldContent)
	newLines := SplitLines(newContent)
	ops := myers(oldLines, newLines)
	p := buildPatch(oldLines, newLines, ops, contextLines)
	p.OldEOL = DetectEOL(oldContent)
	p.NewEOL = DetectEOL(newContent)
	p.OldNoFinalNL = NoFinalNewline(oldContent)
	p.NewNoFinalNL = NoFinalNewline(newContent)
	markNoNewline(p, oldLines, newLines, contextLines)
	return p
}

// markNoNewline sets the NoNewline flags on the final lines of the last hunk
// when it reaches EOF. When the line sequences are identical but only the
// trailing-newline state differs (a change that otherwise produces an empty
// diff), it synthesizes a one-line hunk expressing that change so it is not
// lost.
func markNoNewline(p *Patch, oldLines, newLines []string, context int) {
	if len(p.Hunks) > 0 {
		h := p.Hunks[len(p.Hunks)-1]
		touchesOld := h.OldStart >= 0 && h.OldStart-1+h.OldLines == len(oldLines)
		touchesNew := h.NewStart >= 0 && h.NewStart-1+h.NewLines == len(newLines)
		if touchesOld && touchesNew {
			// A common final context line whose newline presence changed must
			// be rendered as -/+ so each side can carry its own marker.
			if p.OldNoFinalNL != p.NewNoFinalNL && len(h.Lines) > 0 {
				last := &h.Lines[len(h.Lines)-1]
				if last.Op == OpContext {
					text := last.Text
					*last = Line{Op: OpDelete, Text: text, NoNewline: p.OldNoFinalNL}
					h.Lines = append(h.Lines, Line{Op: OpAdd, Text: text, NoNewline: p.NewNoFinalNL})
					return
				}
			}
		}
		if touchesOld {
			for i := len(h.Lines) - 1; i >= 0; i-- {
				if h.Lines[i].Op == OpContext || h.Lines[i].Op == OpDelete {
					h.Lines[i].NoNewline = p.OldNoFinalNL
					break
				}
			}
		}
		if touchesNew {
			for i := len(h.Lines) - 1; i >= 0; i-- {
				if h.Lines[i].Op == OpContext || h.Lines[i].Op == OpAdd {
					h.Lines[i].NoNewline = p.NewNoFinalNL
					break
				}
			}
		}
		return
	}

	// Identical line sequences: a hunk is only required when the
	// trailing-newline presence changed on non-empty content. Pure EOL
	// style changes are expressed entirely via directives.
	if len(oldLines) == 0 || len(newLines) == 0 {
		return
	}
	if p.OldNoFinalNL == p.NewNoFinalNL {
		return
	}
	last := len(oldLines) - 1
	lead := context
	if lead > last {
		lead = last
	}
	if lead < 0 {
		lead = 0
	}
	h := &Hunk{
		OldStart: last - lead + 1,
		OldLines: lead + 1,
		NewStart: last - lead + 1,
		NewLines: lead + 1,
	}
	for i := last - lead; i < last; i++ {
		h.Lines = append(h.Lines, Line{Op: OpContext, Text: oldLines[i]})
	}
	h.Lines = append(h.Lines,
		Line{Op: OpDelete, Text: oldLines[last], NoNewline: p.OldNoFinalNL},
		Line{Op: OpAdd, Text: newLines[last], NoNewline: p.NewNoFinalNL},
	)
	p.Hunks = append(p.Hunks, h)
}

// buildPatch groups the edit script into hunks with context.
func buildPatch(oldLines, newLines []string, ops []Op, context int) *Patch {
	if len(ops) == 0 {
		return &Patch{}
	}

	// First, materialize the full line sequence with ops.
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
		return &Patch{}
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

	patch := &Patch{}
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
		// Count old/new lines and find starts.
		oldCount, newCount := 0, 0
		oldStart, newStart := 0, 0
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
		// Compute 1-based starts: the line number of the first old/new line in hunk.
		// Walk seq[0..start) to count preceding old/new lines.
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
		oldStart = preOld + 1
		newStart = preNew + 1
		// Special case: hunk with zero old lines (pure addition) -> start is preOld+1?
		// Unified diff convention: if oldCount==0, oldStart is the line after which insertion happens,
		// but stored as preOld+1? Actually git uses: for pure add, old start = line number before insertion + 1?
		// Let's use standard: oldStart = preOld+1, but if oldCount==0 and preOld==0, oldStart=0.
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
		patch.Hunks = append(patch.Hunks, hunk)
	}
	return patch
}
