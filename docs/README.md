# Diff-Merge Engine

A Go library for generating diffs, applying patches with fuzzy matching, and
performing three-way merges with structured conflicts.

## Features

- **Portable patches**: Unified diff output that omits timestamps, index hashes
  and file modes. The same patch text applies identically in different
  environments. Line endings are normalized (CRLF/CR → LF).
- **Fuzzy patch application**: Patches tolerate line offset and whitespace
  differences. Ambiguous matches (multiple possible locations) are reported as
  errors rather than silently applying to the wrong place.
- **Reverse application**: Any successfully applied patch can be reversed to
  restore the original content. On failure, the original content is returned
  unchanged (no partial writes).
- **Three-way merge**: Merges changes from two branches relative to a common
  base. Non-overlapping changes are merged cleanly; overlapping changes that
  differ produce structured conflict markers. Conflict detection is
  deterministic and order-independent.
- **Content-addressed store**: Blobs are stored by hash. Atomic writes
  (temp + fsync + rename) ensure no partial files. Idempotent patch
  application prevents duplicate changes. Crash recovery cleans up orphaned
  temp files.
- **Streaming application**: Large files are processed with bounded memory.
  Cancellation or timeout aborts the operation without leaving temp files.

## Matching Rules

When applying a patch, each hunk is located in the target content as follows:

1. **Expected position**: The hunk header specifies the starting line. The
   applier searches within `MaxOffset` lines of this position.
2. **Exact match**: The context and deleted lines must match exactly.
3. **Whitespace-insensitive match**: If exact matching fails, lines are
   compared after trimming leading/trailing whitespace (when
   `IgnoreWhitespace` is set).
4. **Fuzz**: If still unmatched, up to `Fuzz` context lines at each edge of
   the hunk may be ignored. Deleted lines are never ignored.
5. **Ambiguity**: If the pattern matches multiple locations within the search
   range, a `KindAmbiguousMatch` error is returned. The patch is not applied.
6. **No match**: If no unique match is found within the tolerance, a
   `KindNoMatch` error is returned.

### Tolerance Defaults

| Option | Default | Description |
|--------|---------|-------------|
| `Fuzz` | 2 | Max context lines ignored at each edge |
| `MaxOffset` | 100 | Max line shift tolerated |
| `IgnoreWhitespace` | false | Trim lines before comparing |

### Offset Tolerance

The `MaxOffset` parameter controls how far the hunk can be from its expected
position. This accommodates insertions or deletions earlier in the file that
shift line numbers. If the offset exceeds `MaxOffset`, the hunk is not found
and an error is returned.

### Ambiguity

If a hunk's context matches more than one location (e.g., repeated code
blocks), the applier returns `KindAmbiguousMatch` rather than guessing. This
prevents changes from being applied to the wrong location.

## Conflict Semantics

Three-way merge follows standard diff3 semantics:

- **Base**: The common ancestor.
- **Ours**: One side of the merge.
- **Theirs**: The other side.

### Merge Rules

1. **Only one side changed**: The change is applied.
2. **Both sides changed identically**: The change is applied (clean).
3. **Both sides changed differently**: A conflict is produced.
4. **Adjacent changes** (touching but not overlapping): Merged cleanly.
5. **Overlapping changes**: Merged only if the results are identical;
   otherwise a conflict is produced.

### Conflict Markers

Conflicts are rendered with standard markers:

```
<<<<<<< ours
<our version>
=======
<their version>
>>>>>>> theirs
```

The `Conflict` struct provides structured access to the base, ours and theirs
content for programmatic handling.

### Order Independence

Swapping ours and theirs only swaps the conflict sides. The conflict set
(regions and base ranges) is identical. Clean merges produce identical results
regardless of order.

## Error Kinds

Errors are classified by `derr.Kind`:

| Kind | Description |
|------|-------------|
| `KindMalformedPatch` | The patch text is not valid unified diff |
| `KindEncodingMismatch` | Input encoding does not match |
| `KindOutOfBounds` | A hunk references lines outside the content |
| `KindAmbiguousMatch` | Context matched multiple locations |
| `KindNoMatch` | Context could not be located within tolerance |
| `KindConflict` | A three-way merge conflict |
| `KindAborted` | Operation cancelled or timed out |
| `KindCorrupt` | Stored content failed integrity checks |
| `KindLocked` | A concurrent operation holds the lock |

Merge conflicts are not errors; they are returned as `MergeResult.Conflicts`.

## Limitations

- **Diff generation** uses the Myers algorithm and requires random access to
  the content. For very large files, use the streaming applier for patch
  application, but note that diff generation itself loads content into
  memory.
- **Streaming application** supports context and deleted lines but does not
  support fuzz (edge context lines are not ignored in streaming mode).
- **Line endings**: All line endings are normalized to LF internally. The
  output uses LF. If the input had no trailing newline, the output preserves
  that property.
- **Binary files**: The library operates on text lines. Binary content with
  null bytes or unusual encodings may not diff correctly.
- **Multi-file patches**: The parser supports single-file patches. Multi-file
  patches (with multiple `---`/`+++` headers) are not supported.

## Usage

```go
import (
    "github.com/huchangjun118/diff-merge-engine-go/engine"
    "github.com/huchangjun118/diff-merge-engine-go/patch"
)

e, _ := engine.New("/path/to/store")

// Generate a patch.
p := e.Diff(oldContent, newContent, 3)

// Apply with fuzzy matching.
result, err := e.Apply(content, p, patch.ApplyOptions{
    MaxOffset:        100,
    IgnoreWhitespace: true,
})

// Reverse.
restored, err := e.ApplyReverse(result, p, patch.ApplyOptions{})

// Three-way merge.
mr := e.Merge(base, ours, theirs)
if !mr.Clean {
    for _, c := range mr.Conflicts {
        // handle conflict
    }
}
```
