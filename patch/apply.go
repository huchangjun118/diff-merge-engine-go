package patch

import (
	"strings"
	"unicode/utf8"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// ApplyOptions controls fuzzy matching behaviour.
type ApplyOptions struct {
	// Fuzz is the maximum number of context lines that may be ignored at each
	// edge of a hunk when no exact match is found. Defaults to 2.
	Fuzz int
	// MaxOffset is the maximum line shift tolerated when locating a hunk.
	// Defaults to 100.
	MaxOffset int
	// IgnoreWhitespace matches lines after trimming leading/trailing whitespace.
	IgnoreWhitespace bool
}

func (o *ApplyOptions) withDefaults() ApplyOptions {
	cp := *o
	if cp.Fuzz <= 0 {
		cp.Fuzz = 2
	}
	if cp.MaxOffset <= 0 {
		cp.MaxOffset = 100
	}
	return cp
}

// Apply applies patch to content and returns the resulting content.
//
// The result is byte-level determined by the patch, never by the target's
// own conventions:
//
//   - Output line endings equal the patch's NewEOL (LF when the patch does
//     not declare one), regardless of the target's endings.
//   - The output ends with a newline iff the patch's new side does. A patch
//     without that metadata preserves the traditional trailing newline.
//
// Nothing is mutated on failure: on error the returned string is exactly the
// input content and the error kind distinguishes malformed patches
// (KindMalformedPatch), invalid input encoding (KindEncodingMismatch),
// structurally out-of-range hunks (KindOutOfBounds), ambiguous
// (KindAmbiguousMatch) and unlocatable (KindNoMatch) context.
func Apply(content string, p *diff.Patch, opts ApplyOptions) (string, error) {
	opts = opts.withDefaults()
	if p == nil {
		return content, derr.New(derr.KindMalformedPatch, "nil patch")
	}
	if !utf8.ValidString(content) {
		return content, derr.New(derr.KindEncodingMismatch, "target content is not valid UTF-8")
	}
	lines := diff.SplitLines(content)
	work := make([]string, len(lines))
	copy(work, lines)

	if err := validateRanges(p, len(lines)); err != nil {
		return content, err
	}

	offset := 0
	for hi, h := range p.Hunks {
		pos, coreStart, coreEnd, err := locateHunk(work, h, offset, opts)
		if err != nil {
			return content, derr.Wrap(derr.Kind(errKind(err)), "hunk "+itoa(hi), err)
		}
		// Build replacement lines from the matched core.
		var replacement []string
		matchPos := pos
		for i := coreStart; i < coreEnd; i++ {
			l := h.Lines[i]
			switch l.Op {
			case diff.OpContext:
				// Keep the target's matched text (preserves local whitespace).
				replacement = append(replacement, work[matchPos])
				matchPos++
			case diff.OpAdd:
				replacement = append(replacement, l.Text)
			case diff.OpDelete:
				matchPos++
			}
		}
		oldLen := coreOldCount(h, coreStart, coreEnd)
		newResult := make([]string, 0, len(work)-oldLen+len(replacement))
		newResult = append(newResult, work[:matchPos-oldLen]...)
		newResult = append(newResult, replacement...)
		newResult = append(newResult, work[matchPos:]...)
		offset += len(replacement) - oldLen
		work = newResult
	}

	trailing := desiredTrailing(p, len(lines) > 0)
	return diff.Render(work, p.NewEOL, trailing), nil
}

// desiredTrailing resolves the output trailing-newline state. Patches carrying
// explicit metadata (the directive) or an EOF no-newline marker always win;
// the target's own state is never consulted. Bare metadata-free patches keep
// the traditional trailing newline.
func desiredTrailing(p *diff.Patch, inputHadLines bool) bool {
	if metadataPresent(p) {
		return p.NewTrailingNewline
	}
	if len(p.Hunks) > 0 {
		last := p.Hunks[len(p.Hunks)-1]
		for i := len(last.Lines) - 1; i >= 0; i-- {
			l := last.Lines[i]
			if l.Op == diff.OpAdd || l.Op == diff.OpContext {
				return !l.NoNewline
			}
		}
	}
	return true
}

func metadataPresent(p *diff.Patch) bool {
	return p.OldEOL != diff.EOLLF || p.NewEOL != diff.EOLLF ||
		!p.OldTrailingNewline || !p.NewTrailingNewline
}

// validateRanges rejects hunks whose declared old range cannot exist in the
// target before any mutation happens. This separates "the patch references
// lines the file does not have" (KindOutOfBounds) from a contextual mismatch
// at an existing location (KindNoMatch). Pure insertions (count 0) are valid
// exactly at EOF.
func validateRanges(p *diff.Patch, nLines int) error {
	for hi, h := range p.Hunks {
		start := h.OldStart // 1-based; 0 is used for leading insertions
		count := h.OldLines
		if count == 0 {
			// Insertion point: start may equal nLines (append at EOF).
			if start < 0 || start > nLines {
				return derr.New(derr.KindOutOfBounds,
					"hunk "+itoa(hi)+" insertion point "+itoa(start)+" exceeds content length "+itoa(nLines))
			}
			continue
		}
		if start < 1 || start-1+count > nLines {
			return derr.New(derr.KindOutOfBounds,
				"hunk "+itoa(hi)+" old range "+itoa(start)+","+itoa(count)+
					" exceeds content length "+itoa(nLines))
		}
	}
	return nil
}

// locateHunk finds the target index where h applies. It returns the start
// index and the core line range [coreStart, coreEnd) into h.Lines that was
// matched. It tries increasing fuzz levels.
func locateHunk(lines []string, h *diff.Hunk, offset int, opts ApplyOptions) (start int, coreStart, coreEnd int, err error) {
	expected := h.OldStart - 1 + offset // 0-based
	if expected < 0 {
		expected = 0
	}
	maxFuzz := opts.Fuzz
	leadCtx := countLeadingContext(h)
	trailCtx := countTrailingContext(h)
	if maxFuzz > leadCtx {
		maxFuzz = leadCtx
	}
	if maxFuzz > trailCtx {
		maxFuzz = trailCtx
	}

	for fuzz := 0; fuzz <= maxFuzz; fuzz++ {
		coreStart = fuzz
		coreEnd = len(h.Lines) - fuzz
		if coreStart > coreEnd {
			coreStart, coreEnd = 0, 0
		}
		pattern := oldPattern(h, coreStart, coreEnd)
		if len(pattern) == 0 {
			if coreStart == 0 && coreEnd == len(h.Lines) && isPureInsertion(h) {
				// OldStart counts lines before the insertion point, so an
				// insertion after the final line lands at len(lines).
				pos := h.OldStart + offset
				pos = clamp(pos, 0, len(lines))
				return pos, coreStart, coreEnd, nil
			}
			continue
		}
		lo := expected - opts.MaxOffset
		if lo < 0 {
			lo = 0
		}
		hi := expected + opts.MaxOffset
		if hi > len(lines) {
			hi = len(lines)
		}
		matches := findPattern(lines, pattern, lo, hi, opts.IgnoreWhitespace)
		if len(matches) == 1 {
			return matches[0], coreStart, coreEnd, nil
		}
		if len(matches) > 1 {
			return 0, 0, 0, derr.New(derr.KindAmbiguousMatch,
				"hunk matches "+itoa(len(matches))+" locations")
		}
		if !opts.IgnoreWhitespace {
			matches = findPattern(lines, pattern, lo, hi, true)
			if len(matches) == 1 {
				return matches[0], coreStart, coreEnd, nil
			}
			if len(matches) > 1 {
				return 0, 0, 0, derr.New(derr.KindAmbiguousMatch,
					"hunk matches "+itoa(len(matches))+" locations (whitespace-insensitive)")
			}
		}
	}
	return 0, 0, 0, derr.New(derr.KindNoMatch, "cannot locate hunk")
}

// findPattern returns all indices i in [lo,hi) where pattern matches lines[i:i+len(pattern)].
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

// oldPattern returns the context+delete lines in h.Lines[start:end].
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

func coreOldCount(h *diff.Hunk, start, end int) int {
	n := 0
	for i := start; i < end; i++ {
		if h.Lines[i].Op == diff.OpContext || h.Lines[i].Op == diff.OpDelete {
			n++
		}
	}
	return n
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

func errKind(err error) derr.Kind {
	if e, ok := err.(*derr.Error); ok {
		return e.Kind
	}
	return derr.KindNoMatch
}

func isPureInsertion(h *diff.Hunk) bool {
	for _, l := range h.Lines {
		if l.Op != diff.OpAdd {
			return false
		}
	}
	return true
}
