# `rx trace`

Search one or more files or directories for regex patterns using
parallel chunking.

## Synopsis

```text
rx trace [PATTERN] [PATH ...] [flags]
rx trace -e <PATTERN> [-e <PATTERN> ...] [PATH ...] [flags]
rx [PATTERN] [PATH ...] [flags]     # trace is the default subcommand
```

## Description

`rx trace` is the primary search command. It accepts one or more regex
patterns and one or more paths, then:

1. Splits each large file into byte-aligned chunks (see
   [concepts/chunking](../concepts/chunking.md))
2. Launches a goroutine pool to scan chunks in parallel via `ripgrep`
3. Consults the on-disk trace cache — a valid cache entry bypasses
   re-scanning entirely
4. Resolves absolute line numbers using the file's line-offset index
   when one is available
5. Optionally fires webhooks per file / per match / per run completion
6. Emits matches sorted by file, then byte offset

The underlying regex engine is `ripgrep`, so the supported regex syntax
is Rust's `regex` crate with `ripgrep`'s flag extensions.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-e`, `--regexp`, `--regex` | `string[]` | — | Regex pattern (repeatable) |
| `--path`, `--file` | `string[]` | — | Path to search (repeatable; alias of positional) |
| `--max-results` | `int` | `0` | Maximum matches to return; `0` = unlimited |
| `--samples` | `bool` | `false` | Include context lines around each match |
| `--context` | `int` | `0` | Context lines before and after (for `--samples`) |
| `-B`, `--before` | `int` | `0` | Lines before each match (overrides `--context`) |
| `-A`, `--after` | `int` | `0` | Lines after each match (overrides `--context`) |
| `--json` | `bool` | `false` | Emit machine-readable JSON |
| `--no-color` | `bool` | `false` | Disable ANSI colors |
| `--debug` | `bool` | `false` | Write `.debug_*` artifacts for post-mortem |
| `--request-id` | `string` | auto (UUID v7) | Custom request ID for log correlation |
| `--hook-on-file` | `string` | — | Webhook URL, fired per file |
| `--hook-on-match` | `string` | — | Webhook URL, fired per match (requires `--max-results`) |
| `--hook-on-complete` | `string` | — | Webhook URL, fired once per invocation |
| `--no-cache` | `bool` | `false` | Don't consult or write the trace cache |
| `--no-index` | `bool` | `false` | Don't consult the unified line index |
| `-r`, `--recursive` | `bool` | `true` | Recurse into subdirectories (default; present for compatibility) |
| `--no-recursive` | `bool` | `false` | Stop at top-level directory entries |

### Pattern and path resolution

- With **no `-e` flag**: the first positional argument is the pattern.
  All remaining positionals are paths.
- With **one or more `-e` flags**: every positional argument is a path.
- With **no paths** and stdin is not a pipe: defaults to `.` (current
  directory).
- Unknown flags like `-i`, `-w`, `--case-sensitive` parse without
  error (cobra treats them as unknown) so scripts that pass ripgrep
  flags don't fail. Flags `rx` itself recognizes are documented in
  the table above.

### Context flags precedence

`--before`/`--after` override `--context`. All three are ignored unless
`--samples` is also set.

## Examples

### Simple literal search

```bash
rx "timeout" /var/log/app-2026-03.log
```

Prints matches for the literal string `timeout` across the file. Since
this is a dense-literal scan, `rx` chunks the file and scans in parallel;
on a 1.3 GB file with 4 workers this runs in ~40 seconds.

### Regex with multiple patterns

```bash
rx -e "error" -e "panic" -e "timeout.*ms" /var/log/app-2026-03.log
```

Scans the file once, reporting matches for any of the three patterns.
Each match's `pattern` field identifies which pattern ID it matched
(e.g. `p1`, `p2`, `p3`). More efficient than three separate invocations
because the file is scanned once.

### Directory scan with depth control

```bash
rx "connection refused" /var/log/ --no-recursive
```

Searches only files directly in `/var/log/`, skipping subdirectories.
Default behavior recurses the full tree; use `--no-recursive` for
top-level-only scans.

### Structured output for piping

```bash
rx "5xx" /var/log/nginx/access.log --json --max-results=100 \
    | jq '.matches[] | {file, line: .absolute_line_number, offset}'
