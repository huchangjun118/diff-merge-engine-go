// Package stream provides bounded-memory streaming patch application.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// spoolThreshold is the amount of buffered output kept in memory before the
// safe writer spills to a temporary file.
const spoolThreshold = 8 * 1024 * 1024

// ApplyStream applies a patch to base, writing the result to out. It reads
// base incrementally and keeps working memory bounded by roughly
// MaxOffset plus the largest hunk, regardless of total file size.
//
// Atomicity: nothing is written to out until the whole result has been
// produced successfully. If a hunk cannot be located, the context is
// cancelled/times out, or the input is not valid UTF-8, the function
// returns a typed error and out receives zero bytes. Large outputs are
// spooled to a temporary file that is always removed on failure.
func ApplyStream(ctx context.Context, base io.Reader, out io.Writer, p *diff.Patch, opts patch.ApplyOptions) error {
	if err := ctx.Err(); err != nil {
		return derr.Wrap(derr.KindAborted, "cancelled before start", err)
	}
	if p == nil {
		return derr.New(derr.KindMalformedPatch, "nil patch")
	}
	opts = opts.WithDefaults()

	sw, err := newSafeWriter(out, spoolThreshold)
	if err != nil {
		return err
	}
	defer sw.Abort()

	if err := runStream(ctx, base, sw, p, opts); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return derr.Wrap(derr.KindAborted, "cancelled before commit", err)
	}
	return sw.Commit()
}

func runStream(ctx context.Context, base io.Reader, sw *safeWriter, p *diff.Patch, opts patch.ApplyOptions) error {
	r := newLineReader(base)
	var buf []string // read-but-not-output logical lines
	bufStart := 0    // absolute line index of buf[0]
	offset := 0      // cumulative line offset from applied hunks

	for hi, h := range p.Hunks {
		if err := ctx.Err(); err != nil {
			return derr.Wrap(derr.KindAborted, "cancelled before hunk "+itoa(hi), err)
		}
		expectedAbs := h.OldStart - 1 + offset // absolute 0-based
		needAbs := expectedAbs + opts.MaxOffset + 1
		atEOF, err := fillBuffer(ctx, r, &buf, &bufStart, needAbs)
		if err != nil {
			return err
		}
		if eb := checkStreamBounds(totalKnown(atEOF, bufStart, len(buf)), h, opts.MaxOffset); eb != nil {
			return derr.Wrap(derr.KindOutOfBounds, "hunk "+itoa(hi), eb)
		}
		expectedRel := expectedAbs - bufStart
		posRel, coreStart, coreEnd, err := locateInBuffer(buf, expectedRel, h, opts)
		if err != nil {
			return derr.Wrap(errKind(err), "hunk "+itoa(hi), err)
		}
		// Output lines before the match.
		for i := 0; i < posRel; i++ {
			if err := sw.writeLine(buf[i], p.NewEOL); err != nil {
				return err
			}
		}
		// Output replacement.
		oldLen := 0
		for i := coreStart; i < coreEnd; i++ {
			l := h.Lines[i]
			switch l.Op {
			case diff.OpContext:
				if err := sw.writeLine(buf[posRel], p.NewEOL); err != nil {
					return err
				}
				posRel++
				oldLen++
			case diff.OpAdd:
				if err := sw.writeLine(l.Text, p.NewEOL); err != nil {
					return err
				}
			case diff.OpDelete:
				posRel++
				oldLen++
			}
		}
		emitted := posRel
		buf = buf[emitted:]
		bufStart += emitted
		offset += (coreEnd - coreStart) - oldLen
	}

	// Drain remaining input.
	for {
		for i := 0; i < len(buf); i++ {
			if err := ctx.Err(); err != nil {
				return derr.Wrap(derr.KindAborted, "cancelled during drain", err)
			}
			if err := sw.writeLine(buf[i], p.NewEOL); err != nil {
				return err
			}
		}
		buf = nil
		line, ok, err := r.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		buf = append(buf, line)
	}
	// Handle final newline state: renderLines needs to know whether the last
	// line is terminated. Rewrite the tail through sw.
	return sw.finish(p)
}

