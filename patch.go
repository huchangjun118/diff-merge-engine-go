package diffmerge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const defaultContext = 3

// HunkLineKind identifies the role of one physical line inside a hunk body.
type HunkLineKind int

const (
	HunkContext HunkLineKind = iota
	HunkOld
	HunkNew
)

// HunkLine is one line of a hunk body. EOL describes the terminator of the
// represented content line; a missing EOL is represented explicitly so that
// patches do not depend on the platform-native line ending.
type HunkLine struct {
	Kind HunkLineKind
	Line Line
}

// Hunk is one contiguous change with surrounding context.
type Hunk struct {
	OldStart int // 1-based start of the old-side range
	OldLines int // number of old-side lines
	NewStart int // 1-based start of the new-side range
	NewLines int // number of new-side lines
	Body     []HunkLine
}

// Patch is a portable, unambiguous representation of one file's changes.
//
// Wire format is a strict, normalised unified diff:
//   - "--- old" / "+++ new" header lines
//   - "@@ -a,b +c,d @@" hunk headers
//   - body lines prefixed by ' ', '-' or '+', followed by the exact content
//     line; LF is used on the wire regardless of the platform
//   - a content line that had no terminator is followed by a "\ No newline
//     at end of file" marker line
//
// CRLF/CR terminators inside content are carried verbatim after the prefix.
type Patch struct {
	OldName string
	NewName string
	Hunks   []*Hunk
}

// PatchOptions controls patch generation.
type PatchOptions struct {
	OldName      string
	NewName      string
	Context      int // number of context lines; 0 selects a default (3)
	Compare      LineCompareOptions
	MaxHunkBytes int64 // 0 = unlimited
}

const noNewlineMarker = `\ No newline at end of file`

// NewPatch builds a Patch from two line sets and an edit script.
func NewPatch(old, newer RandomLines, edits []Edit, opts PatchOptions) (*Patch, error) {
	ctxLines := opts.Context
	if ctxLines <= 0 {
		ctxLines = defaultContext
	}
	p := &Patch{OldName: opts.OldName, NewName: opts.NewName}
	nOld, nNew := old.N(), newer.N()

	type hunkRange struct{ a0, a1, b0, b1 int } // half-open, 0-based
	var ranges []hunkRange
	i := 0
	for i < len(edits) {
		if edits[i].Op == EditEqual {
			i++
			continue
		}
		j := i
		// collect consecutive non-equal edits plus equals short enough to merge
		for j < len(edits) {
			if edits[j].Op != EditEqual {
				j++
				continue
			}
			if j+1 < len(edits) && edits[j].Count <= 2*ctxLines {
				j++ // bridging context between changes
				continue
			}
			break
		}
		first, last := edits[i], edits[j-1]
		a0 := first.AStart
		b0 := first.BStart
		var a1, b1 int
		switch last.Op {
		case EditEqual:
			a1, b1 = last.AStart+last.Count, last.BStart+last.Count
		case EditDelete:
			a1, b1 = last.AStart+last.Count, last.BStart
		case EditInsert:
			a1, b1 = last.AStart, last.BStart+last.Count
		}
		// grow context, but never overlap the previous hunk
		prevAEnd, prevBEnd := 0, 0
		if n := len(ranges); n > 0 {
			prevAEnd, prevBEnd = ranges[n-1].a1, ranges[n-1].b1
		}
		lo := ctxLines
		if a0-lo < prevAEnd || b0-lo < prevBEnd {
			lo = minInt(a0-prevAEnd, b0-prevBEnd)
			if lo < 0 {
				lo = 0
			}
		}
		hi := ctxLines
		if a1+hi > nOld {
			hi = nOld - a1
		}
		if b1+hi > nNew {
			hi = minInt(hi, nNew-b1)
		}
		ranges = append(ranges, hunkRange{a0 - lo, a1 + hi, b0 - lo, b1 + hi})
		i = j
	}

	for _, r := range ranges {
		h := &Hunk{
			OldStart: r.a0 + 1,
			OldLines: r.a1 - r.a0,
			NewStart: r.b0 + 1,
			NewLines: r.b1 - r.b0,
		}
		ai, bi := r.a0, r.b0
		for ai < r.a1 || bi < r.b1 {
			switch {
			case ai < r.a1 && bi < r.b1 && lineAtEquals(old, ai, newer, bi):
				l, err := old.Line(ai)
				if err != nil {
					return nil, err
				}
				h.Body = append(h.Body, HunkLine{Kind: HunkContext, Line: l})
				ai++
				bi++
			case ai < r.a1:
				l, err := old.Line(ai)
				if err != nil {
					return nil, err
				}
				h.Body = append(h.Body, HunkLine{Kind: HunkOld, Line: l})
				ai++
			default:
				l, err := newer.Line(bi)
				if err != nil {
					return nil, err
				}
				h.Body = append(h.Body, HunkLine{Kind: HunkNew, Line: l})
				bi++
			}
			if opts.MaxHunkBytes > 0 && int64(len(h.Body)) > opts.MaxHunkBytes {
				return nil, &LimitError{What: "hunk size", Limit: opts.MaxHunkBytes}
			}
		}
		p.Hunks = append(p.Hunks, h)
	}
	return p, nil
}

