// Package diff generates line-based diffs and represents portable patches.
package diff

// Op identifies the kind of a diff line.
type Op byte

const (
	OpContext Op = ' '
	OpAdd     Op = '+'
	OpDelete  Op = '-'
)

// Line is a single line in a hunk. Text excludes the trailing newline;
// the newline is normalized to '\n' regardless of the source platform.
type Line struct {
	Op   Op
	Text string
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
type Patch struct {
	OldFile string
	NewFile string
	Hunks   []*Hunk
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
