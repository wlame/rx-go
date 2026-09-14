# `rx samples`

Retrieve lines of content from a file, addressed by byte offset or line
number, with optional surrounding context.

## Synopsis

```text
rx samples PATH -b OFFSETS [flags]
rx samples PATH -l LINES   [flags]
```

Exactly one of `-b` / `--offsets` or `-l` / `--lines` is required.

## Description

`rx samples` is a content-retrieval tool — given a file and a set of
addresses, it prints the targeted lines along with configurable context
above and below each one. The command has two address modes:

- **Byte offset** (`--offsets`): each value is a byte position; the line
  containing that byte is the target. Works on any uncompressed file;
  refused for a compressed one.
- **Line number** (`--lines`): each value is a 1-based line number. When
  a cached line index exists, the read starts at the nearest checkpoint;
  otherwise it starts at byte 0.

Both modes accept single values, ranges, and comma-separated lists. A
single call can retrieve content from many locations across a file in
one pass.

### Bounded reads

On a plain file, `samples` reads only the portion of the file it needs,
with one exception: the first lookup builds an index (below). Measured
on a 465 MB log already in the page cache:

- With an index: seeks to the nearest checkpoint before the first
  wanted line (checkpoints are 1 MB apart by default) and reads to the
  last one. `--lines=700000 --context=3`: 13 ms.
- Without an index: reads from byte 0 until it reaches the last wanted
  line, then stops. `--lines=1-1000`: 12 ms; `--lines=700000`: 90 ms.
- The first lookup in a file of `RX_LARGE_FILE_MB` (50 MB) or more
  with no cached index reads the whole file to build one: 137 ms.

