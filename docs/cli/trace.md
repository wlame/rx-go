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
4. Numbers the matches a `--max-results` cap left unnumbered, from the
   file's line index when one exists (see below)
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
| `--samples` | `bool` | `false` | Show context lines using the default window (3) |
| `--context` | `int` | `0` | Context lines before and after each match |
| `-B`, `--before` | `int` | `0` | Lines before each match (overrides `--context`) |
| `-A`, `--after` | `int` | `0` | Lines after each match (overrides `--context`) |
| `--json` | `bool` | `false` | Emit machine-readable JSON |
| `--color` | `string` | `auto` | Colorize output: `always`, `never`, or `auto` |
| `--no-color` | `bool` | `false` | Alias for `--color=never`; wins over `--color` |
| `--request-id` | `string` | auto (UUID v7) | Custom request ID for log correlation |
| `--hook-on-file` | `string` | `RX_HOOK_ON_FILE_URL` | Webhook URL, fired per file |
| `--hook-on-match` | `string` | `RX_HOOK_ON_MATCH_URL` | Webhook URL, fired per match (requires `--max-results`) |
| `--hook-on-complete` | `string` | `RX_HOOK_ON_COMPLETE_URL` | Webhook URL, fired once per invocation |
| `--no-cache` | `bool` | `false` | Don't consult or write the trace cache |
| `--no-index` | `bool` | `false` | Neither read nor write a line index; number lines by counting instead (same answer) |
| `-r`, `--recursive` | `bool` | `true` | Recurse into subdirectories (default; present for compatibility) |
| `--no-recursive` | `bool` | `false` | Stop at top-level directory entries |
| `-i`, `--ignore-case` | `bool` | `false` | Match case-insensitively (ripgrep `-i`) |
| `-w`, `--word-regexp` | `bool` | `false` | Match only whole words (ripgrep `-w`) |
| `-x`, `--line-regexp` | `bool` | `false` | Match only whole lines (ripgrep `-x`) |
| `-F`, `--fixed-strings` | `bool` | `false` | Treat every pattern as literal text (ripgrep `-F`) |
| `-P`, `--pcre2` | `bool` | `false` | Use the PCRE2 engine, for look-around and backreferences (ripgrep `-P`) |

`--debug` is still accepted so that old scripts keep working, but it
does nothing, is not listed in `--help`, and prints a deprecation note on
stderr.

### Colour

`auto` colours only when stdout is a terminal, and `NO_COLOR` or
`RX_NO_COLOR` in the environment turns it off. `--color=always` wins over
both, so a redirect still gets the sequences. An unrecognised value is a
usage error (exit 2).

The header labels are grey, the path bold cyan, the pattern bold magenta,
the time yellow and the match count bold green; a match line colours its
file, line, offset and pattern separately. rx-python emits the same
sequences in the same places.

Context lines are deliberately plain in both backends: they are file
content, and colouring them would compete with the match highlighting
rather than help it.

### Pattern and path resolution

- With **no `-e` flag**: the first positional argument is the pattern.
  All remaining positionals are paths.
- With **one or more `-e` flags**: every positional argument is a path.
- With **no paths** and stdin is not a pipe: defaults to `.` (current
  directory).
- With **`-` as a path**, or with no paths and a pipe on stdin: the
  piped input is spooled to a temporary file and searched, and the
  output names that file. The file is removed when the search ends.
  A `-` whose input is empty searches nothing and reports no matches,
  rather than falling back to the current directory.
- ripgrep's matching flags `-i`, `-w`, `-x`, `-F` and `-P` are `rx`
  flags, accepted anywhere on the line, and reach ripgrep on every path:
  plain files, compressed streams, seekable frames and the trace cache.
  Any other flag is a usage error (exit 2). The set is closed on purpose:
  ripgrep also has flags that run a program (`--pre`), change the output
  `rx` parses (`--count`, `--files`) or let a match span lines
  (`--multiline`), which chunked scanning cannot honour.

### Context flags precedence

`--before` and `--after` override `--context`, and any of the three is
enough on its own — `--samples` is only a shorthand for the default window
of 3 lines. An explicit zero wins over that shorthand, so
`--samples --context=0` prints the matched lines and nothing around them.

Context is printed under a `Context (N before, N after):` heading, grouped
by file. Overlapping windows are merged so no line is printed twice, a
gap between regions is drawn as `--`, and each line carries its number
with `:` for a match and `-` for context:

