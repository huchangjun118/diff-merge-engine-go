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
// The output is rendered in the patch's declared line-ending style and
// trailing-newline state, never in the target's own style.
//
// On any failure (malformed state, encoding mismatch, out-of-bounds range,
// ambiguous or missing context) the original content is returned unchanged:
// the result string is only produced after every hunk has been located and
// spliced in memory, so there is never a partially applied file.
func Apply(content string, p *diff.Patch, opts ApplyOptions) (string, error) {
	if p == nil {
		return content, derr.New(derr.KindMalformedPatch, "nil patch")
	}
	if !utf8.ValidString(content) {
		return content, derr.New(derr.KindEncodingMismatch, "target content is not valid UTF-8")
	}
	if err := validateForApply(p); err != nil {
		return content, err
	}
	opts = opts.withDefaults()
	lines := diff.SplitLines(content)
	result := make([]string, len(lines))
	copy(result, lines)

	offset := 0
	for hi, h := range p.Hunks {
		if eb := checkBounds(len(lines), h, opts.MaxOffset); eb != nil {
			return content, derr.Wrap(eb.Kind, "hunk "+itoa(hi), eb)
		}
		pos, coreStart, coreEnd, err := locateHunk(result, h, offset, opts)
		if err != nil {
			return content, derr.Wrap(errKind(err), "hunk "+itoa(hi), err)
		}
		// Build replacement lines from the core hunk lines.
		var replacement []string
		for i := coreStart; i < coreEnd; i++ {
			l := h.Lines[i]
			switch l.Op {
			case diff.OpContext:
				// Keep the target's matched text (preserves local whitespace).
				replacement = append(replacement, result[pos])
				pos++
			case diff.OpAdd:
				replacement = append(replacement, l.Text)
			case diff.OpDelete:
				// Removed: advance past the target line without emitting it.
				pos++
			}
		}
		// pos should now equal the end of the matched region.
		oldLen := coreOldCount(h, coreStart, coreEnd)
		// Splice.
		newResult := make([]string, 0, len(result)-oldLen+len(replacement))
		newResult = append(newResult, result[:pos-oldLen]...)
		newResult = append(newResult, replacement...)
		newResult = append(newResult, result[pos:]...)
		offset += len(replacement) - oldLen
		result = newResult
	}
	return diff.RenderLines(result, p.NewEOL, p.NewNoFinalNL), nil
}

// validateForApply rejects structurally impossible patches up front.
func validateForApply(p *diff.Patch) error {
	for hi, h := range p.Hunks {
		if h.OldLines < 0 || h.NewLines < 0 || h.OldStart < 0 || h.NewStart < 0 {
			return derr.New(derr.KindOutOfBounds, "hunk "+itoa(hi)+": negative range")
		}
		oc, nc := 0, 0
		for _, l := range h.Lines {
			switch l.Op {
			case diff.OpContext:
				oc++
				nc++
			case diff.OpAdd:
				nc++
			case diff.OpDelete:
				oc++
			default:
				return derr.New(derr.KindMalformedPatch, "hunk contains invalid op")
			}
		}
		if oc != h.OldLines || nc != h.NewLines {
			return derr.New(derr.KindMalformedPatch, "hunk counts disagree with body")
		}
	}
	return nil
}

// checkBounds distinguishes "the patch references content the target cannot
// contain" (KindOutOfBounds) from "the context does not match"
// (KindNoMatch). A hunk is out of bounds when even placing it at the most
// favorable position allowed by maxOffset cannot fit its old range into the
// target.
func checkBounds(targetLen int, h *diff.Hunk, maxOffset int) *derr.Error {
	if h.OldLines == 0 {
		// Pure insertion: its anchor (lines preceding insertion) must be
		// reachable within tolerance.
		if h.OldStart-maxOffset > targetLen {
			return derr.New(derr.KindOutOfBounds, "insertion point beyond content length")
		}
		return nil
	}
	start := h.OldStart - 1 // 0-based declared start
	if start < 0 {
		start = 0
	}
	earliest := start - maxOffset
	if earliest < 0 {
		earliest = 0
	}
	if earliest+h.OldLines > targetLen {
		return derr.New(derr.KindOutOfBounds, "hunk range exceeds content length")
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
	// Limit fuzz to available context lines.
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
			// Empty pattern (pure insertion or fuzz removed all context).
			// Only treat as a match if the hunk is a genuine pure insertion
			// (no context/delete lines in the original hunk).
			if coreStart == 0 && coreEnd == len(h.Lines) && isPureInsertion(h) {
				pos := clamp(expected, 0, len(lines))
				return pos, coreStart, coreEnd, nil
			}
			// Otherwise this fuzz level is invalid; try the next.
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
		// Try whitespace-insensitive if not already.
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
