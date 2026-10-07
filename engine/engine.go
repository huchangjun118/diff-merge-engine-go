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
// memory and cancellation support. Nothing is written to out unless the
// entire application succeeds.
func (e *Engine) ApplyStream(ctx context.Context, base io.Reader, out io.Writer, p *diff.Patch, opts patch.ApplyOptions) error {
	return stream.ApplyStream(ctx, base, out, p, opts)
}

// ApplyStreamFile applies a patch from base and atomically installs the
// complete result at dstPath. The destination is untouched on any failure,
// cancellation or timeout.
func (e *Engine) ApplyStreamFile(ctx context.Context, base io.Reader, dstPath string, p *diff.Patch, opts patch.ApplyOptions) error {
	return stream.ApplyStreamToFile(ctx, base, dstPath, p, opts)
}

// InitBase establishes the store's initial baseline version.
func (e *Engine) InitBase(content string) (string, error) {
	return e.store.InitBase(content)
}

// Head returns the hash of the latest committed version.
func (e *Engine) Head() (string, error) {
	return e.store.Head()
}

// Submit merges a collaborator patch onto the current HEAD, commits the
// result and advances HEAD. Patches apply at most once; duplicate and
// out-of-order arrivals are serialized onto the newest committed version.
func (e *Engine) Submit(p *diff.Patch, opts patch.ApplyOptions) (string, error) {
	return e.store.Submit(p, opts)
}

// History returns the committed version hashes in chronological order.
func (e *Engine) History() ([]string, error) {
	return e.store.History()
}

// Merge performs a three-way merge.
func (e *Engine) Merge(base, ours, theirs string) *merge.MergeResult {
	return merge.Merge(base, ours, theirs)
}

// Store returns the underlying content-addressed store.
func (e *Engine) Store() *store.Store {
	return e.store
}
