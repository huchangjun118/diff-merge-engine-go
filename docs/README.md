# Diff-Merge Engine

A Go library for generating diffs, applying patches with fuzzy matching,
performing three-way merges with structured conflicts, and submitting
collaborator changes onto a linear, crash-safe version store.

## Features

- **Byte-faithful patches**: Unified diff output that omits timestamps,
  index hashes and file modes. The line-ending style (`LF`/`CRLF`) and the
  presence of a trailing newline are carried explicitly by the patch, so
  the same patch produces byte-identical output on LF and CRLF machines.
- **End-of-file changes are first-class**: A change that only adds or
  removes the final newline is represented by a real hunk (with the
  standard `\ No newline at end of file` marker), never an empty diff.
- **Fuzzy patch application**: Patches tolerate line offset and whitespace
  differences. Ambiguous matches (multiple possible locations) are reported
  as errors rather than silently applying to the wrong place.
- **Reverse application**: Any successfully applied patch can be reversed to
  restore the original content. On failure, the original content is returned
  unchanged (no partial writes).
- **All-or-nothing streaming application**: Large files are processed with
  bounded memory, spooling to a temp file when the result is large. If a
  mid-file hunk cannot be located, or the operation is cancelled/timed out,
  the destination receives zero bytes. `ApplyStreamToFile` additionally
  replaces the target via a sibling temp file + rename, so a file on disk is
  either the old version or the new version, never a half-written file.
- **Deterministic collaborative submission**: Multiple collaborators can
  submit patches authored against the same baseline. Late patches are rebased
  onto the current committed tip with a three-way merge; duplicates are
  suppressed; arrival order never changes the final content. Overlapping
  conflicting changes are rejected and leave the tip untouched.
- **Crash recovery**: Store commits, change index and `HEAD` are updated
  atomically (temp + fsync + rename). After a crash or a damaged `HEAD`,
  opening the store rebuilds the pointer from the published commit chain,
  ignoring orphaned temp files and never-published half commits.
- **Distinguishable failure kinds**: Malformed patch text, invalid encoding,
  out-of-bounds ranges, ambiguous/no context match and aborts each carry a
  distinct `derr.Kind`. None of them performs a partial write; invalid UTF-8
  and illegal patch text are rejected outright.

## Line-Ending and Trailing-Newline Rules

The output of applying a patch is controlled **entirely by the patch**, never
by the line endings of the target file:

- `# eol-old: lf|crlf` / `# eol-new: lf|crlf` directives (placed between the
  `+++` header and the first hunk) declare the line-ending style of the old
  and new content. A missing directive means `lf`, so ordinary LF patches
  stay byte-compatible with standard unified diff.
- The patch text itself is always transported with LF separators; the
  directives reference the content, not the patch file. Patch text written
  with CRLF separators is accepted and parsed identically.
- Output lines are joined with the new-side separator. Applying a CRLF
  patch to an LF target (or vice versa) yields the bytes declared by the
  patch in both cases.
- Whether the result ends with a newline follows the new side of the patch.
  It is expressed with the standard `\ No newline at end of file` marker on
  the final hunk line; the result does not depend on whether the target
  originally ended with a newline.
- A diff whose only change is adding/removing the final newline contains a
  one-region hunk (the final context line is split into `-`/`+` so each side
  can carry its own marker). Pure EOL-style changes (`LF` ↔ `CRLF`) with no
  content difference are expressed by the directives alone.

Example (newline removed at EOF):

```diff
--- file
+++ file
@@ -2,2 +2,2 @@
 line1
-line2
+line2
\ No newline at end of file
```

## Matching Rules

When applying a patch, each hunk is located in the target content as follows:

1. **Bounds check**: If the hunk's declared old range cannot fit into the
   target even at the most favorable position within `MaxOffset`, the call
   fails immediately with `KindOutOfBounds` — distinct from a context
   mismatch.