A gzip, bzip2, xz or plain zstd file is streamed to its end for every
lookup, whatever the line (1.6 s on the 113 MB `.gz` of that log). A
seekable zstd file with an index decompresses only the frames that hold
the wanted lines.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-b`, `--offsets` | `string` | — | Comma-separated byte offsets / ranges |
| `-l`, `--lines` | `string` | — | Comma-separated 1-based line numbers / ranges |
| `-c`, `--context` | `int` | `3` | Lines before AND after each target |
| `-B`, `--before` | `int` | `--context` | Lines before; when given, 0 included, it overrides `--context` |
| `-A`, `--after` | `int` | `--context` | Lines after; when given, 0 included, it overrides `--context` |
| `--json` | `bool` | `false` | Emit machine-readable JSON |
| `--color` | `string` | `auto` | Colorize output: `always`, `never`, or `auto` |
| `--no-color` | `bool` | `false` | Alias for `--color=never`; wins over `--color` |
| `-r`, `--regex` | `string` | — | Highlight matches of this regex in context lines (requires color) |
| `--no-index` | `bool` | `RX_NO_INDEX` or `false` | Do not build or use a line index |

`-b` also answers to `--byte-offset` and `-l` to `--line-offset`, the
spellings rx-python has always used, so a script written against either
backend runs against both.

`auto` colours only when stdout is a terminal, and `NO_COLOR` or
`RX_NO_COLOR` in the environment turns it off. `--color=always` wins over
both, so a redirect still gets the sequences. An unrecognised value is a
usage error (exit 2) rather than a silent fall back to `auto`.

### The index it may build

A lookup in a plain file of `RX_LARGE_FILE_MB` (50 MB) or more, or in
any compressed file, builds a line index if none is cached, so the
next lookup in the same file is fast. That first lookup reads the whole
file. The build runs without analysis. Only a seekable zstd file uses
the index among compressed formats; a gzip, bzip2, xz or plain zstd
lookup streams either way. `--no-index` turns the build off and the
lookup reads the file without an index — slower, same answer. rx-python
behaves identically, and so does `GET /v1/samples`.

### Context at the ends of the file

`before_context` and `after_context` in `--json` echo the window you
asked for (the default is 3). Each window is clamped at line 1 and at
the last line of the file, so a sample near either end has fewer lines
than the echo suggests: `--lines=1 --context=3` reports
`before_context: 3` and prints lines 1-4.

### A position the file does not have

A line past the last one, line 0, or a byte offset past the last byte is
answered rather than refused: `-1` in `lines`/`offsets`, `null` in
`samples`, and the reason on stderr (`Warning: line 99 is not in the
file.`). The command exits 0 and every other
position in the same request is still answered — one bad number in
`--lines=1000,1500000` must not throw away the good one, and `--json`
would otherwise have nothing to return.

Exit 2 is reserved for a malformed argument: `--lines=abc`, or a reversed
range like `--lines=50-10`.

`-1` is the same "asked but unknown" convention a capped search uses for
a match it could not number. rx-python answers identically, on plain,
gzipped and seekable-zstd files alike.

### Address syntax

Both `--offsets` and `--lines` accept the same grammar:

- Single: `100`
- Range: `100-200`
- Negative (line mode only): `-1` means "last line", `-10` means "10th from the end"
- Multiple, comma-separated: `100,500,1000-1050,-5`
- Multiple, by repeating the flag: `-b 100 -b 500`, which may be mixed
  with the comma form

No whitespace is permitted between commas or hyphens.

Several positions are answered from one pass over the file, so asking
about twenty of them costs about what asking about one costs. The viewer
relies on that: a capped search leaves it with a batch of match offsets
whose line numbers it wants at once.

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

### Single line with default context

```bash
rx samples /var/log/nginx/access.log --lines=10000
```

Prints lines 9997-10003 (±3 context lines by default). Line 10000 is
the center. If a cached line index exists for `access.log`, the read
starts at the nearest checkpoint; otherwise it starts at byte 0.

### Multiple byte offsets

```bash
rx samples /tmp/audit-2026-03.log --offsets=1024,524288,1048576 --context=1
```

Prints 3 lines around each of the three byte offsets. Useful directly
after an `rx trace --json` run — the match offsets from the trace
response plug straight into `--offsets` to retrieve content.

### Range query with asymmetric context

```bash
rx samples /var/log/audit-2026-03.log --lines=5000-5100 --before=0 --after=0
```

Prints exactly lines 5000-5100 with no surrounding context. Equivalent
to `sed -n '5000,5100p'`, but with an index the read starts at the
nearest checkpoint.

### Negative line numbers

```bash
rx samples /var/log/audit-2026-03.log --lines=-100--1 --context=0
```

The last 100 lines of the file, like `tail -n 100`. On a plain file
this is fast with or without an index (`--lines=-50--1` on the 465 MB
log: 11 ms either way). On a gzip, bzip2, xz or plain zstd file it
costs a second pass over the stream, to count the lines first.

### Highlight matches within context

```bash
rx samples /var/log/nginx/access.log --lines=5000 --regex="5[0-9]{2} [0-9]+$" --color=always
```

Prints the line and context; any substring matching the regex is
wrapped in ANSI bright-red escape codes. `--regex` is purely a display
concern — it doesn't filter which lines are printed.

### JSON for programmatic use

```bash
rx samples /var/log/audit-2026-03.log --lines=100,200,300 --json \
    | jq '.samples'
