// Package patch parses, applies and reverses unified diff patches.
package patch

import (
	"strconv"
	"strings"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// Parse reads a unified diff and returns a portable Patch.
// It accepts patches with or without timestamps/index lines and normalizes
// line endings. Malformed hunks produce a KindMalformedPatch error.
func Parse(text string) (*diff.Patch, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	// Drop a single trailing empty element produced by a final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	p := &diff.Patch{}
	i := 0
	// Skip leading index/other lines until we find --- header.
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

	for i < len(lines) {
		line := lines[i]
		if line == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			// Could be a second file in a multi-file patch; we only support one file.
			i++
			continue
		}
		if !strings.HasPrefix(line, "@@") {
			return nil, derr.New(derr.KindMalformedPatch, "expected hunk header, got: "+line)
		}
		h, err := parseHunk(lines, &i)
		if err != nil {
			return nil, err
		}
		p.Hunks = append(p.Hunks, h)
	}
	return p, nil
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
	for *i < len(lines) {
		l := lines[*i]
		if l == "" {
			// A blank line inside a hunk is a context line with empty text.
			l = " "
		}
		if strings.HasPrefix(l, "@@") {
			break
		}
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			break
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
		case '\\':
			// "\ No newline at end of file" marker; skip.
		default:
			return nil, derr.New(derr.KindMalformedPatch, "invalid hunk line: "+l)
		}
		*i++
	}
	// Validate counts match the header (header counts may be omitted/1).
	if oldCount != oldLines || newCount != newLines {
		// Some patches omit counts; recompute tolerance: only fail if header
		// declared a count that disagrees.
		if oldLines != 0 && oldCount != oldLines {
			return nil, derr.New(derr.KindMalformedPatch, "old line count mismatch in hunk")
		}
		if newLines != 0 && newCount != newLines {
			return nil, derr.New(derr.KindMalformedPatch, "new line count mismatch in hunk")
		}
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
	spec = strings.TrimPrefix(spec, "-")
	spec = strings.TrimPrefix(spec, "+")
	if idx := strings.IndexByte(spec, ','); idx >= 0 {
		start, err = atoi(spec[:idx])
		if err != nil {
			return 0, 0, err
		}
		count, err = atoi(spec[idx+1:])
		if err != nil {
			return 0, 0, err
		}
		return start, count, nil
	}
	start, err = atoi(spec)
	if err != nil {
		return 0, 0, err
	}
	return start, 1, nil
}

func atoi(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}
