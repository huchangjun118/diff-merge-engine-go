package patch

import (
	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
)

// Validate performs structural validation of an already-parsed patch without
// consulting any target content: nil checks, hunk coordinate consistency and
// EOF marker placement. Parsed patches are already validated; this exists for
// patches constructed in code before streaming or bulk application.
func Validate(p *diff.Patch) error {
	if p == nil {
		return derr.New(derr.KindMalformedPatch, "nil patch")
	}
	for hi, h := range p.Hunks {
		if h == nil {
			return derr.New(derr.KindMalformedPatch, "nil hunk at index "+itoa(hi))
		}
		if h.OldStart < 0 || h.OldLines < 0 || h.NewStart < 0 || h.NewLines < 0 {
			return derr.New(derr.KindMalformedPatch, "negative hunk coordinates at index "+itoa(hi))
		}
		oc, nc := 0, 0
		lastOld, lastNew := -1, -1
		for i, l := range h.Lines {
			switch l.Op {
			case diff.OpContext:
				oc++
				nc++
				lastOld, lastNew = i, i
			case diff.OpAdd:
				nc++
				lastNew = i
			case diff.OpDelete:
				oc++
				lastOld = i
			default:
				return derr.New(derr.KindMalformedPatch, "invalid hunk line op at index "+itoa(hi))
			}
		}
		if oc != h.OldLines || nc != h.NewLines {
			return derr.New(derr.KindMalformedPatch, "hunk "+itoa(hi)+" line counts disagree with header")
		}
		for i, l := range h.Lines {
			if !l.NoNewline {
				continue
			}
			switch l.Op {
			case diff.OpContext:
				if i != lastOld || i != lastNew {
					return derr.New(derr.KindMalformedPatch, "no-newline marker on non-terminal context line")
				}
			case diff.OpDelete:
				if i != lastOld {
					return derr.New(derr.KindMalformedPatch, "no-newline marker on non-terminal old line")
				}
			case diff.OpAdd:
				if i != lastNew {
					return derr.New(derr.KindMalformedPatch, "no-newline marker on non-terminal new line")
				}
			}
		}
	}
	return nil
}

// DesiredTrailing reports whether the rendered new content must end with a
// line terminator according to the patch.
func DesiredTrailing(p *diff.Patch) bool {
	return desiredTrailing(p, true)
}