```text
Context (1 before, 1 after):

/var/log/app.log
2- line two beta
3: line three ERROR here
4- line four delta
```

rx-python prints exactly the same text.

### Global flags

Two flags are declared on the root command, so every subcommand accepts
them and they may be written before or after the subcommand name:

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--hidden` | `bool` | `RX_HIDDEN` or `false` | Include entries whose name starts with a dot |
| `--search-root` | `string[]` | `RX_SEARCH_ROOTS` or no sandbox | Restrict file access to this directory (repeatable) |

A path outside every configured root exits 4. A root that does not exist
is a usage error and exits 2. With neither the flag nor the variable set
there is no sandbox. See [Security](../concepts/security.md).

## Examples

### Simple literal search

```bash
rx "timeout" /var/log/app-2026-03.log
```

Prints the position of every match of the literal string `timeout`.
A large file is chunked and scanned in parallel; see
[performance](../performance.md) for measured times.

### Regex with multiple patterns

```bash
rx -e "error" -e "panic" -e "timeout.*ms" /var/log/app-2026-03.log
```

Scans the file once, reporting matches for any of the three patterns.
Each match's `pattern` field identifies which pattern ID it matched
(e.g. `p1`, `p2`, `p3`). More efficient than three separate invocations
because the file is scanned once.

### ripgrep's matching flags

```bash
rx -i "connection reset" /var/log/app.log      # any case
rx -w -e error -e warn /var/log/app.log        # whole words only
rx -F "user[42].name" /var/log/app.log         # brackets and dot are literal
rx -P "timeout(?=\s+after)" /var/log/app.log    # PCRE2 look-ahead
```

Each answer is the one `rg` gives for the same flags. `-P` needs a
ripgrep built with PCRE2 (`rg --pcre2-version` says whether yours is).
Over HTTP the same flags are query parameters of
[`GET /v1/trace`](../api/endpoints/trace.md#matching-flags), such as
`ignore_case=true`.

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

### The matches a capped search returns are arbitrary

Chunks are scanned in parallel and the cap is filled by whichever workers
finish first, so two runs of the same capped search may return different
matches, and they are not necessarily the first N in the file. `rg -mN`
returns the first N because it does not parallelise; rx trades that for
the speed, and the trade is deliberate.

If you need the first N in file order, run without a cap and take the
first N of the result, or use `rg -mN` directly.

rx-python behaves the same way, and stops its workers as soon as the cap
is met — a capped search does not leave a scan of the rest of the file
running in either backend.

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

### Line numbers, `scanned_files` and the request ID in `--json`

- `absolute_line_number` is `-1` (human output: `?`) for a match a
  capped scan could not number; `relative_line_number` is then the
  line's number within its chunk, not in the file. When the absolute
  number is known, the two are equal. See
  [line number resolution](#line-number-resolution).
- `scanned_files` lists the files found by walking a directory you
  named; it is empty when every path is a file. `files` lists every
  file searched.
- `files` and `path` keep each path as you typed it; over HTTP they are
  absolute.
- `cli_command` is `null`; only an HTTP answer carries one.

### Match with pre/post context

```bash
rx "grep.*failed" /var/log/audit-2026-03.log --samples --before=2 --after=5
```

For each match, also prints the 2 preceding and 5 following lines. In
`--json`, `context_lines` maps each match, keyed `pattern:file:offset`
(`"p1:f1:60"`), to its window in file order, the matched line included;
each entry is `{relative_line_number, absolute_line_number, line_text,
absolute_offset}`. A window holds at most `--before` lines ahead of its
match and at most `--after` lines past it, each bound on its own, and a
line in that range that matches too is part of it. Without a context
flag every window is just the matched line. Over HTTP there is no context window; see
[`GET /v1/trace`](../api/endpoints/trace.md).

### Bypass the cache

```bash
rx "WARN" /var/log/app-2026-03.log --no-cache
```

Skips the trace cache entirely — useful after an edit that keeps the
file's size and mtime on a filesystem whose ctime does not move, or when
debugging cache-related behavior. See
[concepts/caching](../concepts/caching.md) for invalidation rules.

## How it works

### Chunking

A plain file is divided into `size / RX_MIN_CHUNK_SIZE_MB` byte ranges
(20 MB by default, so 40 MB or more gives two), at most
`RX_MAX_SUBPROCESSES` (20), each ending on a newline. A goroutine pool
(`RX_WORKERS`, default the smaller of `NumCPU` and
`RX_MAX_SUBPROCESSES`) consumes these ranges in parallel. At chunk
seams, the boundary is the byte immediately after a `\n`, so no line
is split across workers. See [configuration](../configuration.md).

Each worker spawns a `ripgrep` process scoped to its byte range. Results
are accumulated in per-worker slices and merged at the end — no shared
lock on the hot path.

### Cache hit path

For a plain file of `RX_LARGE_FILE_MB` (50) or more, and for a
seekable zstd file, `rx` computes a cache key from the source path, the
pattern set and the matching flags. If an entry exists under
`~/.cache/rx/trace_cache/` and the file still has the size, mtime,
inode, ctime and fingerprint the entry recorded, that entry is loaded
and reconstructed into a full response without re-scanning. Cache miss
→ full scan. The entry records the file as it was when the scan was
planned, so a log that grows during a scan is scanned again next time.

### Line number resolution

Each chunk worker counts the newlines it reads, so a scan that runs to
the end numbers every match with its line in the whole file. A scan cut
short by `--max-results` stops some chunks part-way, and a match in a
chunk after one of them has an offset but no line number yet.

- With a line index for the file, `rx` counts forward from the nearest
  checkpoint before such a match, which is cheap.
- Without one, the match keeps `absolute_line_number: -1` (a `?` in
  human output) rather than reading the whole file up to it.
  `rx samples --offsets=…` resolves those offsets in one pass.
- With `--no-index`, `rx` neither reads nor writes an index and counts
  the lines from the start of the file up to the last such match. The
  answer is the one the index gives; only the time to reach it differs.

A seekable `.zst` is scanned frame by frame, and a capped scan can leave
the matches of a frame unnumbered when an earlier frame was not
scanned. The same three rules apply to its decompressed text: with an
index, the frame table points to the nearest frame before the match
that holds a line break, and only the frames from there are
decompressed; without one the match keeps
`-1`; with `--no-index` the text is decompressed from the first byte.

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

!!! note "Your ripgrep config file does not apply"
    `rx` runs ripgrep with `--no-config`, so a `RIPGREP_CONFIG_PATH`
    that sets `--smart-case` or `--fixed-strings` for your own `rg`
    cannot change an `rx` answer. Pass the matching flag to `rx` instead.

!!! tip "Use `--max-results` with hooks"
    When `--hook-on-match` is set, `rx` requires `--max-results` to be
    set as well. A 1-million-match scan with an uncapped per-match hook
    would flood your webhook endpoint. Without it the command exits 2
    before scanning anything.

!!! note "Hooks on the command line resolve the way they do over HTTP"
    `RX_HOOK_ON_FILE_URL`, `RX_HOOK_ON_MATCH_URL` and
    `RX_HOOK_ON_COMPLETE_URL` configure the same three hooks without a
    flag, and a flag overrides the variable for that run.
    `RX_DISABLE_CUSTOM_HOOKS=true` makes the flags inert and leaves only
    the environment values, which is how an operator pins the targets a
    shared host may call.

    The queue is drained before the process exits, so a webhook that was
    still in flight when the scan finished is still delivered. An
    `on_complete` hook does not fire when the scan itself failed.

!!! warning "Hook URLs are validated"
    `--hook-on-file`, `--hook-on-match` and `--hook-on-complete` go
    through the same SSRF guard as the `hook_on_*` query parameters: the
    scheme must be `http` or `https`, the URL must carry no credentials,
    and the target must not be a loopback, link-local, private or CGNAT
    address. A rejected URL is a usage error and no scan runs. See
    [webhook SSRF protection](../concepts/security.md#webhook-ssrf-protection).

!!! warning "Directory scans can be slow on network mounts"
    `rx trace <dir>` walks the tree, stats every file to decide if it's
    a text file, and skips binary files. On a slow NFS mount this walk
    alone can dominate wall-clock time. Consider narrowing the path
    set or running the command on the host holding the files.

!!! warning "Compressed file paths"
    `rx trace` reads `.gz`, `.bz2`, `.xz` and plain `.zst` files in
    single-worker mode — a compressed stream has no byte ranges to
    split. A seekable `.zst` written by [`rx compress`](compress.md) is
    scanned frame-parallel. Offsets are positions in the decompressed
    text either way. See [concepts/compression](../concepts/compression.md).

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