2. **Expected position**: The hunk header specifies the starting line. The
   applier searches within `MaxOffset` lines of this position.
3. **Exact match**: The context and deleted lines must match exactly.
4. **Whitespace-insensitive match**: If exact matching fails, lines are
   compared after trimming leading/trailing whitespace (when
   `IgnoreWhitespace` is set).
5. **Fuzz**: If still unmatched, up to `Fuzz` context lines at each edge of
   the hunk may be ignored. Deleted lines are never ignored.
6. **Ambiguity**: If the pattern matches multiple locations within the search
   range, a `KindAmbiguousMatch` error is returned. The patch is not applied.
7. **No match**: If no unique match is found within the tolerance, a
   `KindNoMatch` error is returned.

### Tolerance Defaults

| Option | Default | Description |
|--------|---------|-------------|
| `Fuzz` | 2 | Max context lines ignored at each edge |
| `MaxOffset` | 100 | Max line shift tolerated |
| `IgnoreWhitespace` | false | Trim lines before comparing |

## Failure Kinds

Errors are classified by `derr.Kind`; use `derr.IsKind(err, kind)`:

| Kind | When it is raised | Partial write? |
|------|-------------------|----------------|
| `KindMalformedPatch` | Patch text is not valid unified diff: missing headers, invalid hunk header/counts, illegal op character, unknown directive, unsupported multi-file patch | No |
| `KindEncodingMismatch` | Patch text or target/input bytes are not valid UTF-8 | No |
| `KindOutOfBounds` | A hunk references an old range (or insertion anchor) the target cannot contain even with `MaxOffset` | No |
| `KindAmbiguousMatch` | Context matched multiple locations within tolerance | No |
| `KindNoMatch` | Context could not be uniquely located within tolerance | No |
| `KindConflict` | A submitted patch conflicts with previously committed changes | Tip unchanged |
| `KindAborted` | Operation cancelled or timed out (incl. commit-phase I/O failure) | No |
| `KindCorrupt` | Stored content/metadata failed integrity checks or I/O failed | No |
| `KindLocked` | Another process holds the store lock | No |

Merge conflicts during three-way merge are not Go errors; they are returned as
`MergeResult.Conflicts`. The store's `Submit` *does* surface them as
`KindConflict`, because a conflicted submission cannot be committed.

## Collaborative Submission

`store.Store.Submit(baseHash, patch, opts)` implements the multi-author
workflow:

1. The patch is the author's change relative to `baseHash`.
2. The store loads the **current tip** (not the baseline the author started
   from) and computes the author's intended version by applying the patch to
   their base.
3. A three-way merge `(base, author-version, tip)` rebases the change onto
   the tip. Clean merges commit a new linear version; conflicts return
   `KindConflict` without modifying the tip.
4. Change identity is `sha256(baseHash + ":" + patchText)`. Re-submitting an
   already integrated change is a no-op that reports `Reapplied: false`.
   Identical content arriving through another path is also deduplicated.
5. Submissions are serialized by an in-process mutex plus an advisory
   `flock` for multi-process safety. Non-conflicting change sets converge to
   the same final content regardless of arrival order or concurrency; only
   the linear history order differs.

### Durability and Recovery

- Each new version writes, in order: content blob → commit record
  (`seq`, `parent`, `base`, `result`, `patch`, `change`) → change index →
  `HEAD`. Every write is temp-file + fsync + rename.
- A crash at any point leaves either the old `HEAD` or the new `HEAD`.
  Orphaned temp files and unreferenced commits are harmless.
- On `Open` (or `Recover`), temp files are purged and `HEAD` is validated; if
  it is missing or damaged, it is rebuilt from the highest-sequence commit
  whose entire parent chain resolves (a dangling-parent orphan is never
  selected). The store always reopens on a fully committed version.

## Conflict Semantics (Three-Way Merge)

