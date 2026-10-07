// Package patch parses, applies and reverses unified diff patches.
package patch

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// Parse reads a unified diff and returns a portable Patch.
//
// It accepts patches with or without timestamps/index lines. The patch text
// itself may use LF or CRLF separators (its own separators are normalized),
// but the line-ending style of the referenced content is taken exclusively
// from the '# eol-old'/'# eol-new' directives (default LF).
//
// Malformed patch text, invalid UTF-8 and hunk counts that disagree with the
// hunk header produce distinguishable errors and never return a partial
// patch.
func Parse(text string) (*diff.Patch, error) {
	if !utf8.ValidString(text) {
		return nil, derr.New(derr.KindEncodingMismatch, "patch text is not valid UTF-8")
	}
	lines := splitPatchLines(text)

	p := &diff.Patch{}
	i := 0
	// Skip leading index/other lines until we find the --- header.
	for i < len(lines) {
		if strings.HasPrefix(lines[i], "--- ") || lines[i] == "---" {
			break
		}
		i++
	}
	if i >= len(lines) {
		return nil, derr.New(derr.KindMalformedPatch, "patch has no '--- ' header")
	}
	p.OldFile = parseFileName(lines[i])
	i++
	if i >= len(lines) || !strings.HasPrefix(lines[i], "+++ ") {
		return nil, derr.New(derr.KindMalformedPatch, "patch has no '+++ ' header")
	}
	p.NewFile = parseFileName(lines[i])
	i++

	// Parse directive lines (blank lines tolerated).
	for i < len(lines) {
		line := lines[i]
		if line == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, diff.DirectiveEOLOld) {
			eol, err := parseEOLValue(strings.TrimSpace(strings.TrimPrefix(line, diff.DirectiveEOLOld)))
			if err != nil {
				return nil, err
			}
			p.OldEOL = eol
			i++
			continue
		}
		if strings.HasPrefix(line, diff.DirectiveEOLNew) {
			eol, err := parseEOLValue(strings.TrimSpace(strings.TrimPrefix(line, diff.DirectiveEOLNew)))
			if err != nil {
				return nil, err
			}
			p.NewEOL = eol
			i++
			continue
		}
		if strings.HasPrefix(line, "#") {
			return nil, derr.New(derr.KindMalformedPatch, "unknown directive: "+line)
		}
		break
	}

	sawHunk := false
	for i < len(lines) {
		line := lines[i]
		if line == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			return nil, derr.New(derr.KindMalformedPatch, "multi-file patches are not supported")
		}
		if strings.HasPrefix(line, "#") {
			return nil, derr.New(derr.KindMalformedPatch, "directive outside the header section: "+line)
		}
		if !strings.HasPrefix(line, "@@") {
			return nil, derr.New(derr.KindMalformedPatch, "expected hunk header, got: "+line)
		}
		h, err := parseHunk(lines, &i)
		if err != nil {
			return nil, err
		}
		p.Hunks = append(p.Hunks, h)
		sawHunk = true
	}

	// Derive trailing-newline metadata from the markers of the final hunk.
	if sawHunk {
		last := p.Hunks[len(p.Hunks)-1]
		if last.OldStart >= 0 && last.OldStart-1+last.OldLines >= 0 {
			for k := len(last.Lines) - 1; k >= 0; k-- {
				if last.Lines[k].Op == diff.OpContext || last.Lines[k].Op == diff.OpDelete {
					p.OldNoFinalNL = last.Lines[k].NoNewline
					break
				}
			}
		}
		for k := len(last.Lines) - 1; k >= 0; k-- {
			if last.Lines[k].Op == diff.OpContext || last.Lines[k].Op == diff.OpAdd {
				p.NewNoFinalNL = last.Lines[k].NoNewline
				break
			}
		}
	}
	return p, nil
}

