// Package diff generates line-based diffs and represents portable patches.
package diff

// EOL identifies the line-ending style a file version is encoded with.
type EOL int

const (
	// EOLLF terminates lines with a single '\n' byte.
	EOLLF EOL = iota
	// EOLCRLF terminates lines with the two-byte sequence "\r\n".
	EOLCRLF
)

// String returns "lf" or "crlf".
func (e EOL) String() string {
	switch e {
	case EOLCRLF:
		return "crlf"
	default:
		return "lf"
	}
}

// Terminator returns the raw line terminator for the style.
func (e EOL) Terminator() string {
	if e == EOLCRLF {
		return "\r\n"
	}
	return "\n"
}

// ParseEOL parses "lf"/"crlf" (case-insensitive). Any other value fails.
func ParseEOL(s string) (EOL, bool) {
	switch s {
	case "lf", "LF":
		return EOLLF, true
	case "crlf", "CRLF":
		return EOLCRLF, true
	default:
		return EOLLF, false
	}
}

// Op identifies the kind of a diff line.
type Op byte

const (
	OpContext Op = ' '
	OpAdd     Op = '+'
	OpDelete  Op = '-'
)

// Line is a single line in a hunk. Text excludes the trailing newline and
// never contains CR bytes; the line-ending style is carried by the Patch.
//
// NoNewline reports that this line is the final line of its file version and
// is NOT followed by any line terminator ("\ No newline at end of file").
// For context lines the flag describes both sides; for add/delete lines it
// describes only the side that line belongs to.
type Line struct {
	Op        Op
	Text      string
	NoNewline bool
}

// Hunk is one contiguous region of a patch.
type Hunk struct {
	OldStart int // 1-based start line in the old file
	OldLines int // number of old lines in the hunk
	NewStart int // 1-based start line in the new file
	NewLines int // number of new lines in the hunk
	Lines    []Line
}

// Patch is a portable unified-diff representation. It intentionally omits
// timestamps, index hashes and file modes so the same patch text applies
// identically in different environments.
//
// EOL fidelity: OldEOL/NewEOL carry the exact line-ending style of each file
// version and OldTrailingNewline/NewTrailingNewline carry the end-of-file
// newline state. Applying the patch always renders the output with NewEOL and
// the declared trailing-newline state, independently of the target's own
// style. A patch that changes ONLY the trailing newline is represented by a
// final hunk containing the last file line with NoNewline markers.
type Patch struct {
	OldFile string
	NewFile string
	Hunks   []*Hunk

	// OldEOL/NewEOL are the line-ending styles of the old/new version.
	OldEOL EOL
	NewEOL EOL
	// OldTrailingNewline/NewTrailingNewline report whether the final line
	// is terminated. Empty content is reported as false.
	OldTrailingNewline bool
	NewTrailingNewline bool
}

// ContextLines returns the number of context (unchanged) lines in the hunk.
func (h *Hunk) ContextLines() int {
	n := 0
	for _, l := range h.Lines {
		if l.Op == OpContext {
			n++
		}
	}
	return n
}

// AddedLines returns the number of added lines in the hunk.
func (h *Hunk) AddedLines() int {
	n := 0
	for _, l := range h.Lines {
		if l.Op == OpAdd {
			n++
		}
	}
	return n
}

// DeletedLines returns the number of deleted lines in the hunk.
func (h *Hunk) DeletedLines() int {
	n := 0
	for _, l := range h.Lines {
		if l.Op == OpDelete {
			n++
		}
	}
	return n
}