```

The JSON response has a `samples` map keyed by the original range
string, each mapping to an array of lines. `lines` maps each single
line to the byte offset where it starts (`-1` for a range, and for a
line past the end); `offsets` maps each offset to the number of the
line holding it. From a 30-line file whose lines read `LINE <n>
payload`, `--lines=1,30,99 --json`:

```json
{
  "path": "/var/log/lines.log",
  "offsets": {},
  "lines": {"1": 0, "30": 455, "99": -1},
  "before_context": 3,
  "after_context": 3,
  "samples": {
    "1":  ["LINE 1 payload", "LINE 2 payload", "LINE 3 payload", "LINE 4 payload"],
    "30": ["LINE 27 payload", "LINE 28 payload", "LINE 29 payload", "LINE 30 payload"],
    "99": null
  },
  "is_compressed": false,
  "compression_format": null,
  "cli_command": null
}
```

### Compressed file (line mode only)

```bash
rx samples /var/log/audit-2026-03.log.gz --lines=5000 --context=2
```

For `.gz`, `.bz2`, `.xz`, or plain (non-seekable) `.zst` files, only
line-offset mode works. `rx` streams the decompressor from the start
to the end of the file and captures lines as it passes them, so every
lookup costs one full decompression, whatever the line. The offsets in
`lines` are positions in the decompressed text. For random access on
compressed data, use [`rx compress`](compress.md) to re-encode as
seekable zstd.

## How it works

### Byte-offset mode (uncompressed files)

1. Parse the offsets spec into a list of (offset, range-end) pairs.
2. For each offset, seek to that byte position.
3. Walk backward from there to the start of the enclosing line.
4. Use the line index (if present) to compute the absolute line number;
   otherwise count `\n`s to that point.
5. Emit the enclosing line plus context lines.

To number the line, the read starts at the nearest index checkpoint
before the offset, or at byte 0 without an index (15 ms for an offset
403 MB into the 465 MB log).

### Line-offset mode

1. Parse the lines spec into a list of (line, range-end) pairs. Negative
   values require the total line count — either from the cached index
   or from a one-time linear count.
2. For each line, consult the line index. The index returns the byte
   offset of the nearest prior checkpoint.
3. From that checkpoint, scan forward counting `\n`s until the target
   line is reached.
4. Walk to gather the requested context lines.

With a cached index the forward-scan distance is bounded by the index
step (1 MB by default). Without an index, the walk starts from the
beginning of the file — fine for low line numbers, slower for high
ones.

### Performance characteristics

Measured on a 465 MB log in the page cache, whole command included:

- **Byte offset 403 MB in, no index**: 15 ms; with an index: 14 ms.
- **Line 700000 with an index**: 13 ms; without one: 90 ms.
- **Multiple addresses in one call**: amortized — the file is opened
  once and scanned once, gathering all targeted windows.
- **Compressed line mode**: proportional to the target line number
  because the decompressor has no random access.

## Tips and gotchas

!!! tip "Chain `rx trace` → `rx samples`"
    Run `rx trace --json` to find matches, then pipe the byte offsets
    through `jq` to `rx samples`:
    ```bash
    offsets=$(rx "5xx" access.log --json | jq -r '.matches | map(.offset) | join(",")')
    rx samples access.log --offsets="$offsets" --context=5
    ```

!!! warning "Byte offsets don't work on compressed files"
    The offsets `rx` reports for a compressed file are positions in the
    decompressed text, and `samples` cannot seek there without
    decompressing everything before. It refuses the combination (exit 2,
    `Byte offsets are not supported for compressed files; use lines
    instead`). Use `--lines` with the match's line number.

!!! note "Negative line numbers on compressed files cost a second pass"
    On a gzip, bzip2, xz or plain zstd file, `--lines=-1` streams the
    file once to count its lines and once more to read them. On a plain
    file a negative line is cheap.

!!! warning "`--regex` is cosmetic, not a filter"
    `--regex` only controls highlight styling in the terminal. It
    doesn't restrict which samples are returned. To search-then-retrieve,
    use `rx trace` first to find the lines, then `rx samples` to pull
    them.

## See also

- [concepts/byte-offsets-vs-line-numbers](../concepts/byte-offsets-vs-line-numbers.md)
- [concepts/line-indexes](../concepts/line-indexes.md)
- [`rx index`](line-index.md) — build an index for fast line-number seeks
- [`rx trace`](trace.md) — find patterns to feed into `samples`
- [api/endpoints/samples](../api/endpoints/samples.md) — the same over HTTP
