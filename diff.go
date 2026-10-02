package diffmerge

import (
	"context"
)

// EditOp identifies the kind of an edit script element.
type EditOp int

const (
	EditEqual EditOp = iota
	EditDelete
	EditInsert
)

func (o EditOp) String() string {
	switch o {
	case EditEqual:
		return "equal"
	case EditDelete:
		return "delete"
	case EditInsert:
		return "insert"
	default:
		return "unknown"
	}
}

// Edit is one aligned block in a diff script.
//
// For EditEqual, AStart/Count describe the block in the old version and
// BStart/Count the same block in the new version (Count is identical).
// For EditDelete only AStart/Count are meaningful; for EditInsert only
// BStart/Count are meaningful.
type Edit struct {
	Op     EditOp
	AStart int // 0-based start in old
	BStart int // 0-based start in new
	Count  int
}

// DiffOptions controls diff generation.
type DiffOptions struct {
	Compare LineCompareOptions
}

// DiffStats summarises a generated diff.
type DiffStats struct {
	OldLines int
	NewLines int
	Hunks    int
}

// equalLines compares two indexed lines: hashes first, full comparison on
// collision. Equal hash + unequal content therefore never yields a wrong
// alignment.
func equalLines(a, b RandomLines, i, j int) bool {
	if a.Hash(i) != b.Hash(j) {
		return false
	}
	la, err := a.Line(i)
	if err != nil {
		return false
	}
	lb, err := b.Line(j)
	if err != nil {
		return false
	}
	return LinesEqual(la, lb, a.Opts())
}

// ScriptEdits computes a shortest edit script between two indexed line sets
// using the linear-space (middle snake) variant of Myers' algorithm.
//
// Memory use is O(N+M) integers plus recursion state; line *content* is
// accessed through RandomLines and may itself be spooled to disk.
func ScriptEdits(ctx context.Context, a, b RandomLines) ([]Edit, error) {
	eq := func(i, j int) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return equalLines(a, b, i, j), nil
	}
	eb := &editBuilder{}
	if err := myersLCS(eq, eb, 0, a.N(), 0, b.N()); err != nil {
		return nil, err
	}
	return eb.edits(), nil
}

// InMemoryEdits is a convenience wrapper that materialises both inputs.
func InMemoryEdits(ctx context.Context, oldSrc, newSrc LineSource, opts DiffOptions) ([]Edit, []Line, []Line, error) {
	oldLines, err := ReadAllLines(ctx, oldSrc)
	if err != nil {
		return nil, nil, nil, err
	}
	newLines, err := ReadAllLines(ctx, newSrc)
	if err != nil {
		return nil, nil, nil, err
	}
	a := NewMemoryLines(oldLines, opts.Compare)
	b := NewMemoryLines(newLines, opts.Compare)
	edits, err := ScriptEdits(ctx, a, b)
	if err != nil {
		return nil, nil, nil, err
	}
	return edits, oldLines, newLines, nil
}

// editBuilder records the alignment as per-line tags and coalesces them into
// Edit blocks.
type editBuilder struct {
	tags []byte // '=', '-' (delete a), '+' (insert b)
}

func (e *editBuilder) equal(n int) {
	for k := 0; k < n; k++ {
		e.tags = append(e.tags, '=')
	}
}

func (e *editBuilder) del(n int) {
	for k := 0; k < n; k++ {
		e.tags = append(e.tags, '-')
	}
}

func (e *editBuilder) ins(n int) {
	for k := 0; k < n; k++ {
		e.tags = append(e.tags, '+')
	}
}

func (e *editBuilder) edits() []Edit {
	if len(e.tags) == 0 {
		return nil
	}
	var out []Edit
	a, b := 0, 0
	i := 0
	for i < len(e.tags) {
		t := e.tags[i]
		j := i
		for j < len(e.tags) && e.tags[j] == t {
			j++
		}
		count := j - i
		switch t {
		case '=':
			out = append(out, Edit{Op: EditEqual, AStart: a, BStart: b, Count: count})
			a += count
			b += count
		case '-':
			out = append(out, Edit{Op: EditDelete, AStart: a, BStart: b, Count: count})
			a += count
		case '+':
			out = append(out, Edit{Op: EditInsert, AStart: a, BStart: b, Count: count})
			b += count
		}
		i = j
	}
	return out
}

