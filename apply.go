package diffmerge

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

const (
	defaultMaxOffset = 1000
	defaultApplyMem  = int64(4 << 20)
)

// ApplyOptions controls fuzzy application of a patch.
type ApplyOptions struct {
	// Compare sets whitespace/EOL tolerance for matching context and removed
	// lines. Added lines are taken verbatim from the patch.
	Compare LineCompareOptions
	// MaxOffset bounds how far (in target lines) a hunk may drift from its
	// declared position. 0 selects the default (1000). -1 means unlimited.
	MaxOffset int
	// Fuzz drops this many context lines from each end of a hunk before
	// searching (like git's fuzz factor). 0 = exact context.
	Fuzz int
	// RequireUTF8 rejects invalid UTF-8 in target and patch content.
	RequireUTF8 bool
	// MaxMemoryBytes bounds RAM used to hold target lines; larger targets
	// spill to temporary files. 0 selects the default (4 MiB).
	MaxMemoryBytes int64
}

// AppliedHunk records where one hunk was applied in the target.
type AppliedHunk struct {
	HunkIndex int
	OldStart  int // matched old-side position, 1-based
	NewStart  int // resulting new-side position, 1-based
	Offset    int // signed drift from the declared position
}

// ApplyResult reports a successful application.
type ApplyResult struct {
	Hunks []AppliedHunk
}

type match struct {
	start int // 0-based index in target where the hunk old-side starts
	span  int // number of target lines consumed (context+removed)
}

// Apply streams a patch against target, writing the result to out.
//
// On any error nothing is written to out: the whole application is computed
// (target lines spooled with bounded RAM) before the first output byte.
func Apply(ctx context.Context, target io.Reader, patch *Patch, out io.Writer, opts ApplyOptions) (*ApplyResult, error) {
	if patch == nil {
		return nil, &MalformedPatchError{Msg: "nil patch"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePatchForApply(patch, opts); err != nil {
		return nil, err
	}
	budget := opts.MaxMemoryBytes
	if budget <= 0 {
		budget = defaultApplyMem
	}
	spool, err := spoolReader(ctx, target, opts, budget)
	if err != nil {
		return nil, err
	}
	defer spool.Close()

	if len(patch.Hunks) == 0 {
		// Identity patch: reproduce the (fully read) target.
		for i := 0; i < spool.N(); i++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			l, err := spool.Line(i)
			if err != nil {
				return nil, err
			}
			if _, err := io.WriteString(out, l.Full()); err != nil {
				return nil, err
			}
		}
		return &ApplyResult{}, nil
	}

	matches, err := locateAllHunks(ctx, spool, patch, opts)
	if err != nil {
		return nil, err
	}
	if err := emitPatched(ctx, spool, patch, matches, out, opts); err != nil {
		return nil, err
	}
	res := &ApplyResult{Hunks: make([]AppliedHunk, len(matches))}
	// Replay the transform from line 0 to derive new-side start positions.
	cursor, newPos := 0, 0
	for i, m := range matches {
		for cursor < m.start {
			cursor++
			newPos++
		}
		h := patch.Hunks[i]
		res.Hunks[i] = AppliedHunk{
			HunkIndex: i,
			OldStart:  m.start + 1,
			NewStart:  newPos + 1,
			Offset:    m.start - (h.OldStart - 1),
		}
		for _, hl := range h.Body {
			switch hl.Kind {
			case HunkOld:
				cursor++
			case HunkContext:
				cursor++
				newPos++
			case HunkNew:
				newPos++
			}
		}
	}
	return res, nil
}

func spoolReader(ctx context.Context, r io.Reader, opts ApplyOptions, budget int64) (*Spool, error) {
	if opts.RequireUTF8 {
		r = &utf8Checker{r: r}
	}
	src := NewLineSource(r, StreamOptions{BufferSize: defaultReadBuffer})
	s, err := SpoolLines(ctx, src, budget)
	if err != nil {
		return nil, err
	}
	// Rebuild hashes under the matching tolerance so equality checks use the
	// canonical comparison form (content itself is never rewritten).
	s.SetCompare(opts.Compare)
	return s, nil
}

type utf8Checker struct {
	r    io.Reader
	tail []byte
	off  int64
}

func (u *utf8Checker) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	u.off += int64(n)
	// Validate up to the last possible rune boundary; carry the remainder.
	data := append(append([]byte(nil), u.tail...), p[:n]...)
	valid := 0
	for valid < len(data) {
		r, size := utf8.DecodeRune(data[valid:])
		if r == utf8.RuneError && size == 1 {
			if err == nil && len(data)-valid < utf8.UTFMax {
				break // might complete in next chunk
			}
			return n, &EncodingError{Offset: u.off - int64(len(data)-valid), Msg: "invalid UTF-8 sequence"}
		}
		valid += size
	}
	u.tail = append(u.tail[:0], data[valid:]...)
	return n, err
}

