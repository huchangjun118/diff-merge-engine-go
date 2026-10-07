package patch

import (
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// Reverse returns a new patch that, when applied to the patched content,
// restores the original content. Add and delete operations are swapped in
// place (line order is preserved) and hunk coordinates are exchanged.
func Reverse(p *diff.Patch) *diff.Patch {
	r := &diff.Patch{
		OldFile: p.NewFile,
		NewFile: p.OldFile,
		OldEOL:  p.NewEOL,
		NewEOL:  p.OldEOL,

		OldNoFinalNL: p.NewNoFinalNL,
		NewNoFinalNL: p.OldNoFinalNL,
	}
	for _, h := range p.Hunks {
		rh := &diff.Hunk{
			OldStart: h.NewStart,
			OldLines: h.NewLines,
			NewStart: h.OldStart,
			NewLines: h.OldLines,
		}
		for _, l := range h.Lines {
			// NoNewline belongs to the line's side; swapping add/delete keeps
			// the marker attached to the same physical content.
			switch l.Op {
			case diff.OpAdd:
				rh.Lines = append(rh.Lines, diff.Line{Op: diff.OpDelete, Text: l.Text, NoNewline: l.NoNewline})
			case diff.OpDelete:
				rh.Lines = append(rh.Lines, diff.Line{Op: diff.OpAdd, Text: l.Text, NoNewline: l.NoNewline})
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
