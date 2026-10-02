package diffmerge

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
)

// EOL styles recognised by the engine.
const (
	EOLUnix = "\n"
	EOLCRLF = "\r\n"
	EOLCR   = "\r"
)

const (
	defaultReadBuffer = 64 * 1024
	// defaultInMemoryBounds is the default RAM cap for spooled line content.
	defaultInMemoryBounds int64 = 4 << 20
	// spoolRecordSize is the fixed size of one spilled-line index record.
	spoolRecordSize = 8 + 4 + 1 + 8 // dataOffset, textLen, eolKind, hash
)

// Line is one logical line: its terminator is stored separately so that the
// presence or absence of a final newline can be preserved exactly.
type Line struct {
	Text string // content without the line terminator
	EOL  string // "\n", "\r\n" or "" for the final unterminated line
}

// Full returns the content together with its terminator.
func (l Line) Full() string { return l.Text + l.EOL }

// Bytes returns the content together with its terminator as bytes.
func (l Line) Bytes() []byte { return []byte(l.Full()) }

// HasEOL reports whether the line carries a terminator.
func (l Line) HasEOL() bool { return l.EOL != "" }

// byteLen reports the encoded size of the line.
func (l Line) byteLen() int64 { return int64(len(l.Text) + len(l.EOL)) }

// LineCompareOptions controls whitespace and EOL tolerance used when lines
// are compared or hashed. Options are applied in a fixed order so that
// matching behaviour is identical on every platform.
type LineCompareOptions struct {
	// NormalizeEOL treats CRLF terminators as equivalent to LF.
	NormalizeEOL bool
	// IgnoreTrailingWhitespace ignores spaces and tabs at the end of Text.
	IgnoreTrailingWhitespace bool
	// IgnoreAllWhitespace removes every horizontal whitespace character
	// (' ' and '\t') before comparing. Implies IgnoreTrailingWhitespace.
	IgnoreAllWhitespace bool
}

// StreamOptions bounds resource usage while streaming input.
type StreamOptions struct {
	MaxLineBytes int64 // maximum content length of a single line; 0 = unlimited
	MaxLines     int64 // maximum number of lines; 0 = unlimited
	BufferSize   int   // read buffer size; 0 selects a default
}

// LineSource streams lines one at a time and must be closed after use.
type LineSource interface {
	io.Closer
	Next(ctx context.Context) (Line, error) // returns io.EOF when exhausted
}

// RandomLines provides indexed access to a finite set of lines.
type RandomLines interface {
	io.Closer
	N() int
	Line(i int) (Line, error)
	Hash(i int) uint64
	Opts() LineCompareOptions
}

// streamLines is a bounded-memory line splitter. It recognises LF and CRLF;
// a trailing '\r' is folded into the CRLF terminator rather than left in the
// content, which guarantees identical parsing on every platform.
type streamLines struct {
	r        *bufio.Reader
	so       StreamOptions
	started  bool
	nLines   int64
	leftover []byte // bytes already read beyond the current line (always empty here)
}

// NewLineSource returns a streaming reader over r applying the given limits.
func NewLineSource(r io.Reader, so StreamOptions) LineSource {
	bufSize := so.BufferSize
	if bufSize <= 0 {
		bufSize = defaultReadBuffer
	}
	return &streamLines{r: bufio.NewReaderSize(r, bufSize), so: so}
}

func (s *streamLines) Close() error { return nil }

func (s *streamLines) Next(ctx context.Context) (Line, error) {
	if err := ctx.Err(); err != nil {
		return Line{}, err
	}
	var raw []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		raw = append(raw, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if s.so.MaxLineBytes > 0 && int64(len(raw)) > s.so.MaxLineBytes+2 {
				return Line{}, &LimitError{What: "line length", Limit: s.so.MaxLineBytes}
			}
			if err := ctx.Err(); err != nil {
				return Line{}, err
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return Line{}, err
		}
		if len(raw) == 0 && errors.Is(err, io.EOF) {
			return Line{}, io.EOF
		}
		break
	}
	s.nLines++
	if s.so.MaxLines > 0 && s.nLines > s.so.MaxLines {
		return Line{}, &LimitError{What: "line count", Limit: s.so.MaxLines}
	}
	line := decodeRawLine(raw)
	if s.so.MaxLineBytes > 0 && int64(len(line.Text)) > s.so.MaxLineBytes {
		return Line{}, &LimitError{What: "line length", Limit: s.so.MaxLineBytes}
	}
	return line, nil
}

