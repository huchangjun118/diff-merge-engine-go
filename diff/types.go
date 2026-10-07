// Package diff generates line-based diffs and represents portable patches.
package diff

// Op identifies the kind of a diff line.
type Op byte

const (
	OpContext Op = ' '
	OpAdd     Op = '+'
	OpDelete  Op = '-'
)

// EOL identifies a line-ending style that a patch can explicitly carry.
// The style is part of the patch contract: applying a patch produces output
// in the patch's declared style, independently of the target's style.
type EOL int

const (
	// EOLDefault means "no explicit style"; renderers/appliers treat it as LF.
	EOLDefault EOL = 0
	// EOLLF is the Unix "\n" style.
	EOLLF EOL = 1
	// EOLCRLF is the Windows "\r\n" style.
	EOLCRLF EOL = 2
)

// Sep returns the concrete separator bytes for the style.
func (e EOL) Sep() string {
	if e == EOLCRLF {
		return "\r\n"
	}
	return "\n"
}

// Line is a single line in a hunk. Text excludes the trailing newline.
// NoNewline means this line is not terminated by a newline on the side(s)
// it belongs to (the "\ No newline at end of file" marker in unified diff).
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
// Line-ending fidelity:
//   - OldEOL/NewEOL carry the line-ending style of the old/new content.
//     Application output uses NewEOL regardless of the target's own style.
//   - OldNoFinalNL/NewNoFinalNL record whether the content ends without a
//     trailing newline. Application output follows NewNoFinalNL.
//
// These properties are rendered as parser-recognizable directives and
// "\ No newline at end of file" markers, so a CRLF patch round-trips
// byte-for-byte on both LF and CRLF environments.
type Patch struct {
	OldFile string
	NewFile string
	Hunks   []*Hunk

	OldEOL EOL
	NewEOL EOL

	OldNoFinalNL bool
	NewNoFinalNL bool
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