func validatePatchForApply(p *Patch, opts ApplyOptions) error {
	for hi, h := range p.Hunks {
		var oc, nc int
		oldCount := 0
		for _, hl := range h.Body {
			switch hl.Kind {
			case HunkContext:
				oc++
				nc++
				oldCount++
			case HunkOld:
				oc++
				oldCount++
			case HunkNew:
				nc++
			}
			if opts.RequireUTF8 {
				if !utf8.ValidString(hl.Line.Text) {
					return &EncodingError{Msg: "patch contains invalid UTF-8"}
				}
			}
		}
		if oc != h.OldLines || nc != h.NewLines {
			return &MalformedPatchError{Msg: "hunk counts disagree with body"}
		}
		if oldCount == 0 && h.OldStart == 0 {
			// pure insertion: declared as zero-length range at a line
		}
		if opts.Fuzz < 0 {
			return &MalformedPatchError{Msg: "negative fuzz"}
		}
		_ = hi
	}
	return nil
}

// locateAllHunks searches the target for every hunk, in patch order,
// constraining later hunks to start after earlier matches.
func locateAllHunks(ctx context.Context, t *Spool, p *Patch, opts ApplyOptions) ([]match, error) {
	maxOff := opts.MaxOffset
	if maxOff == 0 {
		maxOff = defaultMaxOffset
	}
	matches := make([]match, len(p.Hunks))
	minStart := 0
	for i, h := range p.Hunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := locateOne(ctx, t, h, i, maxOff, opts, minStart)
		if err != nil {
			return nil, err
		}
		matches[i] = m
		minStart = m.start + m.span
	}
	return matches, nil
}

// hunkPattern is the matchable part of a hunk after fuzz trimming.
type hunkPattern struct {
	// entries in order; each is either a context/removed target line (match)
	// or an inserted line (skipped during matching)
	kinds []HunkLineKind
	lines []Line
	span  int // number of target lines covered by the retained pattern
	// lead and trail count context entries removed from the hunk's front and
	// back; the match at target position p therefore covers the full hunk
	// window [p-lead, p+span+trail).
	lead  int
	trail int
}

// buildPattern returns the hunk pattern used for fuzzy matching. A context
// line is one body entry shared by both old and new sides, so dropping the
// first fuzz leading context entries and the last fuzz trailing context
// entries trims both sides symmetrically by construction.
func buildPattern(h *Hunk, fuzz int) hunkPattern {
	body := h.Body
	first, last := 0, len(body)
	if fuzz > 0 {
		drop := 0
		for first < last && body[first].Kind == HunkContext && drop < fuzz {
			first++
			drop++
		}
		drop = 0
		for last > first && body[last-1].Kind == HunkContext && drop < fuzz {
			last--
			drop++
		}
	}
	var pat hunkPattern
	pat.lead = first
	pat.trail = len(body) - last
	for i := first; i < last; i++ {
		hl := body[i]
		pat.kinds = append(pat.kinds, hl.Kind)
		pat.lines = append(pat.lines, hl.Line)
		if hl.Kind != HunkNew {
			pat.span++
		}
	}
	return pat
}

