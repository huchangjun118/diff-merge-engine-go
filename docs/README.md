# Diff-Merge Engine

A Go library for generating diffs, applying patches with fuzzy matching,
performing three-way merges, and maintaining a linear, crash-safe commit
history. All patch application is byte-level deterministic: the output is
fully determined by the patch text, never by the local platform's line
endings or the target file's own conventions.

## Features

- **Byte-faithful portable patches**: unified diffs that omit timestamps,
  index hashes and file modes, plus an explicit machine-readable preamble
  carrying the exact line-ending style (`lf`/`crlf`) and the
  end-of-file-newline state of both sides. The same patch produces
  byte-identical output whether it is applied in an LF or a CRLF environment.
- **End-of-file-newline changes are real diffs**: adding or removing the
  final newline is expressed with a dedicated trailing hunk and the standard
  `\ No newline at end of file` marker, so the change can never be silently
  lost.
- **Fuzzy patch application**: patches tolerate line offset and whitespace
  differences. Ambiguous matches are reported as errors rather than applied
  to the wrong place.
- **All-or-nothing application**: every failure (malformed patch, encoding
  mismatch, out-of-bounds range, context mismatch, cancellation, timeout,
  write error) rejects the operation before any output is committed. The
  streaming applier spools the full result privately and never touches the
  destination until success; the file API installs results atomically.
- **Distinguishable failure kinds**: malformed patch text, invalid input
  encoding, structurally out-of-range hunks, ambiguous/no context match,
  cancellation and storage corruption each have a distinct `derr.Kind`.
  Invalid UTF-8 or illegal patch text is rejected outright, never processed
  as a normal patch.
- **Reverse application**: successful patches reverse while preserving the
  exact byte conventions of the restored version.
- **Three-way merge**: deterministic diff3-style merge with structured
  conflicts and order-independent output; EOL and trailing-newline
  conventions are merged deterministically.
- **Linear collaborative store**: contributors submit patches that are
  merged onto the current HEAD, not onto stale bases. Submission is
  serialized, idempotent by canonical patch text, and produces a unique
  result regardless of arrival order, duplicates or concurrency. Commits
  crash-recover to the last fully published version.
- **Bounded-memory streaming**: large files are processed with a small
  working set; the result spills to a private temporary spool past 1 MiB.

## Patch Text Conventions

A patch is canonical LF text regardless of the files it describes; byte
conventions are carried by an optional preamble line placed before the file
headers:

```
# diff-merge-engine eol: old=crlf new=crlf trailing: old=yes new=no
--- a/file
+++ b/file
@@ -1,2 +1,2 @@
 first
-last
+last
\ No newline at end of file
```

Rules:

1. `eol: old=<lf|crlf> new=<lf|crlf>` declares the terminator style of each
   file version. The output of applying the patch always uses `new`, even if
   the target file uses the other style. When the line is absent both sides
   default to `lf`.
2. `trailing: old=<yes|no> new=<yes|no>` declares whether each version ends
   with a terminator. The output's final newline state equals `new`,
   independently of the target. When the line is absent, the traditional
   behavior applies and any in-hunk `\ No newline at end of file` marker
   governs the touched tail.
3. The patch body itself is always LF-encoded; a patch delivered over a
   CRLF channel is normalized while parsing. The preamble is the only
   authority for the output byte layout.
4. Pure line-ending conversion (identical logical content, different EOL)
   produces a metadata-only patch with zero hunks.
5. A change limited to the final newline produces a trailing hunk over the
   identical tail ending in a delete/add pair of the last line carrying the
   standard no-newline marker.
6. Standard hand-written or git-emitted patches (with timestamps, section
   annotations and no-newline markers) parse correctly; unknown pre-existing
   `#` comment lines are tolerated, but an unknown `eol`/`trailing` value is
   a malformed-patch error.

## Matching Rules

When applying a patch, each hunk is located in the target content as follows:

1. **Structural precheck**: every hunk's declared old range must fit the
   target length. A range that cannot exist fails with
   `KindOutOfBounds` before anything is applied. Pure insertions
   (old count 0) are valid exactly at or before EOF.
2. **Expected position**: the hunk header specifies the starting line; the
   applier searches within `MaxOffset` lines of it (adjusted for offsets
   introduced by earlier hunks).
3. **Exact match**: context and deleted lines must match exactly.
4. **Whitespace-insensitive match**: on exact failure, lines are compared
   after trimming surrounding whitespace (when `IgnoreWhitespace` is set).
5. **Fuzz**: up to `Fuzz` context lines at each hunk edge may be dropped.
   Deleted lines are never ignored.
6. **Ambiguity**: multiple matches yield `KindAmbiguousMatch`; no unique
   match yields `KindNoMatch`. Neither error applies anything.

### Tolerance Defaults

| Option | Default | Description |
|--------|---------|-------------|
| `Fuzz` | 2 | Max context lines ignored at each edge |
| `MaxOffset` | 100 | Max line shift tolerated |
| `IgnoreWhitespace` | false | Trim lines before comparing |

When integrating collaborator patches generated against a common base, use
sufficient context so that each hunk remains uniquely identifiable after
earlier patches land; hunks whose context itself was edited by a committed
patch fail with `KindNoMatch` (a genuine adjacent conflict) and HEAD does not
move.

## Streaming and Atomic Files

`stream.ApplyStream(ctx, base, out, patch, opts)`:

- Reads the base incrementally (16 MiB maximum line length) and builds the
  result in a private spool (memory up to 1 MiB, then a temporary file).
- On **any** failure, including mid-stream location failure, invalid
  encoding, cancellation or timeout, it returns before copying the spool:
  the caller's writer receives **zero bytes**.
