package diffmerge

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func genPatch(t *testing.T, oldS, newS string, opts PatchOptions) *Patch {
	t.Helper()
	o := NewLineSource(strReader(oldS), StreamOptions{})
	n := NewLineSource(strReader(newS), StreamOptions{})
	p, err := GeneratePatch(context.Background(), o, n, opts)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return p
}

func TestPatchRoundTrip(t *testing.T) {
	oldS := "a\nb\nc\nd\ne\nf\ng\n"
	newS := "a\nB\nc\nd\ne\nX\ng\n"
	p := genPatch(t, oldS, newS, PatchOptions{OldName: "f", NewName: "f"})
	if len(p.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(p.Hunks))
	}
	enc, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := ParsePatch(bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, enc)
	}
	if p2.OldName != "f" || p2.NewName != "f" {
		t.Fatalf("names: %q %q", p2.OldName, p2.NewName)
	}
	if p2.ID() != p.ID() {
		t.Fatalf("IDs differ after round trip: %s vs %s", p2.ID(), p.ID())
	}
	enc2, _ := p2.Encode()
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("encoding not normalised:\n%s\nvs\n%s", enc, enc2)
	}
}

func TestPatchNoNewline(t *testing.T) {
	cases := []struct{ a, b string }{
		{"a\nb\n", "a\nB\n"},
		{"a\nb", "a\nB"},   // neither side ends with newline
		{"a\nb\n", "a\nb"}, // newline removed
		{"a\nb", "a\nb\n"}, // newline added
		{"only", "onlyX"},
		{"", "new\n"},
		{"old\n", ""},
	}
	for i, c := range cases {
		p := genPatch(t, c.a, c.b, PatchOptions{OldName: "x", NewName: "x"})
		enc, err := p.Encode()
		if err != nil {
			t.Fatalf("case %d encode: %v", i, err)
		}
		p2, err := ParsePatch(bytes.NewReader(enc))
		if err != nil {
			t.Fatalf("case %d parse: %v\npatch:\n%s", i, err, enc)
		}
		// semantic equality: reverse(old) etc is verified in apply tests;
		// here compare structure by ID and byte-level re-encoding.
		if p2.ID() != p.ID() {
			t.Fatalf("case %d ID mismatch\n%s", i, enc)
		}
		enc2, _ := p2.Encode()
		if !bytes.Equal(enc, enc2) {
			t.Fatalf("case %d unstable encoding\n%s\nvs\n%s", i, enc, enc2)
		}
	}
}