// locateOne finds the unique target location for a hunk.
func locateOne(ctx context.Context, t *Spool, h *Hunk, idx, maxOff int, opts ApplyOptions, minStart int) (match, error) {
	decl := h.OldStart - 1
	spanCount := h.OldLines
	if spanCount == 0 && h.OldStart == 0 {
		// pure insertion with unusual encoding; normalise to append-ish
		decl = 0
	}

	// Stage 1: exact match at declared position.
	if decl >= minStart && decl+spanCount <= t.N() {
		if ok, err := patternMatchesAt(ctx, t, h.Body, decl, opts, true); err != nil {
			return match{}, err
		} else if ok {
			return match{start: decl, span: spanCount}, nil
		}
	}

	// Stage 2: exact full-pattern search within offset window.
	if m, amb, err := searchWindow(ctx, t, h.Body, opts, decl, spanCount, maxOff, minStart, true); err != nil {
		return match{}, err
	} else if amb {
		return match{}, &AmbiguousMatchError{Hunk: idx, Candidates: ambCandidates(t, h.Body, decl, spanCount, maxOff, minStart, opts, true)}
	} else if m != nil {
		return *m, nil
	}

	// Stage 3: fuzz search (context stripped from both ends). The declared
	// hunk start and its span stay the full window; only matching relaxes.
	if opts.Fuzz > 0 {
		pat := buildPattern(h, opts.Fuzz)
		if pat.span > 0 {
			m, amb, err := searchFuzz(ctx, t, pat, decl, spanCount, maxOff, minStart, opts)
			if err != nil {
				return match{}, err
			}
			if amb {
				cands := fuzzCandidates(t, pat, decl, spanCount, maxOff, minStart, opts)
				return match{}, &AmbiguousMatchError{Hunk: idx, Candidates: cands}
			}
			if m != nil {
				return match{start: *m, span: spanCount}, nil
			}
		}
	}

	// Nothing: distinguish offset-exceeded from out-of-bounds.
	if decl < 0 || decl > t.N() || h.OldStart-1+spanCount > t.N() {
		// only call it bounds if even an unlimited search cannot match
		if maxOff != -1 {
			if m, _, _ := searchWindow(ctx, t, h.Body, opts, decl, spanCount, -1, minStart, true); m != nil {
				return match{}, &OffsetExceededError{Hunk: idx, Want: h.OldStart, Offset: absInt(m.start - decl), MaxOffset: maxOff}
			}
		}
		return match{}, &BoundsError{Hunk: idx, Msg: "hunk content not present in target"}
	}
	return match{}, &OffsetExceededError{Hunk: idx, Want: h.OldStart, Offset: maxOff, MaxOffset: maxOff}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// patternMatchesAt checks the full ordered hunk body against the target
// starting at pos. Inserted (+) lines are skipped; their ordering relative to
// removals is fixed by the normalised hunk structure.
func patternMatchesAt(ctx context.Context, t *Spool, body []HunkLine, pos int, opts ApplyOptions, full bool) (bool, error) {
	return patternStructMatchesAt(ctx, t, hunkPattern{kinds: kindsOf(body), lines: linesOf(body), span: spanOf(body)}, pos, opts)
}

func kindsOf(body []HunkLine) []HunkLineKind {
	k := make([]HunkLineKind, len(body))
	for i, hl := range body {
		k[i] = hl.Kind
	}
	return k
}
func linesOf(body []HunkLine) []Line {
	l := make([]Line, len(body))
	for i, hl := range body {
		l[i] = hl.Line
	}
	return l
}
func spanOf(body []HunkLine) int {
	n := 0
	for _, hl := range body {
		if hl.Kind != HunkNew {
			n++
		}
	}
	return n
}

func patternStructMatchesAt(ctx context.Context, t *Spool, pat hunkPattern, pos int, opts ApplyOptions) (bool, error) {
	if pos+pat.span > t.N() || pos < 0 {
		return false, nil
	}
	tp := pos
	for i, kind := range pat.kinds {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if kind == HunkNew {
			continue
		}
		tl, err := t.Line(tp)
		if err != nil {
			return false, err
		}
		if !LinesEqual(tl, pat.lines[i], opts.Compare) {
			return false, nil
		}
		tp++
	}
	return true, nil
}

// searchWindow returns the unique match inside the offset window, or
// amb==true when at least two matches exist.
func searchWindow(ctx context.Context, t *Spool, body []HunkLine, opts ApplyOptions, decl, span, maxOff, minStart int, full bool) (*match, bool, error) {
	pat := hunkPattern{kinds: kindsOf(body), lines: linesOf(body), span: span}
	return searchWindowStruct(ctx, t, pat, opts, decl, maxOff, minStart)
}

func searchWindowStruct(ctx context.Context, t *Spool, pat hunkPattern, opts ApplyOptions, decl, maxOff, minStart int) (*match, bool, error) {
	lo, hi := windowBounds(t.N(), pat.span, decl, maxOff, minStart)
	var found *match
	count := 0
	for pos := lo; pos <= hi; pos++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		ok, err := patternStructMatchesAt(ctx, t, pat, pos, opts)
		if err != nil {
			return nil, false, err
		}
		if ok {
			count++
			if found == nil {
				m := match{start: pos, span: pat.span}
				found = &m
			}
			if count >= 2 {
				return nil, true, nil
			}
		}
	}
	return found, false, nil
}

