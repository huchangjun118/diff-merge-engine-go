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

// Store returns the underlying content-addressed store.
func (e *Engine) Store() *store.Store {
	return e.store
}