func TestPatchCRLFContent(t *testing.T) {
	oldS := "line1\r\nline2\r\n"
	newS := "line1\r\nchanged\r\n"
	p := genPatch(t, oldS, newS, PatchOptions{})
	enc, _ := p.Encode()
	// CR must be carried verbatim, not swallowed by wire parsing.
	if !strings.Contains(string(enc), "-line2\r\n") || !strings.Contains(string(enc), "+changed\r\n") {
		t.Fatalf("CRLF not preserved:\n%s", enc)
	}
	p2, err := ParsePatch(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range p2.Hunks {
		for _, hl := range h.Body {
			if hl.Kind != HunkContext && hl.Line.EOL != EOLCRLF {
				t.Fatalf("expected CRLF preserved, got EOL=%q", hl.Line.EOL)
			}
		}
	}
}

func TestPatchReverse(t *testing.T) {
	oldS := "a\nb\nc\n"
	newS := "a\nB\nc\n"
	p := genPatch(t, oldS, newS, PatchOptions{})
	r := p.Reverse()
	// reversing twice yields an equal patch
	if r.Reverse().ID() != p.ID() {
		t.Fatal("reverse is not involutive")
	}
	if r.Hunks[0].OldStart != p.Hunks[0].NewStart {
		t.Fatal("reverse ranges wrong")
	}
}

func TestPatchStableIDIgnoresNames(t *testing.T) {
	p1 := genPatch(t, "a\nb\nc\n", "a\nB\nc\n", PatchOptions{OldName: "n1", NewName: "n2"})
	p2 := genPatch(t, "a\nb\nc\n", "a\nB\nc\n", PatchOptions{OldName: "other", NewName: "names"})
	if p1.ID() != p2.ID() {
		t.Fatalf("IDs must ignore file names")
	}
}

func TestPatchMultipleHunks(t *testing.T) {
	var o, n strings.Builder
	for i := 0; i < 20; i++ {
		o.WriteString("same line content repeated here\n")
		n.WriteString("same line content repeated here\n")
		if i == 2 {
			o.WriteString("old-one\n")
			n.WriteString("new-one\n")
		}
		if i == 17 {
			o.WriteString("old-two\n")
			n.WriteString("new-two\n")
		}
	}
	p := genPatch(t, o.String(), n.String(), PatchOptions{})
	if len(p.Hunks) < 2 {
		t.Fatalf("expected >=2 hunks, got %d", len(p.Hunks))
	}
	enc, _ := p.Encode()
	if _, err := ParsePatch(bytes.NewReader(enc)); err != nil {
		t.Fatalf("parse: %v", err)
	}
}

func TestPatchMalformed(t *testing.T) {
	bad := []string{
		"",
		"garbage\n",
		"--- a\n",
		"--- a\n+++ b\n",
		"--- a\n+++ b\n@@ -1 +1 @@\n-no newline after hdr",
		"--- a\n+++ b\n@@ -1,1 +1,1 @@\nx\n",             // bad prefix
		"--- a\n+++ b\n@@ -1,2 +1,2 @@\n+a\n-a\n",        // unnormalised + before -
		"--- a\n+++ b\n@@ -1,1 +1,1 @@\n-a\n+q\nextra\n", // trailing junk impossible? extra becomes next hunk header fail
		"--- a\n+++ b\n@@ -1,2 +1,1 @@\n-a\n",            // old count says 2, body has 1
		"--- a\n+++ b\n@@ -1,1 +1,2 @@\n+a\n",            // new count says 2, body has 1
	}
	for i, b := range bad {
		_, err := ParsePatch(strReader(b))
		var mp *MalformedPatchError
		if !errors.As(err, &mp) {
			t.Errorf("case %d: expected MalformedPatchError, got %v (input=%q)", i, err, b)
		}
	}
}

func TestPatchWhitespaceCompare(t *testing.T) {
	p := genPatch(t, "a  \nb\n", "a\nb\n", PatchOptions{Compare: LineCompareOptions{IgnoreTrailingWhitespace: true}})
	if len(p.Hunks) != 0 {
		t.Fatalf("expected no changes under IgnoreTrailingWhitespace, got %d hunks", len(p.Hunks))
	}
}

func TestParsePatchEncodingError(t *testing.T) {
	good := []byte("--- a\n+++ b\n@@ -1,1 +1,1 @@\n-a\n+\xff\xfe\n")
	// plain parser tolerates the bytes (engine is byte-faithful)
	if _, err := ParsePatch(bytes.NewReader(good)); err != nil {
		t.Fatalf("plain parse should accept bytes: %v", err)
	}
	_, err := ParsePatchUTF8(bytes.NewReader(good))
	var ee *EncodingError
	if !errors.As(err, &ee) {
		t.Fatalf("want EncodingError, got %v", err)
	}
	// malformed structure still reports malformed, not encoding
	if _, err := ParsePatchUTF8(strReader("nope\n")); err != nil {
		var mp *MalformedPatchError
		if !errors.As(err, &mp) {
			t.Fatalf("want MalformedPatchError precedence, got %v", err)
		}
	}
}

func TestPatchWirePortability(t *testing.T) {
	// Ensure encoded patch uses LF at the wire level and carries content CR.
	p := genPatch(t, "one\r\ntwo\r\n", "one\r\nTWO\r\n", PatchOptions{})
	enc, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// no lone CR outside the content CRLF: header lines end with single LF
	if bytes.Contains(enc, []byte("file\r\n")) {
		t.Fatalf("header must use LF wire ending:\n%s", enc)
	}
	// round trip through ParsePatch preserves CRLF exactly
	p2, err := ParsePatch(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Apply(context.Background(), strReader("one\r\ntwo\r\n"), p2, &buf, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "one\r\nTWO\r\n" {
		t.Fatalf("portability apply got %q", buf.String())
	}
}
