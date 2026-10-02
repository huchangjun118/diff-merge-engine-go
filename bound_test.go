package diffmerge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestSpoolSpillsToDisk(t *testing.T) {
	// 5000 lines * ~20 bytes far exceeds a 64 KiB in-memory budget.
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "line number %05d content\n", i)
	}
	src := NewLineSource(strReader(b.String()), StreamOptions{})
	sp, err := SpoolLines(context.Background(), src, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if sp.N() != 5000 {
		t.Fatalf("N=%d", sp.N())
	}
	if sp.spillStart < 0 {
		t.Fatal("expected spill to disk")
	}
	l, err := sp.Line(4999)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.Text, "line number 04999") {
		t.Fatalf("last line wrong: %q", l.Text)
	}
	l0, _ := sp.Line(0)
	if !strings.HasPrefix(l0.Text, "line number 00000") {
		t.Fatalf("first line wrong: %q", l0.Text)
	}
	// hash consistency across memory and disk regions
	if sp.Hash(0) != HashLine(l0, sp.Opts()) {
		t.Fatal("hash mismatch on memory region")
	}
	if sp.Hash(4999) != HashLine(l, sp.Opts()) {
		t.Fatal("hash mismatch on spilled region")
	}
}

func TestSpoolTempDirRemoved(t *testing.T) {
	src := NewLineSource(strReader(strings.Repeat("abcdefghij\n", 10000)), StreamOptions{})
	sp, err := SpoolLines(context.Background(), src, 4096)
	if err != nil {
		t.Fatal(err)
	}
	dir := sp.dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("spool dir should exist during use: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("spool dir should be removed after close")
	}
}

func TestLargeApplyBoundedMemory(t *testing.T) {
	// Generate a large old/new pair differing near the middle.
	const n = 20000
	var oldB, newB strings.Builder
	for i := 0; i < n; i++ {
		if i == 15000 {
			oldB.WriteString("OLD_MIDDLE_LINE_HERE\n")
			newB.WriteString("NEW_MIDDLE_LINE_HERE\n")
			continue
		}
		fmt.Fprintf(&oldB, "filler line content %06d\n", i)
		fmt.Fprintf(&newB, "filler line content %06d\n", i)
	}
	p := genPatch(t, oldB.String(), newB.String(), PatchOptions{Context: 2})
	var out bytes.Buffer
	// Very small RAM budget forces spilling while applying.
	res, err := Apply(context.Background(), strReader(oldB.String()), p, &out,
		ApplyOptions{MaxMemoryBytes: 128 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != newB.String() {
		t.Fatal("large apply produced wrong content")
	}
	if len(res.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(res.Hunks))
	}
	// reverse with bounded memory returns exact original
	var back bytes.Buffer
	if _, err := ApplyReverse(context.Background(), strReader(newB.String()), p, &back,
		ApplyOptions{MaxMemoryBytes: 128 * 1024}); err != nil {
		t.Fatal(err)
	}
	if back.String() != oldB.String() {
		t.Fatal("large reverse mismatch")
	}
}

func TestLargeMergeBoundedMemory(t *testing.T) {
	const n = 12000
	mk := func(changeAt int, replacement string) *strings.Builder {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i == changeAt {
				b.WriteString(replacement)
				continue
			}
			fmt.Fprintf(&b, "row %06d payload payload\n", i)
		}
		return &b
	}
	base := mk(-1, "").String()
	ours := mk(100, "ALICE CHANGE HERE\n").String()
	theirs := mk(11000, "BOB CHANGE HERE\n").String()
	var out bytes.Buffer
	_, err := Merge3(context.Background(), strReader(base), strReader(ours), strReader(theirs), &out,
		MergeOptions{MaxMemoryBytes: 128 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ALICE CHANGE HERE") || !strings.Contains(out.String(), "BOB CHANGE HERE") {
		t.Fatal("large merge lost a change")
	}
}

func TestBoundedMemoryUpperBound(t *testing.T) {
	// Ensure in-memory line bytes never exceed the configured budget much.
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		b.WriteString("012345678901234567890123456789\n") // 30 bytes/line
	}
	src := NewLineSource(strReader(b.String()), StreamOptions{})
	budget := int64(32 * 1024)
	sp, err := SpoolLines(context.Background(), src, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if sp.spillStart < 0 {
		t.Fatal("expected spilling")
	}
	if sp.memBytes > budget {
		// Allowed to exceed by at most one line: spilling starts as soon as
		// the next line would not fit, so the last accepted line can push
		// memBytes up to budget+lineLen.
		if excess := sp.memBytes - budget; excess > 31 {
			t.Fatalf("in-memory bytes %d exceed budget %d by %d (> one line)", sp.memBytes, budget, excess)
		}
	}
	runtime.KeepAlive(sp)
}

func TestApplyStreamLimits(t *testing.T) {
	src := NewLineSource(strReader("ok\n"), StreamOptions{MaxLines: 5})
	ls, err := ReadAllLines(context.Background(), src)
	if err != nil || len(ls) != 1 {
		t.Fatalf("basic read: %v %d", err, len(ls))
	}
	big := strings.Repeat("x", 1000) + "\n"
	src2 := NewLineSource(strReader(big), StreamOptions{MaxLineBytes: 100})
	_, err = ReadAllLines(context.Background(), src2)
	if err == nil {
		t.Fatal("expected LimitError on over-long line")
	}
}

// TestSpoolCloseOnContextError verifies the spool is closed (and its temp
// directory removed) when streaming fails due to cancellation.
func TestSpoolCloseOnContextError(t *testing.T) {
	dir := t.TempDir()
	// Force spilled output into a controlled TMPDIR.
	t.Setenv("TMPDIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A large enough input to guarantee a spill before the first context
	// check hits; instead cancellation is observed immediately, so no spool
	// directory should remain regardless.
	src := NewLineSource(strReader(strings.Repeat("x\n", 5)), StreamOptions{})
	sp, err := SpoolLines(ctx, src, 4)
	if err == nil {
		_ = sp.Close()
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "diffmerge-spool-") {
			t.Fatalf("leaked spool dir: %s", e.Name())
		}
	}
}