func totalKnown(atEOF bool, bufStart, bufLen int) int {
	if !atEOF {
		// Not at EOF yet; only reject ranges beyond the known prefix when the
		// declared range cannot fit even an arbitrarily longer file (never).
		// Return -1 to mean "unknown".
		return -1
	}
	return bufStart + bufLen
}

// checkStreamBounds mirrors patch.checkBounds. totalLen may be -1 while the
// reader has not yet reached EOF, in which case only trivially impossible
// headers are rejected.
func checkStreamBounds(totalLen int, h *diff.Hunk, maxOffset int) *derr.Error {
	if totalLen < 0 {
		if h.OldStart < 0 || h.OldLines < 0 || h.NewStart < 0 || h.NewLines < 0 {
			return derr.New(derr.KindOutOfBounds, "negative hunk range")
		}
		return nil
	}
	if h.OldLines == 0 {
		if h.OldStart-maxOffset > totalLen {
			return derr.New(derr.KindOutOfBounds, "insertion point beyond content length")
		}
		return nil
	}
	start := h.OldStart - 1
	if start < 0 {
		start = 0
	}
	earliest := start - maxOffset
	if earliest < 0 {
		earliest = 0
	}
	if earliest+h.OldLines > totalLen {
		return derr.New(derr.KindOutOfBounds, "hunk range exceeds content length")
	}
	return nil
}

// lineReader yields logical lines (separators stripped, including a missing
// final newline) and validates UTF-8 incrementally.
type lineReader struct {
	sc *bufio.Scanner
}

func newLineReader(r io.Reader) *lineReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024*16)
	sc.Split(scanLinesKeepEOF)
	return &lineReader{sc: sc}
}

func (r *lineReader) Next() (string, bool, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return "", false, derr.Wrap(derr.KindAborted, "read error", err)
		}
		return "", false, nil
	}
	tok := r.sc.Text()
	line := strings.TrimSuffix(tok, "\n")
	line = strings.TrimSuffix(line, "\r")
	if !utf8.ValidString(line) {
		return "", false, derr.New(derr.KindEncodingMismatch, "input line is not valid UTF-8")
	}
	return line, true, nil
}

