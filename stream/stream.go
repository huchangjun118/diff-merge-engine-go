// Package stream provides bounded-memory streaming patch application with
// all-or-nothing output semantics.
package stream

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// spoolLimit bounds the in-memory buffer before the result is spilled to a
// temporary file.
const spoolLimit = 1 << 20 // 1 MiB

// ApplyStream applies a patch to base, writing the result to out ONLY after
// the entire application has succeeded.
//
// All-or-nothing guarantee: while the patch is being processed (including
// location failure, malformed input, encoding failure, cancellation or
// timeout) not a single byte is written to out. The result is first built in
// a private spool (memory up to spoolLimit, then a temporary file that is
// removed on failure). The final copy starts only after a context check; if
// the context is cancelled or out fails during that copy the returned error
// is non-nil and the caller must discard out as invalid.
//
// Output byte layout follows the patch: NewEOL determines line terminators
// and the declared trailing-newline state determines the file ending, never
// the base content.
func ApplyStream(ctx context.Context, base io.Reader, out io.Writer, p *diff.Patch, opts patch.ApplyOptions) error {
	spool, err := newSpool()
	if err != nil {
		return err
	}
	defer spool.cleanup()

	if err := runStream(ctx, base, spool, p, opts); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return derr.Wrap(derr.KindAborted, "cancelled before output", err)
	}
	if _, err := spool.seekStart(); err != nil {
		return err
	}
	if _, err := io.Copy(out, spool); err != nil {
		return derr.Wrap(derr.KindAborted, "cannot write output; result is invalid and must be discarded", err)
	}
	return nil
}

// ApplyStreamToFile applies a patch and installs the result at dstPath
// atomically. The destination is either fully replaced by the complete
// result or left completely untouched (and absent if it did not exist) on
// any failure, cancellation or timeout. The output is fsynced and renamed
// from a sibling temporary file.
func ApplyStreamToFile(ctx context.Context, base io.Reader, dstPath string, p *diff.Patch, opts patch.ApplyOptions) error {
	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".apply-*")
	if err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot create destination temp file", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if err := runStream(ctx, base, tmp, p, opts); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return derr.Wrap(derr.KindAborted, "cancelled before commit", err)
	}
	if err := tmp.Sync(); err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot sync output", err)
	}
	if err := tmp.Close(); err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot close output", err)
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot install output", err)
	}
	cleanup = false
	return syncDir(dir)
}

