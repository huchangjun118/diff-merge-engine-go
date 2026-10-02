package diffmerge

import (
	"context"
	"io"
)

// ConflictInfo describes one unresolvable region of a three-way merge.
type ConflictInfo struct {
	BaseStart   int
	BaseLines   int
	OurStart    int
	OurLines    int
	TheirStart  int
	TheirLines  int
	ResultStart int
	ResultEnd   int
}

// ConflictError is a successful merge computation that found conflicts.
type ConflictError struct{ Conflicts []ConflictInfo }

func (e *ConflictError) Error() string {
	return "merge conflicts in " + itoa(len(e.Conflicts)) + " region(s)"
}

// MergeOptions controls three-way merging.
type MergeOptions struct {
	Compare        LineCompareOptions
	MaxMemoryBytes int64
	OurLabel       string
	TheirLabel     string
}

// MergeResult summarises a merge.
type MergeResult struct {
	OutputLines int
	Conflicts   []ConflictInfo
}

type chunkKind int

const (
	chunkStable chunkKind = iota
	chunkOnlyOurs
	chunkOnlyTheirs
	chunkConflict
)

type chunk struct {
	kind           chunkKind
	baseS, baseN   int
	ourS, ourN     int
	theirS, theirN int
}

// Merge3 performs a three-way merge; see README for conflict semantics.
func Merge3(ctx context.Context, base, ours, theirs io.Reader, w io.Writer, opts MergeOptions) (*MergeResult, error) {
	budget := opts.MaxMemoryBytes
	if budget <= 0 {
		budget = defaultInMemoryBounds
	}
	ts, err := spoolThree(ctx, base, ours, theirs, budget, opts.Compare)
	if err != nil {
		return nil, err
	}
	defer ts.closeAll()
	out, conflicts, err := mergeSpooled(ctx, ts, opts)
	if err != nil {
		return nil, err
	}
	if err := writeMergeOutput(w, out); err != nil {
		return nil, err
	}
	res := &MergeResult{OutputLines: len(out), Conflicts: conflicts}
	if len(conflicts) > 0 {
		return res, &ConflictError{Conflicts: conflicts}
	}
	return res, nil
}

type threeSpool struct{ base, ours, theirs *Spool }

func (t *threeSpool) closeAll() {
	_ = t.base.Close()
	_ = t.ours.Close()
	_ = t.theirs.Close()
}

func spoolThree(ctx context.Context, base, ours, theirs io.Reader, budget int64, cmp LineCompareOptions) (*threeSpool, error) {
	mk := func(r io.Reader) (*Spool, error) {
		src := NewLineSource(r, StreamOptions{BufferSize: defaultReadBuffer})
		sp, err := SpoolLines(ctx, src, budget)
		if err != nil {
			return nil, err
		}
		sp.SetCompare(cmp)
		return sp, nil
	}
	ts := &threeSpool{}
	var err error
	if ts.base, err = mk(base); err != nil {
		return nil, err
	}
	if ts.ours, err = mk(ours); err != nil {
		_ = ts.base.Close()
		return nil, err
	}
	if ts.theirs, err = mk(theirs); err != nil {
		_ = ts.base.Close()
		_ = ts.ours.Close()
		return nil, err
	}
	return ts, nil
}

// Merge3Lines performs the merge on indexed line sets.
func Merge3Lines(ctx context.Context, base, ours, theirs RandomLines, opts MergeOptions) ([]Line, []ConflictInfo, error) {
	chunks, err := buildChunks(ctx, base, ours, theirs)
	if err != nil {
		return nil, nil, err
	}
	return renderChunks(ours, theirs, chunks, opts)
}

func mergeSpooled(ctx context.Context, sp *threeSpool, opts MergeOptions) ([]Line, []ConflictInfo, error) {
	chunks, err := buildChunks(ctx, sp.base, sp.ours, sp.theirs)
	if err != nil {
		return nil, nil, err
	}
	return renderChunks(sp.ours, sp.theirs, chunks, opts)
}

// ---- Alignment ---------------------------------------------------------

// mBlock is one exhaustive block mapping a base interval to a side interval.
// op: '=' unchanged, 'x' changed (replacement/insert/delete).
type mBlock struct {
	b0, b1 int // base range [b0,b1); equal for a pure insert
	s0, s1 int // side range [s0,s1)
	op     byte
}