// scanLinesKeepEOF is like bufio.ScanLines but the final token keeps its
// newline when present, so callers can tell whether the file ended with one.
func scanLinesKeepEOF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[0 : i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// fillBuffer reads lines until the buffer covers absolute line index needAbs
// or EOF is reached.
func fillBuffer(ctx context.Context, r *lineReader, buf *[]string, bufStart *int, needAbs int) (bool, error) {
	for *bufStart+len(*buf) < needAbs {
		if err := ctx.Err(); err != nil {
			return false, derr.Wrap(derr.KindAborted, "cancelled while reading", err)
		}
		line, ok, err := r.Next()
		if err != nil {
			return false, err
		}
		if !ok {
			return true, nil
		}
		*buf = append(*buf, line)
	}
	return false, nil
}

// safeWriter buffers all output in memory or a temp file and only releases it
// to the destination on Commit. Abort discards everything.
type safeWriter struct {
	dst     io.Writer
	mem     bytes.Buffer
	file    *os.File
	tmpName string
	limit   int
	n       int
	hasData bool
}

func newSafeWriter(dst io.Writer, memLimit int) (*safeWriter, error) {
	return &safeWriter{dst: dst, limit: memLimit}, nil
}

func (w *safeWriter) writeLine(line string, eol diff.EOL) error {
	w.hasData = true
	chunk := line + eol.Sep()
	w.n += len(chunk)
	if w.file != nil {
		if _, err := io.WriteString(w.file, chunk); err != nil {
			return derr.Wrap(derr.KindAborted, "write failed", err)
		}
		return nil
	}
	if w.mem.Len()+len(chunk) > w.limit {
		f, err := os.CreateTemp("", "dme-stream-*")
		if err != nil {
			return derr.Wrap(derr.KindAborted, "cannot create temp file", err)
		}
		if _, err := f.Write(w.mem.Bytes()); err != nil {
			f.Close()
			os.Remove(f.Name())
			return derr.Wrap(derr.KindAborted, "cannot spool temp file", err)
		}
		w.file = f
		w.tmpName = f.Name()
		w.mem.Reset()
		if _, err := io.WriteString(w.file, chunk); err != nil {
			return derr.Wrap(derr.KindAborted, "write failed", err)
		}
		return nil
	}
	w.mem.WriteString(chunk)
	return nil
}

// finish applies the patch-mandated final newline state. With the streaming
// model the last emitted line was written with a terminator; if the patch
// demands no final newline the trailing terminator is truncated in the
// spooled output.
func (w *safeWriter) finish(p *diff.Patch) error {
	if !w.hasData {
		return nil
	}
	if !p.NewNoFinalNL {
		return nil
	}
	sep := p.NewEOL.Sep()
	if w.file != nil {
		off, err := w.file.Seek(0, io.SeekEnd)
		if err != nil {
			return derr.Wrap(derr.KindAborted, "seek failed", err)
		}
		if off < int64(len(sep)) {
			return nil
		}
		if err := w.file.Truncate(off - int64(len(sep))); err != nil {
			return derr.Wrap(derr.KindAborted, "truncate failed", err)
		}
		w.n -= len(sep)
		return nil
	}
	s := strings.TrimSuffix(w.mem.String(), sep)
	w.mem.Reset()
	w.mem.WriteString(s)
	w.n -= len(sep)
	return nil
}

// Commit flushes the buffered result to the destination. Exactly one of
// Commit or Abort should be called; a Commit error still leaves the temp file
// cleanup to the deferred Abort.
func (w *safeWriter) Commit() error {
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return derr.Wrap(derr.KindAborted, "sync failed", err)
		}
		if _, err := w.file.Seek(0, io.SeekStart); err != nil {
			return derr.Wrap(derr.KindAborted, "seek failed", err)
		}
		if _, err := io.Copy(w.dst, w.file); err != nil {
			return derr.Wrap(derr.KindAborted, "commit failed", err)
		}
	} else if w.mem.Len() > 0 {
		if _, err := w.dst.Write(w.mem.Bytes()); err != nil {
			return derr.Wrap(derr.KindAborted, "commit failed", err)
		}
	}
	w.Abort()
	return nil
}

// Abort discards any spooled data and removes temp files.
func (w *safeWriter) Abort() {
	if w.file != nil {
		name := w.file.Name()
		w.file.Close()
		os.Remove(name)
		w.file = nil
		w.tmpName = ""
	}
	w.mem.Reset()
}

// ApplyStreamToFile is a convenience wrapper that applies a patch to the
// file at path atomically: the result is fully built in a sibling temp file
// and renamed over the original only on success. Cancellation, timeout or
// apply failure leaves the original file byte-for-byte intact.
func ApplyStreamToFile(ctx context.Context, path string, p *diff.Patch, opts patch.ApplyOptions) error {
	in, err := os.Open(path)
	if err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot open target", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dme-apply-*")
	if err != nil {
		in.Close()
		return derr.Wrap(derr.KindCorrupt, "cannot create temp output", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := ApplyStream(ctx, in, tmp, p, opts); err != nil {
		in.Close()
		return err
	}
	in.Close()
	if err := tmp.Sync(); err != nil {
		return derr.Wrap(derr.KindCorrupt, "sync failed", err)
	}
	if err := tmp.Close(); err != nil {
		return derr.Wrap(derr.KindCorrupt, "close failed", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return derr.Wrap(derr.KindCorrupt, "rename failed", err)
	}
	committed = true
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

func errKind(err error) derr.Kind {
	if e, ok := err.(*derr.Error); ok {
		return e.Kind
	}
	return derr.KindNoMatch
}
