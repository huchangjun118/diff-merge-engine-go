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
func JoinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// Generate produces a Patch between oldContent and newContent.
// contextLines controls how many unchanged lines surround each hunk.
func Generate(oldContent, newContent string, contextLines int) *Patch {
	oldLines := SplitLines(oldContent)
	newLines := SplitLines(newContent)
	ops := myers(oldLines, newLines)
	return buildPatch(oldLines, newLines, ops, contextLines)
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
				hunk.Lines = append(hunk.Lines, Line{OpContext, e.text})
				oldCount++
				newCount++
			case OpAdd:
				hunk.Lines = append(hunk.Lines, Line{OpAdd, e.text})
				newCount++
			case OpDelete:
				hunk.Lines = append(hunk.Lines, Line{OpDelete, e.text})
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