// runStream executes the hunk stream against w. It never returns a partial
// success: on error nothing about the target destination is committed.
func runStream(ctx context.Context, base io.Reader, w io.Writer, p *diff.Patch, opts patch.ApplyOptions) error {
	if err := patch.Validate(p); err != nil {
		return err
	}
	opts = opts.WithDefaults()
	scanner := bufio.NewScanner(base)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 16 MiB max line
	writer := bufio.NewWriter(w)

	eol := p.NewEOL.Terminator()
	trailing := patch.DesiredTrailing(p)

	emit := func(s string) error {
		if _, err := writer.WriteString(s); err != nil {
			return derr.Wrap(derr.KindAborted, "write failed", err)
		}
		return nil
	}
	emitLine := func(s string, terminated bool) error {
		if err := emit(s); err != nil {
			return err
		}
		if terminated {
			return emit(eol)
		}
		return nil
	}

	var buf []string // read-but-not-output lines
	bufStart := 0    // absolute line index of buf[0]
	offset := 0      // cumulative line offset from applied hunks
	atEOF := false

	totalKnown := func() int { return bufStart + len(buf) }

	fill := func(needAbs int) error {
		for !atEOF && totalKnown() < needAbs {
			if err := ctx.Err(); err != nil {
				return derr.Wrap(derr.KindAborted, "cancelled while reading", err)
			}
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return derr.Wrap(derr.KindAborted, "read error", err)
				}
				atEOF = true
				break
			}
			text := scanner.Text()
			if !utf8.ValidString(text) {
				return derr.New(derr.KindEncodingMismatch, "base content is not valid UTF-8")
			}
			buf = append(buf, text)
		}
		return nil
	}

	for hi, h := range p.Hunks {
		if err := ctx.Err(); err != nil {
			return derr.Wrap(derr.KindAborted, "cancelled before hunk "+itoa(hi), err)
		}
		expectedAbs := h.OldStart - 1 + offset // absolute 0-based for normal hunks
		if h.OldLines == 0 {
			// Pure insertion: OldStart is the number of lines preceding the
			// insertion point.
			expectedAbs = h.OldStart + offset
		}
		if err := fill(expectedAbs + opts.MaxOffset + 1); err != nil {
			return err
		}
		expectedRel := expectedAbs - bufStart
		posRel, coreStart, coreEnd, err := locateInBuffer(buf, expectedRel, expectedAbs, bufStart, h, opts)
		if err != nil {
			kind := derr.KindNoMatch
			if e, ok := err.(*derr.Error); ok {
				kind = e.Kind
			}
			if atEOF && rangeExceeds(h, totalKnown()) {
				kind = derr.KindOutOfBounds
			}
			return derr.Wrap(kind, "hunk "+itoa(hi), err)
		}
		// Output lines before the match, all terminated.
		for i := 0; i < posRel; i++ {
			if err := emitLine(buf[i], true); err != nil {
				return err
			}
		}
		// Determine whether the core reaches the final base line, so its
		// terminator can be controlled by the patch's EOF conventions.
		matchEndAbs := bufStart + posRel + coreOldLen(h, coreStart, coreEnd)
		for i := coreStart; i < coreEnd; i++ {
			l := h.Lines[i]
			switch l.Op {
			case diff.OpContext:
				term := true
				if l.NoNewline {
					term = false
				}
				if err := emitLine(buf[posRel], term); err != nil {
					return err
				}
				posRel++
			case diff.OpAdd:
				if err := emitLine(l.Text, !l.NoNewline); err != nil {
					return err
				}
			case diff.OpDelete:
				posRel++
			}
		}
		emitted := posRel
		buf = buf[emitted:]
		bufStart += emitted
		offset += (coreEnd - coreStart) - coreOldLen(h, coreStart, coreEnd)
		_ = matchEndAbs
	}

	// Drain the rest of the base. Every remaining line is terminated unless
	// it is the very last line of the file and the patch wants no trailing
	// newline.
	for {
		if err := fill(bufStart + len(buf) + 1); err != nil {
			return err
		}
		if len(buf) == 0 {
			break
		}
		for i := 0; i < len(buf); i++ {
			if err := ctx.Err(); err != nil {
				return derr.Wrap(derr.KindAborted, "cancelled during drain", err)
			}
			isLast := atEOF && i == len(buf)-1
			term := true
			if isLast {
				term = trailing
			}
			if err := emitLine(buf[i], term); err != nil {
				return err
			}
		}
		buf = nil
	}

	// Empty result with a declared trailing newline yields empty output.
	return writer.Flush()
}

// rangeExceeds reports whether the hunk's declared old range cannot fit a
// target of n lines.
func rangeExceeds(h *diff.Hunk, n int) bool {
	if h.OldLines == 0 {
		return h.OldStart > n
	}
	return h.OldStart < 1 || h.OldStart-1+h.OldLines > n
}

func coreOldLen(h *diff.Hunk, start, end int) int {
	n := 0
	for i := start; i < end; i++ {
		if h.Lines[i].Op == diff.OpContext || h.Lines[i].Op == diff.OpDelete {
			n++
		}
	}
	return n
}