// decodeRawLine splits raw bytes (which end either in '\n' or at EOF) into
// content and terminator.
func decodeRawLine(raw []byte) Line {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		if len(raw) >= 2 && raw[len(raw)-2] == '\r' {
			return Line{Text: string(raw[:len(raw)-2]), EOL: EOLCRLF}
		}
		return Line{Text: string(raw[:len(raw)-1]), EOL: EOLUnix}
	}
	return Line{Text: string(raw), EOL: ""}
}

// ReadAllLines drains src and materialises every line in memory.
func ReadAllLines(ctx context.Context, src LineSource) ([]Line, error) {
	var out []Line
	for {
		l, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
}

// canonicalText returns the comparison form of a line's content.
func canonicalText(text string, opts LineCompareOptions) string {
	switch {
	case opts.IgnoreAllWhitespace:
		return stripHWS(text)
	case opts.IgnoreTrailingWhitespace:
		return trimRightHWS(text)
	default:
		return text
	}
}

func trimRightHWS(s string) string {
	i := len(s)
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
		i--
	}
	return s[:i]
}

func stripHWS(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// HashLine returns the canonical 64-bit FNV-1a hash of a line under opts.
// A separator byte records whether the line carried a terminator so that
// "foo" (no EOL) and "foo\n" never collide.
func HashLine(l Line, opts LineCompareOptions) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(canonicalText(l.Text, opts)))
	sep := byte(0)
	if l.HasEOL() {
		if opts.NormalizeEOL || l.EOL == EOLUnix {
			sep = 1
		} else {
			sep = 2
		}
	}
	_, _ = h.Write([]byte{sep})
	return h.Sum64()
}

// LinesEqual reports whether two lines are considered equal under opts.
func LinesEqual(a, b Line, opts LineCompareOptions) bool {
	if canonicalText(a.Text, opts) != canonicalText(b.Text, opts) {
		return false
	}
	if a.HasEOL() != b.HasEOL() {
		return false
	}
	if !opts.NormalizeEOL && a.EOL != b.EOL {
		return false
	}
	return true
}

// MemoryLines adapts an in-memory slice to RandomLines.
type MemoryLines struct {
	lines []Line
	opts  LineCompareOptions
	hash  []uint64
}

// NewMemoryLines wraps an existing line slice.
func NewMemoryLines(lines []Line, opts LineCompareOptions) *MemoryLines {
	h := make([]uint64, len(lines))
	for i, l := range lines {
		h[i] = HashLine(l, opts)
	}
	return &MemoryLines{lines: lines, opts: opts, hash: h}
}

func (m *MemoryLines) N() int                   { return len(m.lines) }
func (m *MemoryLines) Line(i int) (Line, error) { return m.lines[i], nil }
func (m *MemoryLines) Hash(i int) uint64        { return m.hash[i] }
func (m *MemoryLines) Opts() LineCompareOptions { return m.opts }
func (m *MemoryLines) Close() error             { return nil }

// Spool keeps as many lines as fit in memory and spills the remainder to
// temporary files. Only one spilled line (plus a fixed-size index record) is
// resident at any time, so RAM usage is bounded by maxInMemoryBytes plus a
// small constant regardless of input size.
type Spool struct {
	opts       LineCompareOptions
	mem        []Line
	memHash    []uint64
	memBytes   int64
	memBudget  int64
	spillStart int // index of the first spilled line

	dir      string
	data     *os.File
	index    *os.File
	nSpilled int
	n        int
	closed   bool
	stamp    int64

	// scratch space for reading a spilled record/line
	recBuf [spoolRecordSize]byte
}