func buildBlocks(ctx context.Context, base, side RandomLines) ([]mBlock, error) {
	edits, err := ScriptEdits(ctx, base, side)
	if err != nil {
		return nil, err
	}
	var raw []mBlock
	bp, sp := 0, 0
	padEqual := func(targetBase int) {
		n := targetBase - bp
		if n > 0 {
			raw = append(raw, mBlock{b0: bp, b1: bp + n, s0: sp, s1: sp + n, op: '='})
			bp += n
			sp += n
		}
	}
	for _, e := range edits {
		switch e.Op {
		case EditEqual:
			padEqual(e.AStart)
			raw = append(raw, mBlock{b0: bp, b1: bp + e.Count, s0: sp, s1: sp + e.Count, op: '='})
			bp += e.Count
			sp += e.Count
		case EditDelete:
			padEqual(e.AStart)
			raw = append(raw, mBlock{b0: bp, b1: bp + e.Count, s0: sp, s1: sp, op: 'x'})
			bp += e.Count
		case EditInsert:
			padEqual(e.AStart)
			raw = append(raw, mBlock{b0: bp, b1: bp, s0: sp, s1: sp + e.Count, op: 'x'})
			sp += e.Count
		}
	}
	padEqual(base.N())
	return pairInsertsAndDeletes(raw), nil
}