// searchFuzz scans candidate full-window starts; for each, the retained
// pattern is tested at windowStart+lead. Returns the unique window start.
func searchFuzz(ctx context.Context, t *Spool, pat hunkPattern, decl, spanCount, maxOff, minStart int, opts ApplyOptions) (*int, bool, error) {
	lo, hi := windowBounds(t.N(), spanCount, decl, maxOff, minStart)
	var found *int
	count := 0
	for ws := lo; ws <= hi; ws++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		ok, err := patternStructMatchesAt(ctx, t, pat, ws+pat.lead, opts)
		if err != nil {
			return nil, false, err
		}
		if ok {
			count++
			if found == nil {
				v := ws
				found = &v
			}
			if count >= 2 {
				return nil, true, nil
			}
		}
	}
	return found, false, nil
}

func fuzzCandidates(t *Spool, pat hunkPattern, decl, spanCount, maxOff, minStart int, opts ApplyOptions) []int {
	lo, hi := windowBounds(t.N(), spanCount, decl, maxOff, minStart)
	var out []int
	for ws := lo; ws <= hi; ws++ {
		ok, _ := patternStructMatchesAt(context.Background(), t, pat, ws+pat.lead, opts)
		if ok {
			out = append(out, ws+1)
		}
	}
	return out
}

func windowBounds(n, span, decl, maxOff, minStart int) (lo, hi int) {
	lo = decl - maxOff
	hi = decl + maxOff
	if maxOff == -1 {
		lo = 0
		hi = n - span
	}
	if lo < 0 {
		lo = 0
	}
	if lo < minStart {
		lo = minStart
	}
	if hi > n-span {
		hi = n - span
	}
	if hi < lo {
		return 1, 0 // empty window
	}
	return lo, hi
}

func ambCandidates(t *Spool, body []HunkLine, decl, span, maxOff, minStart int, opts ApplyOptions, full bool) []int {
	pat := hunkPattern{kinds: kindsOf(body), lines: linesOf(body), span: span}
	return ambCandidatesStruct(t, pat, decl, maxOff, minStart, opts)
}

func ambCandidatesStruct(t *Spool, pat hunkPattern, decl, maxOff, minStart int, opts ApplyOptions) []int {
	lo, hi := windowBounds(t.N(), pat.span, decl, maxOff, minStart)
	var out []int
	for pos := lo; pos <= hi; pos++ {
		ok, _ := patternStructMatchesAt(context.Background(), t, pat, pos, opts)
		if ok {
			out = append(out, pos+1)
		}
	}
	return out
}

// emitPatched writes the output after every hunk has been located.
func emitPatched(ctx context.Context, t *Spool, p *Patch, ms []match, w io.Writer, opts ApplyOptions) error {
	cursor := 0
	for i, h := range p.Hunks {
		m := ms[i]
		for ; cursor < m.start; cursor++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			l, err := t.Line(cursor)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(w, l.Full()); err != nil {
				return err
			}
		}
		tp := m.start
		for _, hl := range h.Body {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch hl.Kind {
			case HunkContext:
				l, err := t.Line(tp)
				if err != nil {
					return err
				}
				if _, err := io.WriteString(w, l.Full()); err != nil {
					return err
				}
				tp++
				cursor++
			case HunkOld:
				// removed line is skipped in output
				tp++
				cursor++
			case HunkNew:
				if _, err := io.WriteString(w, hl.Line.Full()); err != nil {
					return err
				}
			}
		}
	}
	for ; cursor < t.N(); cursor++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		l, err := t.Line(cursor)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, l.Full()); err != nil {
			return err
		}
	}
	return nil
}

// ApplyReverse applies the reversed patch (undo). On failure out receives no
// bytes (computation completes before output); the caller may safely discard
// the destination.
func ApplyReverse(ctx context.Context, target io.Reader, patch *Patch, out io.Writer, opts ApplyOptions) (*ApplyResult, error) {
	return Apply(ctx, target, patch.Reverse(), out, opts)
}

// ApplyToFile atomically replaces path with the patched content: output goes
// to a sibling temporary file which is fsynced and renamed only on success.
// Cancellation, timeout or any error leaves the original file untouched and
// removes the temporary file.
func ApplyToFile(ctx context.Context, path string, patch *Patch, reverse bool, opts ApplyOptions) (*ApplyResult, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".diffmerge-*")
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	var applied *Patch
	if reverse {
		applied = patch.Reverse()
	} else {
		applied = patch
	}
	res, applyErr := Apply(ctx, in, applied, tmp, opts)
	_ = in.Close()
	if applyErr != nil {
		cleanup()
		return nil, applyErr
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return nil, err
	}
	// fsync directory so the rename is durable; best-effort on platforms
	// without directory fsync.
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return res, nil
}
