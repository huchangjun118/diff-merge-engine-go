// Package stream provides bounded-memory streaming patch application.
package stream

import (
	"bufio"
	"context"
	"io"
	"strings"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// ApplyStream applies a patch to base, writing the result to out. It reads
// base incrementally and keeps memory bounded by MaxOffset plus the largest
// hunk, regardless of total file size. Cancellation or timeout aborts the
// operation; out is not written to after an abort (the caller should discard
// partial output).
func ApplyStream(ctx context.Context, base io.Reader, out io.Writer, p *diff.Patch, opts patch.ApplyOptions) error {
	opts = opts.WithDefaults()
	scanner := bufio.NewScanner(base)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024*16) // 16 MiB max line
	writer := bufio.NewWriter(out)

	var buf []string // read-but-not-output lines
	bufStart := 0    // absolute line index of buf[0]
	offset := 0      // cumulative line offset from applied hunks

	for hi, h := range p.Hunks {
		if err := ctx.Err(); err != nil {
			return derr.Wrap(derr.KindAborted, "cancelled before hunk "+itoa(hi), err)
		}
		expectedAbs := h.OldStart - 1 + offset // absolute 0-based
		// Ensure buf covers [expectedAbs-MaxOffset, expectedAbs+MaxOffset].
		needAbs := expectedAbs + opts.MaxOffset + 1
		if err := fillBuffer(ctx, scanner, &buf, &bufStart, needAbs); err != nil {
			return err
		}
		// Convert expectedAbs to buffer-relative index.
		expectedRel := expectedAbs - bufStart
		posRel, coreStart, coreEnd, err := locateInBuffer(buf, expectedRel, h, opts)
		if err != nil {
			return derr.Wrap(derr.KindNoMatch, "hunk "+itoa(hi), err)
		}
		// Output lines before the match (buffer-relative).
		for i := 0; i < posRel; i++ {
			if _, err := writer.WriteString(buf[i] + "\n"); err != nil {
				return derr.Wrap(derr.KindAborted, "write failed", err)
			}
		}
		// Output replacement.
		oldLen := 0
		for i := coreStart; i < coreEnd; i++ {
			l := h.Lines[i]
			switch l.Op {
			case diff.OpContext:
				if _, err := writer.WriteString(buf[posRel] + "\n"); err != nil {
					return derr.Wrap(derr.KindAborted, "write failed", err)
				}
				posRel++
				oldLen++
			case diff.OpAdd:
				if _, err := writer.WriteString(l.Text + "\n"); err != nil {
					return derr.Wrap(derr.KindAborted, "write failed", err)
				}
			case diff.OpDelete:
				posRel++
				oldLen++
			}
		}
		// posRel now points past the matched region (buffer-relative).
		// Remove the emitted prefix from the buffer.
		emitted := posRel
		buf = buf[emitted:]
		bufStart += emitted
		offset += (coreEnd - coreStart) - oldLen
	}
	// Drain remaining buffer, reading until EOF.
	for {
		for i := 0; i < len(buf); i++ {
			if err := ctx.Err(); err != nil {
				return derr.Wrap(derr.KindAborted, "cancelled during drain", err)
			}
			if _, err := writer.WriteString(buf[i] + "\n"); err != nil {
				return derr.Wrap(derr.KindAborted, "write failed", err)
			}
		}
		buf = nil
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return derr.Wrap(derr.KindAborted, "read error", err)
			}
			break
		}
		buf = append(buf, scanner.Text())
		bufStart++
	}
	return writer.Flush()
}

// fillBuffer reads lines until the buffer covers absolute line index needAbs.
func fillBuffer(ctx context.Context, scanner *bufio.Scanner, buf *[]string, bufStart *int, needAbs int) error {
	for *bufStart+len(*buf) < needAbs {
		if err := ctx.Err(); err != nil {
			return derr.Wrap(derr.KindAborted, "cancelled while reading", err)
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return derr.Wrap(derr.KindAborted, "read error", err)
			}
			break
		}
		*buf = append(*buf, scanner.Text())
	}
	return nil
}

// locateInBuffer finds the match for h within buf. expectedRel is the
// buffer-relative expected start.
func locateInBuffer(buf []string, expectedRel int, h *diff.Hunk, opts patch.ApplyOptions) (int, int, int, error) {
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
				pos := clamp(expectedRel, 0, len(buf))
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
			if strings.TrimSpace(l) != strings.TrimSpace(p) {
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
