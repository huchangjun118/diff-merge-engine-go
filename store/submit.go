package store

import (
	"path/filepath"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/merge"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
)

// SubmitResult reports the outcome of a Submit call.
type SubmitResult struct {
	// Head is the tip hash after submission.
	Head string
	// Reapplied is true when the patch was actually integrated into the tip;
	// false when the change had already been integrated (duplicate submit).
	Reapplied bool
}

// Submit applies a collaborator patch against the current committed tip.
//
// Semantics:
//
//   - The patch was authored relative to baseHash. It is rebased onto the
//     latest committed version using a three-way merge (tip, base, patched),
//     so later arrivals stack on earlier ones instead of each computing
//     from the stale baseline.
//   - Re-submitting the same change (same base+patch identity) is a no-op:
//     it returns the then-current tip without duplicating the change.
//   - Arrival order affects only the linear history, never the final tree:
//     non-conflicting sets of changes converge to identical content in any
//     order.
//   - If rebasing produces a conflict, no new version is committed and a
//     KindConflict error is returned; the tip stays exactly as it was.
//   - All state transitions (blob, commit, change index, HEAD) are atomic;
//     a crash leaves either the previous tip or the new tip, recoverable to
//     a consistent committed version on the next Open.
func (s *Store) Submit(baseHash string, p *diff.Patch, opts patch.ApplyOptions) (SubmitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.branch()
	if err != nil {
		return SubmitResult{}, err
	}
	defer b.Close()

	st, err := b.readHEAD()
	if err != nil {
		// HEAD pointer damaged: rebuild from committed versions.
		if derr.IsKind(err, derr.KindCorrupt) {
			if rerr := b.recoverHEAD(); rerr != nil {
				return SubmitResult{}, rerr
			}
			st, err = b.readHEAD()
			if err != nil {
				return SubmitResult{}, err
			}
		} else {
			return SubmitResult{}, err
		}
	}

	changeID := changeIdentity(baseHash, p)

	// First submission ever initializes the tip at the baseline.
	if st.head == "" {
		if _, err := s.Get(baseHash); err != nil {
			return SubmitResult{}, err
		}
		st = headState{head: baseHash, seq: 0}
		if err := b.writeHEAD(st); err != nil {
			return SubmitResult{}, err
		}
	}

	// Duplicate submit: the change is already in history.
	if tip, ok := b.changeIntegrated(changeID); ok {
		if s.Has(tip) {
			return SubmitResult{Head: st.head, Reapplied: false}, nil
		}
	}

	// The tip is what the collaborator started from: their change is already
	// reflected. Treat as a duplicate in effect.
	if baseHash == st.head && tipHasChange(s, b, st, p, opts) {
		return SubmitResult{Head: st.head, Reapplied: false}, nil
	}

	baseContent, err := s.Get(baseHash)
	if err != nil {
		return SubmitResult{}, err
	}
	// Compute the collaborator's intended version (base + their patch).
	patched, err := patch.Apply(baseContent, p, opts)
	if err != nil {
		return SubmitResult{}, err
	}
	tipContent, err := s.Get(st.head)
	if err != nil {
		return SubmitResult{}, err
	}

	// If the tip already contains exactly the intended content, the change is
	// effectively integrated (handles same change arriving through another id).
	if Hash(patched) == st.head {
		if err := b.markChangeIntegrated(changeID, st.head); err != nil {
			return SubmitResult{}, err
		}
		return SubmitResult{Head: st.head, Reapplied: false}, nil
	}

	// Rebase the collaborator change onto the tip via three-way merge.
	mr := merge.Merge(baseContent, patched, tipContent)
	if !mr.Clean {
		return SubmitResult{}, derr.New(derr.KindConflict,
			"patch conflicts with previously submitted changes")
	}
	merged := mr.Render(mr.TrailingEOL)
	// Convergence rule: a no-op rebase (content unchanged) still records the
	// change so duplicates are suppressed.
	if Hash(merged) == st.head {
		if err := b.markChangeIntegrated(changeID, st.head); err != nil {
			return SubmitResult{}, err
		}
		return SubmitResult{Head: st.head, Reapplied: false}, nil
	}

	newHash, err := s.Put(merged)
	if err != nil {
		return SubmitResult{}, err
	}
	// Order of durable writes: commit record first, then change index, and
	// HEAD last. A crash before the HEAD rename leaves an unreferenced commit
	// that recovery ignores (it only becomes a tip via the chain if selected
	// by seq — unreferenced higher-seq commits are still valid content, but
	// to be strict we only advance seq when HEAD moves).
	rec := commitRecord{
		seq:      st.seq + 1,
		parent:   st.head,
		base:     baseHash,
		result:   newHash,
		patch:    Hash(p.Format()),
		changeID: changeID,
	}
	if err := b.writeCommit(rec); err != nil {
		return SubmitResult{}, err
	}
	if err := b.markChangeIntegrated(changeID, newHash); err != nil {
		return SubmitResult{}, err
	}
	if err := b.writeHEAD(headState{head: newHash, seq: rec.seq}); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Head: newHash, Reapplied: true}, nil
}

// changeIdentity is the per-collaborator-change identity. The same patch
// text authored against the same base identifies the same change regardless
// of when it arrives.
func changeIdentity(baseHash string, p *diff.Patch) string {
	return Hash(baseHash + ":" + p.Format())
}

// tipHasChange cheaply handles the "patch based on the tip" duplicate case:
// applying it must be a no-op producing identical content.
func tipHasChange(s *Store, b *Branch, st headState, p *diff.Patch, opts patch.ApplyOptions) bool {
	tip, err := s.Get(st.head)
	if err != nil {
		return false
	}
	out, err := patch.Apply(tip, p, opts)
	if err != nil {
		return false
	}
	return Hash(out) == st.head
}

// Head returns the content hash of the current committed tip, or "" if no
// baseline has ever been submitted.
func (s *Store) Head() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.branch()
	if err != nil {
		return "", err
	}
	defer b.Close()
	st, err := b.readHEAD()
	if err != nil {
		return "", err
	}
	return st.head, nil
}

// Tip returns the content of the current committed tip.
func (s *Store) Tip() (string, error) {
	h, err := s.Head()
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", nil
	}
	return s.Get(h)
}

// recoverLocked runs crash recovery while holding the cross-process lock.
func (s *Store) recoverLocked() error {
	b, err := s.branch()
	if err != nil {
		return err
	}
	defer b.Close()

	// 1. Remove torn temp files.
	tmpDir := filepath.Join(s.root, dirTmp)
	entries, err := readDirIfExists(tmpDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		osRemoveAll(filepath.Join(tmpDir, e.Name()))
	}

	// 2. Validate/rebuild HEAD against the commit chain.
	st, err := b.readHEAD()
	if derr.IsKind(err, derr.KindCorrupt) {
		if rerr := b.recoverHEAD(); rerr != nil {
			return rerr
		}
	} else if err != nil {
		return err
	}
	if st.head == "" {
		_ = b.recoverHEAD()
	}
	return nil
}