// myersLCS emits the alignment of a[a0:a0+N] vs b[b0:b0+M].
// It implements the linear-space Myers recursion (Myers, 1986, Fig. 2).
func myersLCS(eq func(i, j int) (bool, error), out *editBuilder, a0, N, b0, M int) error {
	switch {
	case N > 0 && M > 0:
		// proceed below
	case N > 0:
		out.del(N)
		return nil
	case M > 0:
		out.ins(M)
		return nil
	default:
		return nil
	}

	eqR := func(x, y int) (bool, error) { return eq(a0+x, b0+y) }

	d, x1, y1, x2, y2, err := middleSnake(N, M, eqR)
	if err != nil {
		return err
	}
	if d > 1 {
		if err := myersLCS(eq, out, a0, x1, b0, y1); err != nil {
			return err
		}
		out.equal(x2 - x1)
		if err := myersLCS(eq, out, a0+x2, N-x2, b0+y2, M-y2); err != nil {
			return err
		}
		return nil
	}
	if d == 0 {
		out.equal(N)
		return nil
	}
	// d == 1: exactly one deletion or one insertion. Emit the common
	// prefix, the single edit, and the common suffix.
	i := 0
	for i < N && i < M {
		ok, err := eqR(i, i)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		i++
	}
	if N > M {
		out.equal(i)
		out.del(1)
		out.equal(N - i - 1)
	} else {
		out.equal(i)
		out.ins(1)
		out.equal(M - i - 1)
	}
	return nil
}

// middleSnake returns the edit distance d of the subproblem and the endpoints
// (x1,y1)->(x2,y2) of a middle diagonal snake, in coordinates relative to
// the subproblem.
func middleSnake(N, M int, eq func(x, y int) (bool, error)) (d, x1, y1, x2, y2 int, err error) {
	const maxStep = 1 << 30
	delta := N - M
	odd := delta&1 != 0
	// Backward kappa reaches |delta| + D before overlap is guaranteed;
	// allocate for 2*(N+M)+2 on both sides and the +1 transition neighbours.
	off := 2*(N+M) + 2
	vf := make([]int, 2*off+2)
	vb := make([]int, 2*off+2)
	vf[1+off] = 0
	vb[delta-1+off] = N

	getF := func(k int) int { return vf[k+off] }
	setF := func(k, v int) { vf[k+off] = v }
	getB := func(k int) int { return vb[k+off] }
	setB := func(k, v int) { vb[k+off] = v }

	for D := 0; D <= maxStep; D++ {
		// Forward pass.
		for k := -D; k <= D; k += 2 {
			var x int
			if k == -D || (k != D && getF(k-1) < getF(k+1)) {
				x = getF(k + 1)
			} else {
				x = getF(k-1) + 1
			}
			y := x - k
			sx, sy := x, y
			for x < N && y < M {
				ok, err := eq(x, y)
				if err != nil {
					return 0, 0, 0, 0, 0, err
				}
				if !ok {
					break
				}
				x++
				y++
			}
			setF(k, x)
			if odd && k >= delta-(D-1) && k <= delta+(D-1) && x >= getB(k) {
				return 2*D - 1, sx, sy, x, y, nil
			}
		}
		// Backward pass.
		for k := -D; k <= D; k += 2 {
			K := delta + k
			var x int
			if K == delta+D || (K != delta-D && getB(K-1) < getB(K+1)) {
				x = getB(K - 1)
			} else {
				x = getB(K+1) - 1
			}
			y := x - K
			ex, ey := x, y
			for x > 0 && y > 0 {
				ok, err := eq(x-1, y-1)
				if err != nil {
					return 0, 0, 0, 0, 0, err
				}
				if !ok {
					break
				}
				x--
				y--
			}
			setB(K, x)
			if !odd && K >= -D && K <= D && x <= getF(K) {
				return 2 * D, x, y, ex, ey, nil
			}
		}
	}
	return // unreachable
}
