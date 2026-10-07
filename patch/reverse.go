package patch

import (
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// Reverse returns a new patch that, when applied to the patched content,
// restores the original content. Add and delete operations are swapped in
// place (line order is preserved), hunk coordinates are exchanged and the
// byte-level text conventions (EOL style and trailing-newline state) are
// swapped as well.
func Reverse(p *diff.Patch) *diff.Patch {
	r := &diff.Patch{
		OldFile:            p.NewFile,
		NewFile:            p.OldFile,
		OldEOL:             p.NewEOL,
		NewEOL:             p.OldEOL,
		OldTrailingNewline: p.NewTrailingNewline,
		NewTrailingNewline: p.OldTrailingNewline,
	}
	for _, h := range p.Hunks {
		rh := &diff.Hunk{
			OldStart: h.NewStart,
			OldLines: h.NewLines,
			NewStart: h.OldStart,
			NewLines: h.OldLines,
		}
		for _, l := range h.Lines {
			switch l.Op {
			case diff.OpAdd:
				rh.Lines = append(rh.Lines, diff.Line{Op: diff.OpDelete, Text: l.Text})
			case diff.OpDelete:
				rh.Lines = append(rh.Lines, diff.Line{Op: diff.OpAdd, Text: l.Text})
			default:
				rh.Lines = append(rh.Lines, l)
			}
		}
		r.Hunks = append(r.Hunks, rh)
	}
	return r
}

// ApplyReverse applies the reverse of p to content, restoring the original.
// On failure content is returned unchanged.
func ApplyReverse(content string, p *diff.Patch, opts ApplyOptions) (string, error) {
	return Apply(content, Reverse(p), opts)
}
