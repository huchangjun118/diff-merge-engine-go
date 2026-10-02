// Package merge implements three-way merging with structured conflicts.
package merge

import (
	"sort"
	"strings"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// Conflict describes a region where both sides changed differently.
type Conflict struct {
	Base      []string
	Ours      []string
	Theirs    []string
	StartLine int // 1-based line in the base where the region begins
}

// MergeResult is the outcome of a three-way merge.
type MergeResult struct {
	Content         []string // merged content (conflicts rendered with markers)
	Conflicts       []Conflict
	Clean           bool
	trailingNewline bool
}

// hunk is a change region in base and version coordinates (0-based, half-open).
type hunk struct {
	baseStart, baseEnd int
	verStart, verEnd   int
}

// Merge performs a three-way merge of base, ours and theirs.
// It is deterministic and order-independent: swapping ours and theirs only
// swaps the conflict sides, never the conflict set or resolution.
func Merge(base, ours, theirs string) *MergeResult {
	baseLines := diff.SplitLines(base)
	ourLines := diff.SplitLines(ours)
	theirLines := diff.SplitLines(theirs)

	hunksO := diffHunks(baseLines, ourLines)
	hunksT := diffHunks(baseLines, theirLines)

	regions := mergeRegions(hunksO, hunksT, len(baseLines))

	var content []string
	var conflicts []Conflict
	clean := true

	for _, r := range regions {
		if len(r.oHunks) == 0 && len(r.tHunks) == 0 {
			content = append(content, baseLines[r.baseStart:r.baseEnd]...)
			continue
		}
		if len(r.oHunks) == 0 {
			content = append(content, theirLines[hunksT[r.tHunks[0]].verStart:hunksT[r.tHunks[0]].verEnd]...)
			continue
		}
		if len(r.tHunks) == 0 {
			content = append(content, ourLines[hunksO[r.oHunks[0]].verStart:hunksO[r.oHunks[0]].verEnd]...)
			continue
		}
		oText := reconstruct(baseLines, ourLines, hunksO, r.baseStart, r.baseEnd, r.oHunks)
		tText := reconstruct(baseLines, theirLines, hunksT, r.baseStart, r.baseEnd, r.tHunks)
		if equalLines(oText, tText) {
			content = append(content, oText...)
			continue
		}
		clean = false
		conflicts = append(conflicts, Conflict{
			Base:      append([]string{}, baseLines[r.baseStart:r.baseEnd]...),
			Ours:      oText,
			Theirs:    tText,
			StartLine: r.baseStart + 1,
		})
		content = append(content, "<<<<<<< ours")
		content = append(content, oText...)
		content = append(content, "=======")
		content = append(content, tText...)
		content = append(content, ">>>>>>> theirs")
	}

	return &MergeResult{Content: content, Conflicts: conflicts, Clean: clean, trailingNewline: strings.HasSuffix(base, "\n")}
}

// diffHunks computes change hunks between base and ver.
func diffHunks(base, ver []string) []hunk {
	p := diff.Generate(diff.JoinLines(base), diff.JoinLines(ver), 0)
	var out []hunk
	for _, dh := range p.Hunks {
		baseStart := dh.OldStart - 1
		baseEnd := baseStart + dh.OldLines
		if baseStart < 0 {
			baseStart, baseEnd = 0, 0
		}
		verStart := dh.NewStart - 1
		verEnd := verStart + dh.NewLines
		if verStart < 0 {
			verStart, verEnd = 0, 0
		}
		out = append(out, hunk{
			baseStart: baseStart,
			baseEnd:   baseEnd,
			verStart:  verStart,
			verEnd:    verEnd,
		})
	}
	return out
}

// region is a merged base range with the hunks from each side that cover it.
type region struct {
	baseStart, baseEnd int
	oHunks, tHunks     []int
}

// mergeRegions groups overlapping hunks from both sides into regions.
// Adjacent (touching) hunks are kept separate; only genuinely overlapping
// base ranges are merged.
func mergeRegions(hunksO, hunksT []hunk, baseLen int) []region {
	type interval struct {
		start, end int
		side       byte
		idx        int
	}
	var intervals []interval
	for i, h := range hunksO {
		intervals = append(intervals, interval{h.baseStart, h.baseEnd, 'o', i})
	}
	for i, h := range hunksT {
		intervals = append(intervals, interval{h.baseStart, h.baseEnd, 't', i})
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start != intervals[j].start {
			return intervals[i].start < intervals[j].start
		}
		return intervals[i].end < intervals[j].end
	})

	var regions []region
	pos := 0
	i := 0
	for i < len(intervals) {
		start := intervals[i].start
		end := intervals[i].end
		var oIdx, tIdx []int
		for i < len(intervals) {
			iv := intervals[i]
			if iv.start > end {
				break
			}
			if !overlaps(start, end, iv.start, iv.end) {
				break
			}
			if iv.side == 'o' {
				oIdx = append(oIdx, iv.idx)
			} else {
				tIdx = append(tIdx, iv.idx)
			}
			if iv.end > end {
				end = iv.end
			}
			i++
		}
		if pos < start {
			regions = append(regions, region{baseStart: pos, baseEnd: start})
		}
		regions = append(regions, region{baseStart: start, baseEnd: end, oHunks: oIdx, tHunks: tIdx})
		pos = end
	}
	if pos < baseLen {
		regions = append(regions, region{baseStart: pos, baseEnd: baseLen})
	}
	return regions
}

// overlaps reports whether two half-open intervals [s1,e1) and [s2,e2) share
// at least one point. Zero-length intervals (pure insertions) at the same
// position are considered overlapping.
func overlaps(s1, e1, s2, e2 int) bool {
	if s1 == e1 && s2 == e2 {
		return s1 == s2
	}
	if s1 == e1 {
		return s2 < s1 && s1 < e2
	}
	if s2 == e2 {
		return s1 < s2 && s2 < e1
	}
	return s1 < e2 && s2 < e1
}

// reconstruct builds the version's text for base[rs:re) using the given hunks.
func reconstruct(base, ver []string, hunks []hunk, rs, re int, idxs []int) []string {
	var out []string
	pos := rs
	for _, idx := range idxs {
		h := hunks[idx]
		if h.baseStart > pos {
			out = append(out, base[pos:h.baseStart]...)
			pos = h.baseStart
		}
		out = append(out, ver[h.verStart:h.verEnd]...)
		pos = h.baseEnd
	}
	if pos < re {
		out = append(out, base[pos:re]...)
	}
	return out
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// String renders the merged content with conflict markers.
func (r *MergeResult) String() string {
	if len(r.Content) == 0 {
		return ""
	}
	s := strings.Join(r.Content, "\n")
	if r.trailingNewline {
		s += "\n"
	}
	return s
}
