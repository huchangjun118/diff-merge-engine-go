package diffmerge

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

func mustLines(t *testing.T, text string) []Line {
	t.Helper()
	src := NewLineSource(strReader(text), StreamOptions{})
	ls, err := ReadAllLines(context.Background(), src)
	if err != nil {
		t.Fatalf("read lines: %v", err)
	}
	return ls
}

func dpSESLen(a, b []string) int {
	n, m := len(a), len(b)
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1]
				continue
			}
			d := prev[j] + 1
			if cur[j-1]+1 < d {
				d = cur[j-1] + 1
			}
			cur[j] = d
		}
		prev, cur = cur, prev
	}
	return prev[m]
}

func validateEdits(t *testing.T, edits []Edit, a, b []Line) {
	t.Helper()
	var rebuilt []Line
	for _, e := range edits {
		switch e.Op {
		case EditEqual:
			for k := 0; k < e.Count; k++ {
				rebuilt = append(rebuilt, a[e.AStart+k])
			}
		case EditDelete:
		case EditInsert:
			for k := 0; k < e.Count; k++ {
				rebuilt = append(rebuilt, b[e.BStart+k])
			}
		}
	}
	if len(rebuilt) != len(b) {
		t.Fatalf("reconstruction length %d != %d", len(rebuilt), len(b))
	}
	for i := range rebuilt {
		if rebuilt[i] != b[i] {
			t.Fatalf("reconstruction mismatch at %d: %q != %q", i, rebuilt[i], b[i])
		}
	}
	// Check boundary coverage of a and b.
	if len(edits) > 0 {
		var ac, bc int
		for _, e := range edits {
			if e.Op != EditInsert {
				ac += e.Count
			}
			if e.Op != EditDelete {
				bc += e.Count
			}
		}
		if ac != len(a) || bc != len(b) {
			t.Fatalf("coverage a=%d/%d b=%d/%d", ac, len(a), bc, len(b))
		}
	}
	// SES minimality.
	as := make([]string, len(a))
	bs := make([]string, len(b))
	for i, l := range a {
		as[i] = l.Full()
	}
	for i, l := range b {
		bs[i] = l.Full()
	}
	want := dpSESLen(as, bs)
	var ses int
	for _, e := range edits {
		if e.Op != EditEqual {
			ses += e.Count
		}
	}
	if ses != want {
		t.Fatalf("SES length %d, DP says %d\nedits=%v\na=%q\nb=%q", ses, want, edits, as, bs)
	}
}

func TestDiffHandcrafted(t *testing.T) {
	cases := []struct{ a, b string }{
		{"", ""},
		{"a\n", "a\n"},
		{"a\n", "b\n"},
		{"a\nb\nc\n", "a\nc\n"},
		{"a\nb\nc\n", "a\nB\nc\n"},
		{"a\nb\nc\nd\n", "a\nX\nc\nY\n"},
		{"x\na\nb\nc\n", "a\nb\nc\nd\n"},

		{"no newline", "no newline too"},
		{"a\nc\n", "a\nc\nd\n"},
	}
	for i, c := range cases {
		a := mustLines(t, c.a)
		b := mustLines(t, c.b)
		edits, err := ScriptEdits(context.Background(), NewMemoryLines(a, LineCompareOptions{}), NewMemoryLines(b, LineCompareOptions{}))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		validateEdits(t, edits, a, b)
	}
}

func TestDiffRandomSES(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	vocab := []string{"a\n", "b\n", "c\n", "d\n"}
	for iter := 0; iter < 400; iter++ {
		mk := func() []Line {
			n := rng.Intn(14)
			out := make([]Line, n)
			for i := range out {
				out[i] = Line{Text: vocab[rng.Intn(len(vocab))][:1], EOL: EOLUnix}
			}
			return out
		}
		a := mk()
		b := mk()
		edits, err := ScriptEdits(context.Background(), NewMemoryLines(a, LineCompareOptions{}), NewMemoryLines(b, LineCompareOptions{}))
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		validateEdits(t, edits, a, b)
	}
}

func TestDiffCRLFAndWhitespace(t *testing.T) {
	a := mustLines(t, "foo \nbar\r\nbaz\n")
	b := mustLines(t, "foo\nbar\nbaz  \n")
	opts := LineCompareOptions{NormalizeEOL: true, IgnoreTrailingWhitespace: true}
	edits, err := ScriptEdits(context.Background(), NewMemoryLines(a, opts), NewMemoryLines(b, opts))
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].Op != EditEqual || edits[0].Count != 3 {
		t.Fatalf("expected one equal block, got %v", edits)
	}
}

func TestDiffCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := make([]Line, 10)
	for i := range a {
		a[i] = Line{Text: fmt.Sprintf("l%d", i), EOL: EOLUnix}
	}
	_, err := ScriptEdits(ctx, NewMemoryLines(a, LineCompareOptions{}), NewMemoryLines(a, LineCompareOptions{}))
	if err == nil {
		t.Fatal("expected context error")
	}
}