func lineAtEquals(a RandomLines, ai int, b RandomLines, bi int) bool {
	return equalLines(a, b, ai, bi)
}

func minInt(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// WriteTo writes the normalised wire representation.
func (p *Patch) WriteTo(w io.Writer) (int64, error) {
	bw := bufio.NewWriter(w)
	write := func(s string) error {
		_, err := bw.WriteString(s)
		return err
	}
	if err := write("--- " + fileNameOrEmpty(p.OldName) + "\n"); err != nil {
		return 0, err
	}
	if err := write("+++ " + fileNameOrEmpty(p.NewName) + "\n"); err != nil {
		return 0, err
	}
	for _, h := range p.Hunks {
		if err := write(fmt.Sprintf("@@ -%s +%s @@\n", hunkRange(h.OldStart, h.OldLines), hunkRange(h.NewStart, h.NewLines))); err != nil {
			return 0, err
		}
		for _, hl := range h.Body {
			var prefix string
			switch hl.Kind {
			case HunkContext:
				prefix = " "
			case HunkOld:
				prefix = "-"
			case HunkNew:
				prefix = "+"
			}
			if err := write(prefix + hl.Line.Text + eolWire(hl.Line.EOL)); err != nil {
				return 0, err
			}
			if !hl.Line.HasEOL() {
				if err := write("\n" + noNewlineMarker + "\n"); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return 0, nil
}

// eolWire returns the terminator bytes carried after the +/-/space prefix.
// EOLUnix itself is the physical wire newline; CRLF content keeps its CR so
// it round-trips exactly.
func eolWire(eol string) string {
	switch eol {
	case "":
		return ""
	case EOLUnix:
		return "\n"
	case EOLCRLF:
		return "\r\n"
	default:
		return eol
	}
}

func fileNameOrEmpty(name string) string {
	if name == "" {
		return "file"
	}
	return name
}

func hunkRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(count)
}

// Encode returns the normalised wire bytes.
func (p *Patch) Encode() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := p.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Reverse returns a patch that turns the new content back into the old
// content. The receiver is not modified.
func (p *Patch) Reverse() *Patch {
	q := &Patch{OldName: p.NewName, NewName: p.OldName}
	for _, h := range p.Hunks {
		rh := &Hunk{
			OldStart: h.NewStart, OldLines: h.NewLines,
			NewStart: h.OldStart, NewLines: h.OldLines,
			Body: make([]HunkLine, len(h.Body)),
		}
		for i, hl := range h.Body {
			kind := hl.Kind
			switch kind {
			case HunkOld:
				kind = HunkNew
			case HunkNew:
				kind = HunkOld
			}
			rh.Body[i] = HunkLine{Kind: kind, Line: hl.Line}
		}
		q.Hunks = append(q.Hunks, rh)
	}
	return q
}

// clone returns a deep copy.
func (p *Patch) clone() *Patch {
	q := &Patch{OldName: p.OldName, NewName: p.NewName, Hunks: make([]*Hunk, len(p.Hunks))}
	for i, h := range p.Hunks {
		nh := *h
		nh.Body = append([]HunkLine(nil), h.Body...)
		q.Hunks[i] = &nh
	}
	return q
}

// ID returns a stable content identifier independent of file names: patches
// carrying identical content changes have identical IDs in every environment.
func (p *Patch) ID() string {
	h := sha256.New()
	for _, hnk := range p.Hunks {
		fmt.Fprintf(h, "%d,%d;", hnk.OldLines, hnk.NewLines)
		for _, hl := range hnk.Body {
			var tag byte
			switch hl.Kind {
			case HunkContext:
				tag = 'c'
			case HunkOld:
				tag = '-'
			case HunkNew:
				tag = '+'
			}
			h.Write([]byte{tag})
			h.Write(hl.Line.Bytes())
			h.Write([]byte{0})
		}
		h.Write([]byte{'|'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// GeneratePatch is a convenience that diffs two streams and returns a patch.
func GeneratePatch(ctx context.Context, oldSrc, newSrc LineSource, opts PatchOptions) (*Patch, error) {
	oldLines, err := ReadAllLines(ctx, oldSrc)
	if err != nil {
		return nil, err
	}
	newLines, err := ReadAllLines(ctx, newSrc)
	if err != nil {
		return nil, err
	}
	return GeneratePatchLines(oldLines, newLines, opts)
}

// GeneratePatchLines is a convenience for in-memory inputs.
func GeneratePatchLines(old, new []Line, opts PatchOptions) (*Patch, error) {
	o := NewMemoryLines(old, opts.Compare)
	n := NewMemoryLines(new, opts.Compare)
	edits, err := ScriptEdits(context.Background(), o, n)
	if err != nil {
		return nil, err
	}
	return NewPatch(o, n, edits, opts)
}

// ParsePatch reads and validates a unified-diff patch from r. Malformed
// structure yields *MalformedPatchError.
func ParsePatch(r io.Reader) (*Patch, error) {
	return parsePatch(r, false)
}

// ParsePatchUTF8 behaves like ParsePatch but additionally rejects content that
// is not valid UTF-8 with *EncodingError, keeping encoding failures distinct
// from structurally malformed patches.
func ParsePatchUTF8(r io.Reader) (*Patch, error) {
	return parsePatch(r, true)
}

func parsePatch(r io.Reader, requireUTF8 bool) (*Patch, error) {
	defer func() {
		// validation hook after successful parse is handled below
	}()
	pr := &pReader{br: bufio.NewReaderSize(r, defaultReadBuffer), requireUTF8: requireUTF8}
	p := &Patch{}

	// --- header ---
	oldLine, err := pr.readLine()
	if err != nil {
		return nil, &MalformedPatchError{Msg: "empty or unreadable patch"}
	}
	if !strings.HasPrefix(oldLine.text, "--- ") {
		return nil, &MalformedPatchError{Line: pr.lineNo, Msg: "expected --- header, got: " + truncate(oldLine.text)}
	}
	p.OldName = parseHeaderName(oldLine.text[4:])

	newLine, err := pr.readLine()
	if err != nil && !errorsIsEOF(err) {
		return nil, &MalformedPatchError{Msg: "missing +++ header"}
	}
	if !strings.HasPrefix(newLine.text, "+++ ") {
		return nil, &MalformedPatchError{Line: pr.lineNo, Msg: "expected +++ header, got: " + truncate(newLine.text)}
	}
	p.NewName = parseHeaderName(newLine.text[4:])

	// --- hunks ---
	for {
		hdrLine, err := pr.readLine()
		if errorsIsEOF(err) && hdrLine.text == "" {
			break
		}
		if err != nil && !errorsIsEOF(err) {
			return nil, err
		}
		hdrLineNo := pr.lineNo
		if !strings.HasPrefix(hdrLine.text, "@@") {
			return nil, &MalformedPatchError{Line: hdrLineNo, Msg: "expected hunk header @@, got: " + truncate(hdrLine.text)}
		}
		h, err := parseHunkHeader(hdrLine.text, hdrLineNo)
		if err != nil {
			return nil, err
		}
		if hdrLine.eol == "" {
			return nil, &MalformedPatchError{Line: hdrLineNo, Msg: "unterminated hunk header"}
		}
		body, err := parseHunkBody(pr, h)
		if err != nil {
			return nil, err
		}
		h.Body = body
		if err := validateHunk(h, hdrLineNo); err != nil {
			return nil, err
		}
		p.Hunks = append(p.Hunks, h)
	}
	if len(p.Hunks) == 0 {
		return nil, &MalformedPatchError{Msg: "patch contains no hunks"}
	}
	return p, nil
}

type physLine struct {
	text string // physical line content without its terminator
	eol  string // "\n", "\r\n" (wire normalised) or "" at EOF
}

type pReader struct {
	br          *bufio.Reader
	lineNo      int
	requireUTF8 bool
}

func (pr *pReader) readLine() (physLine, error) {
	raw, err := pr.br.ReadString('\n')
	if err != nil && !errorsIsEOF(err) {
		return physLine{}, err
	}
	if len(raw) == 0 && errorsIsEOF(err) {
		return physLine{}, io.EOF
	}
	pr.lineNo++
	if pr.requireUTF8 && !utf8.ValidString(strings.TrimSuffix(strings.TrimSuffix(raw, "\r"), "\n")) {
		return physLine{}, &EncodingError{Offset: int64(pr.lineNo), Msg: "patch content is not valid UTF-8"}
	}
	if strings.HasSuffix(raw, "\n") {
		body := raw[:len(raw)-1]
		if strings.HasSuffix(body, "\r") {
			return physLine{text: body[:len(body)-1], eol: EOLCRLF}, nil
		}
		return physLine{text: body, eol: EOLUnix}, nil
	}
	return physLine{text: raw, eol: ""}, err
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

func parseHeaderName(rest string) string {
	if i := strings.IndexByte(rest, '\t'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

func errorsIsEOF(err error) bool { return err == io.EOF }

// parseHunkHeader parses "@@ -a,b +c,d @@" (optional trailing section).
func parseHunkHeader(line string, lineNo int) (*Hunk, error) {
	oldS, oldC, newS, newC, ok := parseRangePair(line)
	if !ok {
		return nil, &MalformedPatchError{Line: lineNo, Msg: "bad hunk header: " + truncate(line)}
	}
	return &Hunk{OldStart: oldS, OldLines: oldC, NewStart: newS, NewLines: newC}, nil
}

func parseRangePair(spec string) (oldS, oldC, newS, newC int, ok bool) {
	end := strings.Index(spec[2:], "@@")
	if end < 0 {
		return 0, 0, 0, 0, false
	}
	fields := strings.Fields(spec[2 : 2+end])
	if len(fields) != 2 || fields[0][0] != '-' || fields[1][0] != '+' {
		return 0, 0, 0, 0, false
	}
	o, ok1 := parseRange(fields[0][1:])
	n, ok2 := parseRange(fields[1][1:])
	if !ok1 || !ok2 {
		return 0, 0, 0, 0, false
	}
	return o[0], o[1], n[0], n[1], true
}

func parseRange(s string) ([2]int, bool) {
	var out [2]int
	if i := strings.IndexByte(s, ','); i >= 0 {
		a, err1 := strconv.Atoi(s[:i])
		b, err2 := strconv.Atoi(s[i+1:])
		if err1 != nil || err2 != nil {
			return out, false
		}
		return [2]int{a, b}, true
	}
	a, err := strconv.Atoi(s)
	if err != nil {
		return out, false
	}
	return [2]int{a, 1}, true
}

// parseHunkBody reads exactly OldLines+NewLines logical hunk entries.
func parseHunkBody(pr *pReader, h *Hunk) ([]HunkLine, error) {
	var body []HunkLine
	needOld, needNew := h.OldLines, h.NewLines
	for needOld > 0 || needNew > 0 {
		pl, err := pr.readLine()
		if errorsIsEOF(err) && pl.text == "" {
			return nil, &MalformedPatchError{Line: pr.lineNo + 1, Msg: "hunk body truncated"}
		}
		if err != nil && !errorsIsEOF(err) {
			return nil, err
		}
		lineNo := pr.lineNo
		if pl.eol == "" && !errorsIsEOF(err) {
			return nil, err
		}
		if len(pl.text) == 0 {
			return nil, &MalformedPatchError{Line: lineNo, Msg: "hunk body line missing prefix"}
		}
		prefix, text := pl.text[0], pl.text[1:]
		if prefix != ' ' && prefix != '-' && prefix != '+' {
			return nil, &MalformedPatchError{Line: lineNo, Msg: "hunk body line must start with space, '-' or '+'"}
		}
		line := Line{Text: text, EOL: pl.eol}
		var hl HunkLine
		switch prefix {
		case ' ':
			hl = HunkLine{HunkContext, line}
			needOld--
			needNew--
		case '-':
			hl = HunkLine{HunkOld, line}
			needOld--
		case '+':
			hl = HunkLine{HunkNew, line}
			needNew--
		}
		body = append(body, hl)
		// A missing line terminator is announced by a marker line beginning
		// with '\'. It must refer to the line just read.
		if pl.eol != "" {
			peek, _ := pr.br.Peek(1)
			if len(peek) > 0 && peek[0] == '\\' {
				marker, mErr := pr.readLine()
				if mErr != nil && !errorsIsEOF(mErr) {
					return nil, mErr
				}
				if marker.text != noNewlineMarker {
					return nil, &MalformedPatchError{Line: pr.lineNo, Msg: "invalid no-newline marker: " + truncate(marker.text)}
				}
				body[len(body)-1].Line.EOL = ""
			}
		}
	}
	return body, nil
}

func validateHunk(h *Hunk, lineNo int) error {
	var oc, nc int
	prevKind := HunkContext
	for i, hl := range h.Body {
		switch hl.Kind {
		case HunkContext:
			oc++
			nc++
		case HunkOld:
			oc++
		case HunkNew:
			nc++
		}
		// Reject unnormalised order such as "+a -b" that would make fuzzy
		// application ambiguous.
		if i > 0 && prevKind == HunkNew && hl.Kind == HunkOld {
			return &MalformedPatchError{Line: lineNo, Msg: "'-' line after '+' line (unnormalised hunk)"}
		}
		prevKind = hl.Kind
	}
	if oc != h.OldLines || nc != h.NewLines {
		return &MalformedPatchError{Line: lineNo, Msg: fmt.Sprintf("hunk header counts (-%d,+%d) disagree with body (-%d,+%d)", h.OldLines, h.NewLines, oc, nc)}
	}
	if h.OldStart < 0 || h.NewStart < 0 || h.OldLines < 0 || h.NewLines < 0 {
		return &MalformedPatchError{Line: lineNo, Msg: "negative hunk range"}
	}
	return nil
}
