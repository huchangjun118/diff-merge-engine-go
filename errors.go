package diffmerge

import "strings"

// MalformedPatchError indicates that a patch could not be parsed or is
// structurally invalid (bad hunk headers, mismatched counts, garbage lines).
type MalformedPatchError struct {
	Line int // 1-based line number in the patch input, 0 if unknown
	Msg  string
}

func (e *MalformedPatchError) Error() string {
	if e.Line > 0 {
		return "malformed patch at line " + itoa(e.Line) + ": " + e.Msg
	}
	return "malformed patch: " + e.Msg
}

// EncodingError indicates that an input did not satisfy the declared encoding
// (for example invalid UTF-8 when RequireUTF8 is enabled) or that a patch
// payload cannot represent the source bytes.
type EncodingError struct {
	Offset int64
	Msg    string
}

func (e *EncodingError) Error() string {
	if e.Offset > 0 {
		return "encoding error at offset " + itoa64(e.Offset) + ": " + e.Msg
	}
	return "encoding error: " + e.Msg
}

// AmbiguousMatchError means a hunk matched more than one location inside the
// tolerance window and the correct location could not be uniquely determined.
type AmbiguousMatchError struct {
	Hunk       int   // 0-based hunk index
	Candidates []int // candidate old-side line numbers (1-based)
}

func (e *AmbiguousMatchError) Error() string {
	parts := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		parts[i] = itoa(c)
	}
	return "hunk " + itoa(e.Hunk+1) + " matches multiple locations: lines " + strings.Join(parts, ",")
}

// OffsetExceededError means a hunk could not be found within the configured
// maximum context offset.
type OffsetExceededError struct {
	Hunk      int // 0-based hunk index
	Want      int // hunk's declared old start (1-based)
	Offset    int // best (smallest) offset found, 0 if none
	MaxOffset int // configured tolerance
}

func (e *OffsetExceededError) Error() string {
	return "hunk " + itoa(e.Hunk+1) + " starting at line " + itoa(e.Want) +
		" could not be located within offset tolerance " + itoa(e.MaxOffset)
}

// BoundsError indicates that a hunk references content outside of the target
// file (for example line numbers past EOF with no match).
type BoundsError struct {
	Hunk int
	Msg  string
}

func (e *BoundsError) Error() string {
	return "out of bounds: hunk " + itoa(e.Hunk+1) + ": " + e.Msg
}

// LimitError indicates that a configured resource limit was exceeded
// (maximum line count, maximum line length, ...). This is never a conflict.
type LimitError struct {
	What  string
	Limit int64
}

func (e *LimitError) Error() string {
	return "limit exceeded: " + e.What + " > " + itoa64(e.Limit)
}

// ChecksumError indicates that stored content failed its checksum verification.
type ChecksumError struct {
	Want string
	Got  string
}

func (e *ChecksumError) Error() string {
	return "checksum mismatch: want " + e.Want + " got " + e.Got
}

// NotFoundError indicates a referenced object (base version, blob, commit)
// does not exist.
type NotFoundError struct {
	What string
}

func (e *NotFoundError) Error() string { return "not found: " + e.What }

// StateError indicates an invalid store/transaction state.
type StateError struct{ Msg string }

func (e *StateError) Error() string { return "invalid state: " + e.Msg }

func itoa(i int) string { return itoa64(int64(i)) }
func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var neg bool
	var b [24]byte
	if i < 0 {
		neg = true
		i = -i
	}
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