```

Produces structured JSON capped at 100 matches. Use `--max-results`
whenever you plan to stream the output downstream — it bounds memory use
and prevents a runaway scan from filling the pipe.

### Early-termination on `--max-results`

When `--max-results` is set to a non-zero value, `rx` stops scanning as
soon as the cap is reached:

- **Regular files**: the collector watches per-chunk match counts as
  workers report in. The moment the running total meets the cap, the
  errgroup context is canceled. In-flight ripgrep subprocesses receive
  SIGKILL via `exec.CommandContext`; queued chunks are skipped before
  they spawn rg.
- **Compressed files** (`.gz`, `.bz2`, `.xz`, `.zst`): the same
  semantics apply to the single-stream decompressor — once the cap is
  hit, the io.Copy feeder to rg is canceled and rg exits.
- **Seekable zstd** (`.zst` with a seek index): frame batches that
  haven't started are skipped; in-flight batches are canceled.

This means `rx trace --max-results=10` on a 100 GB file typically
completes in milliseconds once the first chunk produces matches,
rather than scanning the whole file and truncating at the end. The
total matches returned MAY exceed `--max-results` slightly when
concurrent workers each produce matches past the cap before the
cancel propagates; the response is truncated to the cap before
return so callers always see at most `--max-results` matches.

### Match with pre/post context

```bash
rx "grep.*failed" /var/log/audit-2026-03.log --samples --before=2 --after=5
```

For each match, also returns the 2 preceding and 5 following lines. The
matched line sits in the middle of the `context_lines` array. Context
retrieval uses the file's line index if present; otherwise falls back
to a linear scan around each match.

### Bypass the cache

```bash
rx "WARN" /var/log/app-2026-03.log --no-cache
```

Skips the trace cache entirely — useful after manual file edits that
don't change mtime, or when debugging cache-related behavior. See
[concepts/caching](../concepts/caching.md) for invalidation rules.

## How it works

### Chunking

For each input file above ~20 MB (configurable via
[`RX_MIN_CHUNK_SIZE_MB`](../configuration.md)), `rx` divides the file
into byte ranges, each ending on a newline. A goroutine pool
(`RX_WORKERS`, default `NumCPU`) consumes these ranges in parallel. At
chunk seams, the boundary is the byte immediately after a `\n`, so no
line is split across workers.

Each worker spawns a `ripgrep` process scoped to its byte range. Results
are accumulated in per-worker slices and merged at the end — no shared
lock on the hot path.

### Cache hit path

When a trace request is made, `rx` computes a cache key from the source
path, mtime, size, pattern set, and flags. If the key matches an entry
under `~/.cache/rx/trace_cache/`, that entry is loaded and reconstructed
into a full response without re-scanning. Cache miss → full scan.

### Line number resolution

`ripgrep` reports line numbers relative to the start of each chunk. `rx`
converts these to absolute line numbers using a per-file line index — a
sparse map of line-number-to-byte-offset checkpoints. If no index
exists, absolute line numbers are still computed by counting `\n`s
before each match, which is more expensive on the first access.

### Performance characteristics

- **Scales near-linearly** up to physical core count on literal-dense
  patterns. Beyond physical cores (hyperthreads), gains plateau because
  regex scanning is memory-bandwidth-bound.
- **Regex-heavy patterns** (look-around, nested quantifiers) don't
  benefit as much from extra workers; the compiled automaton becomes
  the bottleneck.
- **Multi-pattern scans** cost roughly 1× plus a small per-pattern
  constant, not N× — `ripgrep` compiles the patterns once into a
  combined matcher.
- **Warm cache hits** return in sub-second time regardless of file size.

## Tips and gotchas

!!! tip "Use `--max-results` with hooks"
    When `--hook-on-match` is set, `rx` requires `--max-results` to be
    set as well. A 1-million-match scan with an uncapped per-match hook
    would flood your webhook endpoint.

!!! warning "Directory scans can be slow on network mounts"
    `rx trace <dir>` walks the tree, stats every file to decide if it's
    a text file, and skips binary files. On a slow NFS mount this walk
    alone can dominate wall-clock time. Consider narrowing the path
    set or running the command on the host holding the files.

!!! warning "Compressed file paths"
    `rx trace` can read `.gz`, `.bz2`, `.xz`, and `.zst` files, but
    only in single-worker mode — compressed streams don't support
    byte-range scans. For random access on compressed data, see
    [`rx compress`](compress.md) and [concepts/compression](../concepts/compression.md).

!!! note "Regex engine is ripgrep"
    Pattern syntax is Rust's `regex` crate as supported by `ripgrep`.
    Flags like `(?i)` (inline case-insensitive) and PCRE syntax with
    `--pcre2`-equivalent alternatives work inside the pattern itself.
    The `rx` CLI itself understands only the flags documented above.

## See also

- [concepts/chunking](../concepts/chunking.md) — how parallel chunking works
- [concepts/caching](../concepts/caching.md) — trace cache layout and invalidation
- [`rx index`](line-index.md) — build an index for faster line-number resolution
- [`rx samples`](samples.md) — retrieve content around matches
- [api/endpoints/trace](../api/endpoints/trace.md) — same feature over HTTP
- [api/webhooks](../api/webhooks.md) — webhook payload shapes
