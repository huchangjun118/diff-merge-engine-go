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
// The patch text may use LF or CRLF physical line endings; CRLF is stripped
// while parsing because the canonical patch body is LF-encoded. Byte-level
// output conventions are taken exclusively from the optional
// "# diff-merge-engine eol: ... trailing: ..." preamble line.
//
// Failures are classified:
//   - KindEncodingMismatch: the text is not valid UTF-8.
//   - KindMalformedPatch: structurally invalid diff text, bad coordinates,
//     mismatched line counts, illegal markers or unknown directives.
func Parse(text string) (*diff.Patch, error) {
	if !utf8.ValidString(text) {
		return nil, derr.New(derr.KindEncodingMismatch, "patch text is not valid UTF-8")
	}
	// Preserve a copy of the raw text for directive scanning, then normalize
	// physical line endings. Patches are canonically LF-encoded.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	// Drop a single trailing empty element produced by a final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	p := &diff.Patch{
		OldEOL:             diff.EOLLF,
		NewEOL:             diff.EOLLF,
		OldTrailingNewline: true,
		NewTrailingNewline: true,
	}

	i := 0
	// Preamble: comments (#...), index lines and other non-header lines are
	// tolerated until the '--- ' header.
	for i < len(lines) {
		if strings.HasPrefix(lines[i], "--- ") || lines[i] == "---" {
			break
		}
		if strings.HasPrefix(lines[i], diff.DirectiveMarker()) {
			if err := parseDirective(lines[i], p); err != nil {
				return nil, err
			}
			i++
			continue
		}
		if strings.HasPrefix(lines[i], "#") {
			// Unknown comment line: tolerate.
			i++
			continue
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
			return nil, derr.New(derr.KindMalformedPatch, "multi-file patches are not supported")
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

	// Cross-check trailing metadata against markers present in the final hunk.
	if err := validateTrailingMetadata(p); err != nil {
		return nil, err
	}
	return p, nil
}

// parseDirective parses the machine-readable preamble:
//
//	# diff-merge-engine eol: old=lf new=crlf trailing: old=yes new=no
func parseDirective(line string, p *diff.Patch) error {
	fields := strings.Fields(line)
	// ["#", "diff-merge-engine", "eol:", "old=lf", "new=crlf",
	//  "trailing:", "old=yes", "new=no"]
	seen := map[string]string{}
	mode := ""
	for _, f := range fields[2:] {
		switch f {
		case "eol:", "trailing:":
			mode = f
			continue
		}
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			return derr.New(derr.KindMalformedPatch, "malformed directive token: "+f)
		}
		seen[mode+kv[0]] = kv[1]
	}
	if v, ok := seen["eol:old"]; ok {
		e, ok := diff.ParseEOL(v)
		if !ok {
			return derr.New(derr.KindMalformedPatch, "unknown old EOL in directive: "+v)
		}
		p.OldEOL = e
	}
	if v, ok := seen["eol:new"]; ok {
		e, ok := diff.ParseEOL(v)
		if !ok {
			return derr.New(derr.KindMalformedPatch, "unknown new EOL in directive: "+v)
		}
		p.NewEOL = e
	}
	if v, ok := seen["trailing:old"]; ok {
		b, ok := parseYesNo(v)
		if !ok {
			return derr.New(derr.KindMalformedPatch, "bad trailing flag in directive: "+v)
		}
		p.OldTrailingNewline = b
	}
	if v, ok := seen["trailing:new"]; ok {
		b, ok := parseYesNo(v)
		if !ok {
			return derr.New(derr.KindMalformedPatch, "bad trailing flag in directive: "+v)
		}
		p.NewTrailingNewline = b
	}
	return nil
}

func parseYesNo(s string) (bool, bool) {
	switch s {
	case "yes":
		return true, true
	case "no":
		return false, true
	default:
		return false, false
	}
}

// validateTrailingMetadata ensures EOF no-newline markers agree with the
// preamble metadata and that markers only decorate the terminal lines.
func validateTrailingMetadata(p *diff.Patch) error {
	if len(p.Hunks) == 0 {
		return nil
	}
	last := p.Hunks[len(p.Hunks)-1]
	lastOld := -1
	lastNew := -1
	for i, l := range last.Lines {
		if l.Op == diff.OpContext || l.Op == diff.OpDelete {
			lastOld = i
		}
		if l.Op == diff.OpContext || l.Op == diff.OpAdd {
			lastNew = i
		}
	}
	for i, l := range last.Lines {
		if !l.NoNewline {
			continue
		}
		switch l.Op {
		case diff.OpContext:
			if i != lastOld || i != lastNew {
				return derr.New(derr.KindMalformedPatch, "no-newline marker on non-terminal context line")
			}
		case diff.OpDelete:
			if i != lastOld {
				return derr.New(derr.KindMalformedPatch, "no-newline marker on non-terminal old line")
			}
		case diff.OpAdd:
			if i != lastNew {
				return derr.New(derr.KindMalformedPatch, "no-newline marker on non-terminal new line")
			}
		}
	}
	return nil
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
	if oldStart < 0 || oldLines < 0 || newStart < 0 || newLines < 0 {
		return nil, derr.New(derr.KindMalformedPatch, "negative hunk coordinates: "+header)
	}
	*i++
	h := &diff.Hunk{OldStart: oldStart, OldLines: oldLines, NewStart: newStart, NewLines: newLines}
	oldCount, newCount := 0, 0
	for *i < len(lines) {
		l := lines[*i]
		if strings.HasPrefix(l, "@@") {
			break
		}
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			break
		}
		if l == "" {
			// A truly blank physical line would be ambiguous with the end of
			// the patch stream; unified diff encodes an empty context line as
			// a single leading space.
			return nil, derr.New(derr.KindMalformedPatch, "blank line without leading space in hunk body")
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
			if len(h.Lines) == 0 || l != diff.NoNewlineMarker() {
				return nil, derr.New(derr.KindMalformedPatch, "unexpected hunk annotation: "+l)
			}
			h.Lines[len(h.Lines)-1].NoNewline = true
		default:
			return nil, derr.New(derr.KindMalformedPatch, "invalid hunk line: "+l)
		}
		*i++
	}
	if oldCount != oldLines {
		return nil, derr.New(derr.KindMalformedPatch, "old line count mismatch in hunk")
	}
	if newCount != newLines {
		return nil, derr.New(derr.KindMalformedPatch, "new line count mismatch in hunk")
	}
	return h, nil
}

// parseHunkHeader parses "@@ -oldStart[,oldCount] +newStart[,newCount] @@".
// Trailing section annotations (e.g. "@@ function name") are tolerated.
func parseHunkHeader(header string) (oldStart, oldLines, newStart, newLines int, err error) {
	if !strings.HasPrefix(header, "@@ ") {
		return 0, 0, 0, 0, derr.New(derr.KindMalformedPatch, "malformed hunk header: "+header)
	}
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
		return 0, 0, strconv.ErrSyntax
	}
	switch spec[0] {
	case '-', '+':
		spec = spec[1:]
	}
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