// SpoolLines drains src into a memory-bounded spool.
func SpoolLines(ctx context.Context, src LineSource, maxInMemoryBytes int64) (s *Spool, err error) {
	budget := maxInMemoryBytes
	if budget <= 0 {
		budget = defaultInMemoryBounds
	}
	s = &Spool{spillStart: -1, memBudget: budget}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	var count int64
	for {
		l, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s, err
		}
		if err := s.add(l); err != nil {
			return s, err
		}
		s.n++
		count++
		if count&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// SpoolFromMemory wraps already-materialised lines, spilling lazily so that
// large in-memory inputs used by merge can also respect the bound.
func SpoolFromMemory(lines []Line, opts LineCompareOptions, maxInMemoryBytes int64) (s *Spool, err error) {
	budget := maxInMemoryBytes
	if budget <= 0 {
		budget = defaultInMemoryBounds
	}
	s = &Spool{opts: opts, spillStart: -1, memBudget: budget}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	for _, l := range lines {
		if err := s.add(l); err != nil {
			return s, err
		}
		s.n++
	}
	return s, nil
}

func (s *Spool) add(l Line) error {
	h := HashLine(l, s.opts)
	if s.data == nil && s.memBytes+l.byteLen() <= s.memBudget {
		s.mem = append(s.mem, l)
		s.memHash = append(s.memHash, h)
		s.memBytes += l.byteLen()
		return nil
	}
	// This line spills (either spilling already started, or the single line
	// alone exceeds the budget, in which case it is the first spilled line).
	if s.data == nil {
		dir, err := os.MkdirTemp("", "diffmerge-spool-")
		if err != nil {
			return err
		}
		s.dir = dir
		s.spillStart = s.n
		s.data, err = os.Create(filepath.Join(dir, "data"))
		if err != nil {
			return err
		}
		s.index, err = os.Create(filepath.Join(dir, "index"))
		if err != nil {
			return err
		}
	}
	off, err := s.data.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := s.data.Write(l.Bytes()); err != nil {
		return err
	}
	rec := s.recBuf[:0:spoolRecordSize]
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(off))
	rec = append(rec, tmp[:]...)
	binary.BigEndian.PutUint32(tmp[:4], uint32(len(l.Text)))
	rec = append(rec, tmp[:4]...)
	rec = append(rec, eolKind(l.EOL))
	binary.BigEndian.PutUint64(tmp[:], h)
	rec = append(rec, tmp[:]...)
	if _, err := s.index.Write(rec); err != nil {
		return err
	}
	s.nSpilled++
	return nil
}

func eolKind(eol string) byte {
	switch eol {
	case EOLUnix:
		return 1
	case EOLCRLF:
		return 2
	case EOLCR:
		return 3
	default:
		return 0
	}
}

func kindEOL(k byte) string {
	switch k {
	case 1:
		return EOLUnix
	case 2:
		return EOLCRLF
	case 3:
		return EOLCR
	default:
		return ""
	}
}

// SetCompare updates comparison options and rebuilds in-memory hashes.
// Spilled-line hashes are recomputed on demand through Hash using current
// opts; call this before any diff/merge use when the spool was created with
// different options.
func (s *Spool) SetCompare(opts LineCompareOptions) {
	s.opts = opts
	if s.spillStart < 0 {
		for i, l := range s.mem {
			s.memHash[i] = HashLine(l, opts)
		}
		return
	}
	for i, l := range s.mem {
		s.memHash[i] = HashLine(l, opts)
	}
	// Spilled hashes live in the index file; rather than rewriting it, Hash
	// recomputes from the line content when opts differ. Mark index hashes
	// stale by clearing the spillStart hashes lazily via optsStamp.
	s.stamp++
}

func (s *Spool) N() int                   { return s.n }
func (s *Spool) Opts() LineCompareOptions { return s.opts }

func (s *Spool) Hash(i int) uint64 {
	if s.spillStart < 0 || i < s.spillStart {
		return s.memHash[i]
	}
	l, err := s.Line(i)
	if err != nil {
		return 0
	}
	return HashLine(l, s.opts)
}

// readRec loads the fixed index record for spilled index i into recBuf.
func (s *Spool) readRec(i int) (int, error) {
	idx := i - s.spillStart
	if _, err := s.index.Seek(int64(idx)*spoolRecordSize, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(s.index, s.recBuf[:]); err != nil {
		return 0, err
	}
	return idx, nil
}

func (s *Spool) Line(i int) (Line, error) {
	if s.spillStart < 0 || i < s.spillStart {
		return s.mem[i], nil
	}
	if _, err := s.readRec(i); err != nil {
		return Line{}, err
	}
	off := int64(binary.BigEndian.Uint64(s.recBuf[0:8]))
	textLen := int(binary.BigEndian.Uint32(s.recBuf[8:12]))
	eol := kindEOL(s.recBuf[12])
	buf := make([]byte, textLen+len(eol))
	if _, err := s.data.Seek(off, io.SeekStart); err != nil {
		return Line{}, err
	}
	if _, err := io.ReadFull(s.data, buf); err != nil {
		return Line{}, err
	}
	return Line{Text: string(buf[:textLen]), EOL: eol}, nil
}

func (s *Spool) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var firstErr error
	if s.data != nil {
		if err := s.data.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.index != nil {
		if err := s.index.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.dir != "" {
		if err := os.RemoveAll(s.dir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