- The spool is copied to `out` only after a final context check. If the
  context expires or the writer fails during that final copy, the returned
  error is non-nil and the caller must treat whatever it received as invalid
  (a writer cannot be un-written).
- `stream.ApplyStreamToFile(ctx, base, dstPath, patch, opts)` provides the
  stronger guarantee for paths: the result is fsynced in a sibling temp
  file and renamed into place, so an existing destination is either fully
  replaced or left byte-for-byte untouched.

## Collaborative Store

`store.Store` keeps a single append-only chain of versions:

1. `InitBase(content)` establishes the root version and HEAD. It is
   idempotent while the content is unchanged; re-initializing with different
   content fails.
2. `Submit(patch, opts)` is serialized by an in-process mutex. It always:
   - applies the patch to the **current HEAD**, not to the submitter's base;
   - is idempotent by the SHA-256 of the canonical patch text — repeat,
     late and out-of-order arrivals return the originally committed hash and
     create no second change;
   - appends exactly one commit (`parent`, `result`, `patch`, `seq`) and
     advances HEAD last.
3. `ApplyPatch(baseHash, patch, opts)` remains available for caller-managed,
   non-linear graphs and does not move HEAD.
4. Crash model: blobs and commit objects are written with
   temp-file + fsync + atomic rename. The commit object is written first;
   HEAD is published last. A crash before publish leaves an inert orphan
   commit and the previous HEAD intact. On open, `Recover` removes temp
   files and validates the HEAD chain (hashes, sequence numbers, blob
   presence); a broken chain reports `KindCorrupt` rather than exposing a
   half-written state.

Arrival order determines commit order, but every distinct patch lands once
and the final content is the unique merge of all accepted changes.

## Conflict Semantics

Three-way merge follows diff3 semantics:

1. Only one side changed -> the change is applied.
2. Both sides changed identically -> applied cleanly.
3. Both sides changed differently -> standard conflict markers
   (`<<<<<<< ours`, `=======`, `>>>>>>> theirs`) and a structured
   `Conflict`.
4. Adjacent (touching but non-overlapping) changes merge cleanly.

Output conventions are resolved without depending on argument order:
identical side conventions win; otherwise the base convention is kept unless
exactly one side changed it.

## Error Kinds

| Kind | Meaning | Output |
|------|---------|--------|
| `KindMalformedPatch` | Patch text/structure is invalid (headers, ranges, counts, markers, directives) | Nothing applied |
| `KindEncodingMismatch` | Patch or target is not valid UTF-8 | Nothing applied |
| `KindOutOfBounds` | A hunk declares a line range the target cannot contain | Nothing applied |
| `KindAmbiguousMatch` | Context matched multiple locations | Nothing applied |
| `KindNoMatch` | Context could not be located within tolerance | Original content returned |
| `KindConflict` | Three-way merge conflict / store re-init with different base | See result |
| `KindAborted` | Cancellation, timeout, or output write failure | No bytes (stream); destination untouched (file API) |
| `KindCorrupt` | Blob/commit/HEAD integrity failure or I/O damage | Store refuses operation |
| `KindLocked` | Reserved for external lock contention | Nothing applied |

Merge conflicts are not returned as Go errors; they are reported in
`MergeResult.Conflicts`.

## Limitations

- Diff generation uses the in-memory Myers algorithm (whole content in RAM);
  application supports streaming.
- Only single-file patches are supported; a second `---`/`+++` pair is a
  malformed-patch error.
- Text is assumed UTF-8. Invalid UTF-8 is rejected with
  `KindEncodingMismatch`; other encodings must be converted by the caller.
- One dominant EOL style per file version is recorded (`CRLF` wins if a
  CRLF sequence occurs anywhere); arbitrary mixed endings are not preserved
  per-line.
- The store's serialization lock is in-process; multi-process writers on the
  same directory are not coordinated.
- Streaming output beyond the final copy to an arbitrary `io.Writer` cannot
  be physically retracted; use `ApplyStreamToFile` for a path-level atomic
  guarantee.

## Usage

```go
import (
    "context"
    "github.com/huchangjun118/diff-merge-engine-go/engine"
    "github.com/huchangjun118/diff-merge-engine-go/patch"
)

e, _ := engine.New("/path/to/store")

// Generate a byte-faithful patch (EOL and final newline captured).
p := e.Diff(oldContent, newContent, 3)

// Apply: output layout follows the patch, not the target.
result, err := e.Apply(content, p, patch.ApplyOptions{MaxOffset: 100})

// Reverse with byte conventions restored.
restored, _ := e.ApplyReverse(result, p, patch.ApplyOptions{})

// Three-way merge.
mr := e.Merge(base, ours, theirs)

// Collaborative submission onto the newest committed version.
e.InitBase(base)
hash, err := e.Submit(p, patch.ApplyOptions{})
```

## Verification

Run the full suite (including the race detector):

```sh
make build
go test ./...
go test -race ./...
go vet ./...
```

Key automated behaviors:

- CRLF patch applied to both LF and CRLF targets -> byte-identical CRLF
  output (`patch/eol_test.go`, `diff/eol_test.go`).
- Trailing-newline-only add/remove roundtrip, independent of the target's
  own final newline.
- Concurrent, duplicated and reordered collaborator submissions -> each
  patch lands once and the final content is unique (`store/collab_test.go`).
- Mid-stream failure, cancellation and timeout -> zero bytes in the writer;
  the file API leaves the destination untouched (`stream/safety_test.go`).
- Malformed patches, invalid UTF-8 and out-of-range hunks -> distinct error
  kinds and no partial writes (`patch/eol_test.go`).