// splitPatchLines splits the patch text, tolerating LF and CRLF patch
// separators and dropping exactly one final empty element.
func splitPatchLines(text string) []string {
	lines := strings.Split(text, "\n")
	for k := range lines {
		lines[k] = strings.TrimSuffix(lines[k], "\r")
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func parseEOLValue(v string) (diff.EOL, error) {
	switch v {
	case "lf":
		return diff.EOLLF, nil
	case "crlf":
		return diff.EOLCRLF, nil
	default:
		return diff.EOLDefault, derr.New(derr.KindMalformedPatch, "unknown eol directive value: "+v)
	}
}

func parseFileName(header string) string {
	// Strip "--- " or "+++ " prefix.
	name := header[4:]
	// Strip optional tab-separated timestamp.
	if idx := strings.IndexByte(name, '\t'); idx >= 0 {
		name = name[:idx]
	}
	name = strings.TrimSpace(name)
	// Strip a/ b/ prefixes commonly used by git.
	name = strings.TrimPrefix(name, "a/")
	name = strings.TrimPrefix(name, "b/")
	return name
}

// parseHunk parses one hunk starting at lines[i] (the @@ line).
// On success i is advanced past the hunk body.
func parseHunk(lines []string, i *int) (*diff.Hunk, error) {
	header := lines[*i]
	oldStart, oldLines, newStart, newLines, err := parseHunkHeader(header)
	if err != nil {
		return nil, err
	}
	*i++
	h := &diff.Hunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}
	oldCount, newCount := 0, 0
	noNL := false
	for *i < len(lines) {
		l := lines[*i]
		if strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			break
		}
		if l == "\\ No newline at end of file" {
			if len(h.Lines) == 0 {
				return nil, derr.New(derr.KindMalformedPatch, "no-newline marker without a preceding hunk line")
			}
			if noNL {
				return nil, derr.New(derr.KindMalformedPatch, "duplicate no-newline marker")
			}
			h.Lines[len(h.Lines)-1].NoNewline = true
			noNL = true
			*i++
			continue
		}
		noNL = false
		if l == "" {
			// A blank line inside a hunk is a context line with empty text.
			l = " "
		}
		switch l[0] {
		case ' ':
			h.Lines = append(h.Lines, diff.Line{Op: diff.OpContext, Text: l[1:]})
			oldCount++
			newCount++
		case '+':
			h.Lines = append(h.Lines, diff.Line{Op: diff.OpAdd, Text: l[1:]})
			newCount++
		case '-':
			h.Lines = append(h.Lines, diff.Line{Op: diff.OpDelete, Text: l[1:]})
			oldCount++
		default:
			return nil, derr.New(derr.KindMalformedPatch, "invalid hunk line: "+l)
		}
		*i++
	}
	if oldCount != oldLines || newCount != newLines {
		return nil, derr.New(derr.KindMalformedPatch,
			"hunk line count mismatch: header says old="+itoa(oldLines)+",new="+itoa(newLines)+
				" but body has old="+itoa(oldCount)+",new="+itoa(newCount))
	}
	return h, nil
}

// parseHunkHeader parses "@@ -oldStart[,oldCount] +newStart[,newCount] @@".
func parseHunkHeader(header string) (oldStart, oldLines, newStart, newLines int, err error) {
	// Find the part between @@ and @@.
	rest := strings.TrimPrefix(header, "@@")
	end := strings.Index(rest, "@@")
	if end < 0 {
		return 0, 0, 0, 0, derr.New(derr.KindMalformedPatch, "unterminated hunk header: "+header)
	}
	rest = rest[:end]
	parts := strings.Fields(rest)
	if len(parts) < 2 {
		return 0, 0, 0, 0, derr.New(derr.KindMalformedPatch, "malformed hunk header: "+header)
	}
	oldStart, oldLines, err = parseRange(parts[0])
	if err != nil {
		return 0, 0, 0, 0, derr.Wrap(derr.KindMalformedPatch, "bad old range", err)
	}
	newStart, newLines, err = parseRange(parts[1])
	if err != nil {
		return 0, 0, 0, 0, derr.Wrap(derr.KindMalformedPatch, "bad new range", err)
	}
	return oldStart, oldLines, newStart, newLines, nil
}

func parseRange(spec string) (start, count int, err error) {
	if spec == "" {
		return 0, 0, derr.New(derr.KindMalformedPatch, "empty range in hunk header")
	}
	neg := false
	switch spec[0] {
	case '-', '+':
		spec = spec[1:]
	}
	if strings.HasPrefix(spec, "-") {
		neg = true
		spec = strings.TrimPrefix(spec, "-")
	}
	if idx := strings.IndexByte(spec, ','); idx >= 0 {
		start, err = atoi(spec[:idx])
		if err != nil {
			return 0, 0, derr.Wrap(derr.KindMalformedPatch, "bad range start", err)
		}
		count, err = atoi(spec[idx+1:])
		if err != nil {
			return 0, 0, derr.Wrap(derr.KindMalformedPatch, "bad range count", err)
		}
	} else {
		start, err = atoi(spec)
		if err != nil {
			return 0, 0, derr.Wrap(derr.KindMalformedPatch, "bad range start", err)
		}
		count = 1
	}
	if neg || start < 0 || count < 0 {
		return 0, 0, derr.New(derr.KindMalformedPatch, "negative range in hunk header")
	}
	return start, count, nil
}

func atoi(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}
