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
  O(1) seek.
- **Line number** (`--lines`): each value is a 1-based line number. When
  a cached line index exists, the seek is O(1); otherwise it falls back
  to a linear scan.

Both modes accept single values, ranges, and comma-separated lists. A
single call can retrieve content from many locations across a file in
one pass.

### Bounded reads

`samples` reads only the portion of the file it needs to produce the
result. For a range `--lines=START-END`:

- With an index: seeks to the nearest checkpoint `<= START`, then reads
  lines from there to END. On a 1.3 GB file with a 10 MB-checkpoint
  index, a mid-file range (e.g. `--lines=5000000-5001000`) typically
  reads ~15 KB total and completes in ~5 ms.
- Without an index: reads from byte 0 until it reaches END, then stops.
  On a 1.3 GB file, a request for `--lines=1-1000` reads ~150 KB
  (NOT the whole file) and completes in ~5 ms.

Single-line queries with context (`--lines=N --context=K`) read only
the `2K+1`-line window around N.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-b`, `--offsets` | `string` | — | Comma-separated byte offsets / ranges |
| `-l`, `--lines` | `string` | — | Comma-separated 1-based line numbers / ranges |
| `-c`, `--context` | `int` | `3` | Lines before AND after each target |
| `-B`, `--before` | `int` | `0` | Lines before (overrides `--context`) |
| `-A`, `--after` | `int` | `0` | Lines after (overrides `--context`) |
| `--json` | `bool` | `false` | Emit machine-readable JSON |
| `--color` | `string` | auto | Force color: `always`, `never`, or empty (auto) |
| `--no-color` | `bool` | `false` | Alias for `--color=never` |
| `-r`, `--regex` | `string` | — | Highlight matches of this regex in context lines (requires color) |

### Address syntax

Both `--offsets` and `--lines` accept the same grammar:

- Single: `100`
- Range: `100-200`
- Negative (line mode only): `-1` means "last line", `-10` means "10th from the end"
- Multiple, comma-separated: `100,500,1000-1050,-5`

No whitespace is permitted between commas or hyphens.

## Examples

### Single line with default context

```bash
rx samples /var/log/nginx/access.log --lines=10000
```

Prints lines 9997-10003 (±3 context lines by default). Line 10000 is
the center. If a cached line index exists for `access.log`, this seeks
directly; otherwise the file is scanned linearly counting newlines.

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
to `sed -n '5000,5100p'` but with O(1) seek via the index.

### Negative line numbers

```bash
rx samples /var/log/audit-2026-03.log --lines=-100--1 --context=0
```

The last 100 lines of the file. Equivalent to `tail -n 100` but
backed by the index, so it works at any file size with stable latency.

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
string, each mapping to an array of context lines:

```json
{
  "path": "/var/log/audit-2026-03.log",
  "offsets": {},
  "lines":   { "100": 1024, "200": 2048, "300": 3072 },
  "before_context": 3,
  "after_context":  3,
  "samples": {
    "100": [ "...", "line 98", "line 99", "line 100", "line 101", "line 102", "..." ],
    "200": [ ... ],
    "300": [ ... ]
  },
  "is_compressed": false,
  "compression_format": null,
  "cli_command": "..."
}
```

### Compressed file (line mode only)

```bash
rx samples /var/log/audit-2026-03.log.gz --lines=5000 --context=2
```

For `.gz`, `.bz2`, `.xz`, or plain (non-seekable) `.zst` files, only
line-offset mode works. `rx` streams the decompressor from the start
and captures lines as it passes them. Performance degrades with line
number — line 5,000 is fast; line 5,000,000 decompresses everything
before it. For random access on compressed data, use
[`rx compress`](compress.md) to re-encode as seekable zstd.

## How it works

### Byte-offset mode (uncompressed files)

1. Parse the offsets spec into a list of (offset, range-end) pairs.
2. For each offset, seek to that byte position.
3. Walk backward from there to the start of the enclosing line.
4. Use the line index (if present) to compute the absolute line number;
   otherwise count `\n`s to that point.
5. Emit the enclosing line plus context lines.

Byte offsets are O(1) to seek regardless of where they land. This is
why byte offsets are the default unit throughout `rx`.

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
step (typically a few hundred KB). Without an index, the walk starts
from the beginning of the file — fine for low line numbers, slow for
high ones.

### Performance characteristics

- **Byte offsets, any file size**: sub-millisecond per offset once the
  file is open.
- **Line offsets with warm index**: low single-digit milliseconds per
  lookup on files up to 1 GB.
- **Line offsets without index**: linearly proportional to the target
  line number; a 10-millionth line on a 1.3 GB file takes several
  seconds.
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
    Byte offsets into a compressed stream have no stable meaning after
    partial decompression. `rx samples` will refuse the combination
    with a clear error. Use `--lines` instead, or re-encode the file
    with `rx compress` for random-access decompression.

!!! note "Negative line numbers need the total count"
    `--lines=-1` or any other negative value requires knowing the
    file's total line count. If the index has it cached, this is free;
    otherwise `rx` scans the whole file once. Build an index first for
    large files.

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