- **Base**: common ancestor; **Ours** / **Theirs**: the two sides.
1. Only one side changed → the change is applied.
2. Both sides changed identically → applied (clean).
3. Both sides changed differently → structured conflict.
4. Adjacent (touching, non-overlapping) changes merge cleanly.
5. Overlapping changes merge only if the results are identical.

Conflicts are rendered with standard markers and exposed structurally in
`MergeResult.Conflicts`. Swapping sides only swaps marker sides; the conflict
set is identical. `MergeResult.TrailingNewline` / `.TrailingEOL` resolve the
ending state deterministically (a changed side wins over the base; when both
sides change it, "ours" is the deterministic tie-break); `Render(eol)`
serializes in a chosen style.

## Streaming and Atomicity Guarantees

- `ApplyStream(ctx, in, out, patch, opts)` writes nothing to `out` until the
  full result exists. Up to 8 MiB is buffered in memory; larger results
  spill to a temp file in the OS temp directory, deleted on abort.
- The context is checked before starting, while reading, before each hunk and
  before committing. A cancelled/timed-out call returns `KindAborted` and the
  output has zero bytes.
- Mid-file hunk failure (`KindNoMatch`/`KindAmbiguousMatch`/
  `KindOutOfBounds`) or invalid input encoding aborts before commit as well.
- `ApplyStreamToFile(ctx, path, patch, opts)` builds the result in a sibling
  hidden temp file and renames it over the target only on success; the
  original file remains byte-identical on any failure.

## Limitations

- Diff generation uses Myers and loads content into memory; for very large
  inputs use the streaming applier for application (diff generation itself
  is not streamed).
- Streaming application supports the same fuzz/offset/whitespace options as
  `patch.Apply`, with working memory bounded by `MaxOffset` and the largest
  hunk.
- Only `LF` and `CRLF` line endings are represented as explicit styles; lone
  `CR` terminators in inputs are normalized for logical line matching, but
  cannot be declared as a patch output style.
- Text only: inputs must be valid UTF-8. Binary content (invalid UTF-8) is
  rejected with `KindEncodingMismatch`.
- Single-file patches; a second `---`/`+++` header is a malformed-patch
  error.

## Validation

```sh
make build   # compile everything
make test    # run the full test suite
make vet     # static checks
go test -race ./...   # race detector for concurrent submissions
```

The test suite covers, without fixed paths or external services (all stores
use `t.TempDir()`):

- CRLF/LF byte-identical round trips in both environments, including reverse;
- EOF-only newline add/remove expressed by a hunk and applied deterministically;
- malformed patches, invalid UTF-8 (patch and target), out-of-bounds vs.
  context-mismatch error kinds, and no-partial-write on every failure;
- concurrent and out-of-order collaborator submissions, duplicate
  suppression and unique convergence;
- mid-file streaming aborts, cancellation and timeout producing zero bytes;
- lost/corrupt `HEAD`, orphan temp files and dangling orphan commits during
  recovery.

## Usage

```go
import (
    "context"
    "github.com/huchangjun118/diff-merge-engine-go/engine"
    "github.com/huchangjun118/diff-merge-engine-go/patch"
)

e, _ := engine.New("/path/to/store")

// Generate a patch (CRLF content yields a patch with CRLF directives).
p := e.Diff(oldContent, newContent, 3)

// Apply; output style/newline state come from the patch.
result, err := e.Apply(content, p, patch.ApplyOptions{MaxOffset: 100})

// Reverse.
restored, err := e.ApplyReverse(result, p, patch.ApplyOptions{})

// Large input: zero bytes reach dst on any failure.
err = e.ApplyStream(ctx, reader, dst, p, patch.ApplyOptions{})

// Collaborative submission onto the latest tip.
baseHash, _ := e.Store().Put(baseContent)
res, err := e.Submit(baseHash, p, patch.ApplyOptions{})
tip, _ := e.Tip()
```