// pairInsertsAndDeletes folds adjacent insert/delete blocks at the same base
// anchor into one replacement block spanning the deleted base range.
func pairInsertsAndDeletes(blocks []mBlock) []mBlock {
	var out []mBlock
	for i := 0; i < len(blocks); i++ {
		b := blocks[i]
		if b.op == 'x' && b.b0 == b.b1 && i+1 < len(blocks) {
			n := blocks[i+1]
			if n.op == 'x' && n.b1 > n.b0 && n.b0 == b.b0 {
				out = append(out, mBlock{b0: n.b0, b1: n.b1, s0: b.s0, s1: b.s1, op: 'x'})
				i++
				continue
			}
		}
		if b.op == 'x' && b.b1 > b.b0 && i+1 < len(blocks) {
			// delete followed by insert (unusual ordering): also pair
			n := blocks[i+1]
			if n.op == 'x' && n.b0 == n.b1 && n.b0 == b.b1 {
				out = append(out, mBlock{b0: b.b0, b1: b.b1, s0: b.s0, s1: n.s1, op: 'x'})
				i++
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// buildChunks aligns both sides' block streams on the shared base axis and
// classifies each maximal interval. The two streams are merged by base
// boundary like two sorted interval sequences, splitting blocks at every
// boundary introduced by the other side.
func buildChunks(ctx context.Context, base, ours, theirs RandomLines) ([]chunk, error) {
	bo, err := buildBlocks(ctx, base, ours)
	if err != nil {
		return nil, err
	}
	bt, err := buildBlocks(ctx, base, theirs)
	if err != nil {
		return nil, err
	}
	// Build base-axis cuts from every block boundary on both sides; zero
	// width inserts contribute the same point twice (deduped by the map).
	cuts := map[int]struct{}{0: {}, base.N(): {}}
	add := func(bs []mBlock) {
		for _, b := range bs {
			cuts[b.b0] = struct{}{}
			cuts[b.b1] = struct{}{} // zero-width insert contributes b0 only
		}
	}
	add(bo)
	add(bt)
	pts := make([]int, 0, len(cuts))
	for c := range cuts {
		pts = append(pts, c)
	}
	sortInts(pts)

	var chunks []chunk
	prev := pts[0]
	for _, p := range pts[1:] {
		// Zero-width interval at prev captures inserts anchored exactly there
		// that are not absorbed by a wide interval starting at prev.
		if zc := insertOnlyChunk(ctx, bo, bt, prev, ours, theirs); zc != nil {
			chunks = append(chunks, *zc)
		}
		if p > prev {
			c, err := classifyInterval(ctx, base, ours, theirs, bo, bt, prev, p)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, c...)
		}
		prev = p
	}
	// inserts anchored at the final boundary (after last base line)
	if zc := insertOnlyChunk(ctx, bo, bt, pts[len(pts)-1], ours, theirs); zc != nil {
		chunks = append(chunks, *zc)
	}
	return mergeAdjacent(chunks), nil
}

// insertOnlyChunk handles pure inserts anchored at base boundary anchor when
// no positive-width interval starts at the same anchor on that side. It
// returns nil if there is nothing to emit or the insert is already covered by
// a wide interval.
func insertOnlyChunk(ctx context.Context, bo, bt []mBlock, anchor int, ours, theirs RandomLines) *chunk {
	var io2, it mBlock
	hasO, hasT := false, false
	for _, b := range bo {
		if b.b0 == b.b1 && b.b0 == anchor {
			io2 = b
			hasO = true
		}
	}
	for _, b := range bt {
		if b.b0 == b.b1 && b.b0 == anchor {
			it = b
			hasT = true
		}
	}
	if !hasO && !hasT {
		return nil
	}
	o0, o1 := io2.s0, io2.s1
	t0, t1 := it.s0, it.s1
	if !hasO {
		o0, o1 = boundaryCursor(bo, anchor)
	}
	if !hasT {
		t0, t1 = boundaryCursor(bt, anchor)
	}
	k := chunkStable
	switch {
	case hasO && !hasT:
		k = chunkOnlyOurs
	case !hasO && hasT:
		k = chunkOnlyTheirs
	case hasO && hasT:
		if o1-o0 == t1-t0 && rangesEqual(ctx, ours, o0, theirs, t0, o1-o0) {
			k = chunkStable
		} else {
			k = chunkConflict
		}
	}
	return &chunk{kind: k, baseS: anchor, baseN: 0, ourS: o0, ourN: o1 - o0, theirS: t0, theirN: t1 - t0}
}

// boundaryCursor returns the side index aligned at base boundary anchor.
func boundaryCursor(bs []mBlock, anchor int) (int, int) {
	for _, b := range bs {
		if b.b0 == anchor && b.b1 > b.b0 {
			return b.s0, b.s0
		}
		if b.b1 == anchor {
			return b.s1, b.s1
		}
	}
	return 0, 0
}

// blockAt returns the block in bs covering base interval [p0,p1), and the
// side range mapped to that interval (including zero-width inserts at p0).
func covering(bs []mBlock, p0, p1 int) []mBlock {
	var out []mBlock
	for _, b := range bs {
		if b.b0 == b.b1 {
			if b.b0 >= p0 && b.b0 < p1 {
				out = append(out, b)
			}
			continue
		}
		if b.b0 < p1 && b.b1 > p0 {
			out = append(out, b)
		}
	}
	return out
}

// sideRangeOf computes the side range contributed by blocks over base
// interval [p0,p1). Equal blocks intersecting the interval contribute their
// intersecting sub-range; changed blocks contribute fully; zero-width inserts
// anchored inside contribute their side lines.
func sideRangeOf(bs []mBlock, p0, p1 int) (int, int) {
	s0, s1 := -1, -1
	note := func(a, b int) {
		if s0 < 0 || a < s0 {
			s0 = a
		}
		if b > s1 {
			s1 = b
		}
	}
	for _, blk := range covering(bs, p0, p1) {
		if blk.b0 == blk.b1 {
			continue // standalone inserts are emitted as their own chunks
		}
		// intersection offset within the block (positive-width blocks are
		// unchanged for '=' so side offset tracks base offset; for changed
		// blocks the whole side range belongs to the block)
		if blk.op == '=' {
			lo := maxInt(p0, blk.b0)
			hi := minInt(p1, blk.b1)
			off0 := lo - blk.b0
			off1 := hi - blk.b0
			note(blk.s0+off0, blk.s0+off1)
		} else {
			note(blk.s0, blk.s1)
		}
	}
	if s0 < 0 {
		return 0, 0
	}
	return s0, s1
}

// classifyInterval produces one or two chunks for a positive-width or
// zero-width (insert-only) base interval between consecutive cut points.
func classifyInterval(ctx context.Context, base, ours, theirs RandomLines, bo, bt []mBlock, p0, p1 int) ([]chunk, error) {
	// First emit any zero-width insert chunks anchored exactly at p0 (both
	// sides may insert here) that are not otherwise part of a wide block.
	var out []chunk
	insO := insertsAt(bo, p0)
	insT := insertsAt(bt, p0)
	if p1 == p0 {
		return out, nil
	}
	o0, o1 := sideRangeOf(bo, p0, p1)
	t0, t1 := sideRangeOf(bt, p0, p1)
	wideChangedO := hasChanged(bo, p0, p1)
	wideChangedT := hasChanged(bt, p0, p1)
	// "changed" means the wide interval is not identical to base.
	changedO := wideChangedO || !regionMatchesBase(ctx, base, p0, p1, ours, o0, o1)
	changedT := wideChangedT || !regionMatchesBase(ctx, base, p0, p1, theirs, t0, t1)
	same := (o1-o0 == t1-t0) && rangesEqual(ctx, ours, o0, theirs, t0, o1-o0)
	k := chunkStable
	switch {
	case !changedO && !changedT:
		k = chunkStable
	case changedO && !changedT:
		k = chunkOnlyOurs
	case !changedO && changedT:
		k = chunkOnlyTheirs
	case same:
		k = chunkStable
	default:
		k = chunkConflict
	}
	out = append(out, chunk{kind: k, baseS: p0, baseN: p1 - p0, ourS: o0, ourN: o1 - o0, theirS: t0, theirN: t1 - t0})
	// Inserts anchored at p0 that belong to neither side's wide contribution
	// are already included via sideRangeOf only when the interval is a change
	// region; for a stable interval they must still be checked above. Keep
	// ins vars referenced for clarity.
	_ = insO
	_ = insT
	return out, nil
}

func insertsAt(bs []mBlock, p int) []mBlock {
	var out []mBlock
	for _, b := range bs {
		if b.b0 == b.b1 && b.b0 == p {
			out = append(out, b)
		}
	}
	return out
}

func hasChanged(bs []mBlock, p0, p1 int) bool {
	for _, b := range covering(bs, p0, p1) {
		if b.op == 'x' {
			return true
		}
	}
	return false
}

func regionMatchesBase(ctx context.Context, base RandomLines, b0, b1 int, side RandomLines, s0, s1 int) bool {
	if b1-b0 != s1-s0 {
		return false
	}
	return rangesEqual(ctx, base, b0, side, s0, b1-b0)
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func rangesEqual(ctx context.Context, a RandomLines, a0 int, b RandomLines, b0, n int) bool {
	if a0+n > a.N() || b0+n > b.N() || n < 0 {
		return false
	}
	for k := 0; k < n; k++ {
		if err := ctx.Err(); err != nil {
			return false
		}
		if !equalLines(a, b, a0+k, b0+k) {
			return false
		}
	}
	return true
}

func mergeAdjacent(in []chunk) []chunk {
	if len(in) == 0 {
		return in
	}
	out := []chunk{in[0]}
	for i := 1; i < len(in); i++ {
		c := in[i]
		prev := &out[len(out)-1]
		if c.kind == prev.kind && c.baseS == prev.baseS+prev.baseN {
			prev.baseN += c.baseN
			prev.ourN = c.ourS + c.ourN - prev.ourS
			prev.theirN = c.theirS + c.theirN - prev.theirS
			continue
		}
		out = append(out, c)
	}
	return out
}

// renderChunks converts classified chunks into output lines and conflicts.
func renderChunks(ours, theirs RandomLines, chunks []chunk, opts MergeOptions) ([]Line, []ConflictInfo, error) {
	ourLabel := opts.OurLabel
	if ourLabel == "" {
		ourLabel = "ours"
	}
	theirLabel := opts.TheirLabel
	if theirLabel == "" {
		theirLabel = "theirs"
	}
	var out []Line
	var conflicts []ConflictInfo
	outLine := 1
	cp := func(src RandomLines, s, n int) error {
		for k := 0; k < n; k++ {
			l, err := src.Line(s + k)
			if err != nil {
				return err
			}
			out = append(out, l)
			outLine++
		}
		return nil
	}
	for _, c := range chunks {
		switch c.kind {
		case chunkStable:
			if c.ourN > 0 {
				if err := cp(ours, c.ourS, c.ourN); err != nil {
					return nil, nil, err
				}
			} else {
				if err := cp(theirs, c.theirS, c.theirN); err != nil {
					return nil, nil, err
				}
			}
		case chunkOnlyOurs:
			if err := cp(ours, c.ourS, c.ourN); err != nil {
				return nil, nil, err
			}
		case chunkOnlyTheirs:
			if err := cp(theirs, c.theirS, c.theirN); err != nil {
				return nil, nil, err
			}
		case chunkConflict:
			start := outLine
			out = append(out, Line{Text: "<<<<<<< " + ourLabel, EOL: EOLUnix})
			outLine++
			if err := cp(ours, c.ourS, c.ourN); err != nil {
				return nil, nil, err
			}
			out = append(out, Line{Text: "=======", EOL: EOLUnix})
			outLine++
			if err := cp(theirs, c.theirS, c.theirN); err != nil {
				return nil, nil, err
			}
			out = append(out, Line{Text: ">>>>>>> " + theirLabel, EOL: EOLUnix})
			outLine++
			conflicts = append(conflicts, ConflictInfo{
				BaseStart: nzStart(c.baseS, c.baseN), BaseLines: c.baseN,
				OurStart: nzStart(c.ourS, c.ourN), OurLines: c.ourN,
				TheirStart: nzStart(c.theirS, c.theirN), TheirLines: c.theirN,
				ResultStart: start, ResultEnd: outLine - 1,
			})
		}
	}
	return out, conflicts, nil
}

// nzStart renders a 1-based start, using the raw index for an empty range
// (pure insert) which reports 0.
func nzStart(s, n int) int {
	if n == 0 {
		return 0
	}
	return s + 1
}

func writeMergeOutput(w io.Writer, out []Line) error {
	for _, l := range out {
		if _, err := io.WriteString(w, l.Full()); err != nil {
			return err
		}
	}
	return nil
}
