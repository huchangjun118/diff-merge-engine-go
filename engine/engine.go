// Package engine is the top-level facade for the diff/merge engine.
package engine

import (
	"context"
	"io"

	"github.com/huchangjun118/diff-merge-engine-go/diff"
	"github.com/huchangjun118/diff-merge-engine-go/merge"
	"github.com/huchangjun118/diff-merge-engine-go/patch"
	"github.com/huchangjun118/diff-merge-engine-go/store"
	"github.com/huchangjun118/diff-merge-engine-go/stream"
)

// Engine provides the full diff/merge capability.
type Engine struct {
	store *store.Store
}

// New creates an engine backed by a store at root.
func New(root string) (*Engine, error) {
	s, err := store.Open(root)
	if err != nil {
		return nil, err
	}
	return &Engine{store: s}, nil
}

// Diff generates a portable patch between oldContent and newContent.
func (e *Engine) Diff(oldContent, newContent string, contextLines int) *diff.Patch {
	return diff.Generate(oldContent, newContent, contextLines)
}

// Apply applies a patch to content.
func (e *Engine) Apply(content string, p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	return patch.Apply(content, p, opts)
}

// ApplyReverse reverses a previously applied patch.
func (e *Engine) ApplyReverse(content string, p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	return patch.ApplyReverse(content, p, opts)
}

// ApplyStream applies a patch to a reader, writing to a writer, with bounded
// memory and cancellation support.
func (e *Engine) ApplyStream(ctx context.Context, base io.Reader, out io.Writer, p *diff.Patch, opts patch.ApplyOptions) error {
	return stream.ApplyStream(ctx, base, out, p, opts)
}

// Merge performs a three-way merge.
func (e *Engine) Merge(base, ours, theirs string) *merge.MergeResult {
	return merge.Merge(base, ours, theirs)
}

// Submit stacks a collaborator patch (authored against baseHash) onto the
// latest committed tip and advances the branch. See store.Store.Submit for
// the ordering, duplication and crash-recovery guarantees.
func (e *Engine) Submit(baseHash string, p *diff.Patch, opts patch.ApplyOptions) (store.SubmitResult, error) {
	return e.store.Submit(baseHash, p, opts)
}

// Head returns the current committed tip hash.
func (e *Engine) Head() (string, error) { return e.store.Head() }

// Tip returns the current committed tip content.
func (e *Engine) Tip() (string, error) { return e.store.Tip() }

// Store returns the underlying content-addressed store.
func (e *Engine) Store() *store.Store {
	return e.store
}