// locateInBuffer finds the match for h within buf. expectedRel is the
// buffer-relative expected start; expectedAbs/bufStart locate pure
// insertions whose hunk has no old lines to match.
func locateInBuffer(buf []string, expectedRel, expectedAbs, bufStart int, h *diff.Hunk, opts patch.ApplyOptions) (int, int, int, error) {
	leadCtx := countLeadingContext(h)
	trailCtx := countTrailingContext(h)
	maxFuzz := opts.Fuzz
	if maxFuzz > leadCtx {
		maxFuzz = leadCtx
	}
	if maxFuzz > trailCtx {
		maxFuzz = trailCtx
	}
	for fuzz := 0; fuzz <= maxFuzz; fuzz++ {
		coreStart := fuzz
		coreEnd := len(h.Lines) - fuzz
		if coreStart > coreEnd {
			coreStart, coreEnd = 0, 0
		}
		pattern := oldPattern(h, coreStart, coreEnd)
		if len(pattern) == 0 {
			if coreStart == 0 && coreEnd == len(h.Lines) && isPureInsertion(h) {
				// OldStart counts lines before the insertion point; convert
				// the absolute position to the current buffer start.
				pos := clamp(expectedAbs-bufStart, 0, len(buf))
				return pos, coreStart, coreEnd, nil
			}
			continue
		}
		lo := expectedRel - opts.MaxOffset
		if lo < 0 {
			lo = 0
		}
		hi := expectedRel + opts.MaxOffset
		if hi > len(buf) {
			hi = len(buf)
		}
		matches := findPattern(buf, pattern, lo, hi, opts.IgnoreWhitespace)
		if len(matches) == 1 {
			return matches[0], coreStart, coreEnd, nil
		}
		if len(matches) > 1 {
			return 0, 0, 0, derr.New(derr.KindAmbiguousMatch, "hunk matches "+itoa(len(matches))+" locations")
		}
		if !opts.IgnoreWhitespace {
			matches = findPattern(buf, pattern, lo, hi, true)
			if len(matches) == 1 {
				return matches[0], coreStart, coreEnd, nil
			}
			if len(matches) > 1 {
				return 0, 0, 0, derr.New(derr.KindAmbiguousMatch, "hunk matches "+itoa(len(matches))+" locations (whitespace-insensitive)")
			}
		}
	}
	return 0, 0, 0, derr.New(derr.KindNoMatch, "cannot locate hunk")
}

func oldPattern(h *diff.Hunk, start, end int) []string {
	var p []string
	for i := start; i < end; i++ {
		l := h.Lines[i]
		if l.Op == diff.OpContext || l.Op == diff.OpDelete {
			p = append(p, l.Text)
		}
	}
	return p
}

func findPattern(lines []string, pattern []string, lo, hi int, ignoreWS bool) []int {
	var out []int
	plen := len(pattern)
	for i := lo; i < hi; i++ {
		if i+plen > len(lines) {
			break
		}
		if matchAt(lines, pattern, i, ignoreWS) {
			out = append(out, i)
		}
	}
	return out
}

func matchAt(lines []string, pattern []string, i int, ignoreWS bool) bool {
	for j, p := range pattern {
		l := lines[i+j]
		if ignoreWS {
			if trimSpace(l) != trimSpace(p) {
				return false
			}
		} else {
			if l != p {
				return false
			}
		}
	}
	return true
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && isSpace(s[start]) {
		start++
	}
	end := len(s)
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
}

func countLeadingContext(h *diff.Hunk) int {
	n := 0
	for _, l := range h.Lines {
		if l.Op == diff.OpContext {
			n++
		} else {
			break
		}
	}
	return n
}

func countTrailingContext(h *diff.Hunk) int {
	n := 0
	for i := len(h.Lines) - 1; i >= 0; i-- {
		if h.Lines[i].Op == diff.OpContext {
			n++
		} else {
			break
		}
	}
	return n
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func isPureInsertion(h *diff.Hunk) bool {
	for _, l := range h.Lines {
		if l.Op != diff.OpAdd {
			return false
		}
	}
	return true
}
