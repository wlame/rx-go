# `rx samples`

Retrieve lines of content from a file, addressed by byte offset, line
number or time, with optional surrounding context.

## Synopsis

```text
rx samples PATH -b OFFSETS [flags]
rx samples PATH -l LINES   [flags]
rx samples PATH -t TIME    [flags]
```

Exactly one of `-b` / `--offsets`, `-l` / `--lines` or `-t` /
`--timestamps` is required; combining them exits 2.

## Description

`rx samples` is a content-retrieval tool — given a file and a set of
addresses, it prints the targeted lines along with configurable context
above and below each one. The command has three address modes:

- **Byte offset** (`--offsets`): each value is a byte position; the line
  containing that byte is the target. Works on any uncompressed file;
  refused for a compressed one.
- **Line number** (`--lines`): each value is a 1-based line number. When
  a cached line index exists, the read starts at the nearest checkpoint;
  otherwise it starts at byte 0.
- **Time** (`--timestamps`): each value is a time or a time range; the
  target is the first line whose own timestamp is at or after the time.
  See [By time](#by-time) and [Timestamps](../concepts/timestamps.md).

The first two modes accept single values, ranges, and comma-separated
lists. A
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

A gzip, bzip2, xz or plain zstd file is decompressed from its first
byte up to the last wanted line, then the read stops. A seekable zstd
file with an index decompresses only the frames that hold the wanted
lines.

When the first line of a sample has no timestamp of its own, the text
before it is read back for `line_timestamps`: at most
`RX_TIMESTAMP_LOOKBACK_KB` KiB (64 by default) of a plain or seekable
zstd file, never using the index. A gzip, bzip2, xz or plain zstd file
cannot be entered there, so it decompresses its text up to that line
once more.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-b`, `--offsets` | `string` | — | Comma-separated byte offsets / ranges |
| `-l`, `--lines` | `string` | — | Comma-separated 1-based line numbers / ranges |
| `-t`, `--timestamps` | `string` | — | A time or time range (`T`, `T1..T2`, `..T2`, `T1..`); repeat the flag for several, never comma-separated |
| `-c`, `--context` | `int` | `3` | Lines before AND after each target (no cap here; `GET /v1/samples` stops at 100) |
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
file, and `rx samples` waits for it however long it takes. The build
runs without analysis. Every format uses the index: a plain file seeks
to its checkpoints, a seekable zstd file decodes only the frames its
frame table names, and a gzip, bzip2, xz or plain zstd stream, which
must still be decompressed from its first byte, takes its line count
for a line counted from the end and starts counting lines at the
checkpoint before the first wanted one. `--no-index` turns the build off
and the lookup neither builds nor reads an index, even one already
stored: it reads the file from the start — slower, same answer. It is
the flag to reach for when an index is suspect.

`GET /v1/samples` builds the same index as a background task shared by
every request for the file, and answers a request that sends
`Prefer: respond-async` with `202` and the task when the build outlasts
`RX_SAMPLES_WAIT_SECONDS`; see
[`GET /v1/samples`](../api/endpoints/samples.md#response-202-accepted).

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
range like `--lines=50-10`, and for a path that has no lines to give: a
directory, or a file whose text is not text (a NUL byte in the first
8 KiB of its text, decompressed for a compressed file: a `.tar.gz`, a
binary file, UTF-16). That file is refused with the reason, as in
`Error: Not a text file: a NUL byte in the first 8 KiB of its decompressed text: logs.tar.gz`,
and no index is built for it. See
[Compression](../concepts/compression.md#how-rx-decides-what-a-file-is).

`-1` is the same "asked but unknown" convention a capped search uses for
a match it could not number. rx-python answers identically, on plain,
gzipped and seekable-zstd files alike.

### By time

`--timestamps=T` finds the first line whose own timestamp is T or
later and prints it with its context, as `--lines` prints that line.
`--timestamps=T1..T2` prints the lines from the line at T1 to the last
line before the first line later than T2, without context; either end
may be left open. A time may be written as ISO 8601 or RFC 3339 (zone
optional), a date, a time of day, epoch seconds or milliseconds, or
exactly as the file writes it. Each query is its own flag:

```bash
rx samples app.log --timestamps='2025-12-10 12:34:56,123'
rx samples app.log -t 14:33:12..14:35:15 -t 15:00
rx samples app.log.gz --timestamps=2025-12-10T07:30:00Z --context=10
```

The heading names the line each query found:

```text
=== app.log:4 @ 2025-12-10 12:34:57,000 ===
2025-12-10 12:34:57,000 INFO LINE 4
```

A range heading names its lines (`app.log:2-4 @ 12:34:56..12:34:57`).
A query no line answers prints `:-1`, gives `-1` and a `null` sample in
`--json`, warns on stderr (`Warning: no line at or after 2026-01-01 in
the file.`) and exits 0. A value that is not a time, a time of day on a
file whose timestamps span two dates, and a file with no timestamp
format exit 2. In `--json`, `timestamps` maps each query to the line it
found (a range to its first line) and `samples` holds its lines under
the query.

With an index the search reads at most one index step from the
checkpoint before the answer; without one it reads from the first line.
The rule, the formats and the zone settings (`RX_LOG_TZ`,
`RX_QUERY_TZ`) are in [Timestamps](../concepts/timestamps.md).

### Address syntax

Both `--offsets` and `--lines` accept the same grammar (`--timestamps`
has its own, above):

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
  "cli_command": null,
  "timestamps": {},
  "time_format": null,
  "line_timestamps": null
}
```

`time_format` is the file's timestamp format, `{format, has_zone,
assumed_zone}`, in every mode, or `null` when no format is recognized
in the first mebibyte of its text, as here. Without an index it costs a
read of that mebibyte; with one, nothing. `timestamps` is filled only by
`--timestamps`.

`line_timestamps` gives each sample line its effective timestamp, keyed
and ordered as `samples`, in every mode: milliseconds since the Unix
epoch as a UTC instant, or `null`. A line without a timestamp of its
own (a traceback line) carries that of the nearest earlier line with
one, when that line starts at most `RX_TIMESTAMP_LOOKBACK_KB` KiB (64)
before it ([effective timestamps](../concepts/timestamps.md#effective-timestamps)).
The whole field is `null` when `time_format` is. Human output does not
show it: each line's text already carries its time. On a log whose
lines 2 to 4 read

```text
2025-12-10 12:34:56,123 ERROR LINE 2
Traceback LINE 3
2025-12-10 12:34:57,000 INFO LINE 4
```

`--lines=3 --context=1 --json` answers
`"line_timestamps": {"3": [1765370096123, 1765370096123, 1765370097000]}`:
the traceback line carries line 2's time.

### Compressed file

```bash
rx samples /var/log/audit-2026-03.log.gz --lines=5000 --context=2
rx samples /var/log/audit-2026-03.log.gz --offsets=403366791
```

Both modes work on `.gz`, `.bz2`, `.xz` and `.zst` files. Offsets are
positions in the decompressed text, in the request and in the answer:
the `lines` map reports where each line starts in that text, and
`--offsets=B` answers with the line holding byte B of it, the same
line the plain copy of the file gives. `--lines=N` and `--offsets=` the
offset of line N lead to each other.

A gzip, bzip2, xz or plain (non-seekable) zstd file has no way in except
from its first byte. In line mode `rx` decompresses up to the last
wanted line and captures lines as it passes them; in offset mode, up to
the line holding the last offset asked about. Either way the read stops
there, and with an index it also skips counting the lines before the
nearest checkpoint. A seekable `.zst` with an index decompresses only
the frames around the wanted lines or offsets. For random access on
compressed data, use [`rx compress`](compress.md) to re-encode as
seekable zstd.

## How it works

### Byte-offset mode

1. Parse the offsets spec into a list of (offset, range-end) pairs.
2. For each offset, seek to that byte position.
3. Walk backward from there to the start of the enclosing line.
4. Use the line index (if present) to compute the absolute line number;
   otherwise count `\n`s to that point.
5. Emit the enclosing line plus context lines.

To number the line, the read starts at the nearest index checkpoint
before the offset, or at byte 0 without an index (15 ms for an offset
403 MB into the 465 MB log). For a compressed file the same walk runs
over the decompressed text: for an indexed seekable `.zst`, from the
nearest frame before the offset's frame that holds a line break (the
frame just before it, unless a line longer than a frame spans several),
and from the first byte otherwise.

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

!!! note "Byte offsets on compressed files are positions in the text"
    The offsets `rx trace` reports for a compressed file are positions
    in the decompressed text, and `samples --offsets` takes them as they
    are. That is how a match a capped trace left without a line number
    (`-1`) gets one, on a compressed file as on a plain one.

!!! note "Negative line numbers on compressed files need the line count"
    On a gzip, bzip2, xz or plain zstd file, `--lines=-1` takes the line
    count from the index and decompresses the stream once to the end.
    Without an index (`--no-index`) it streams the file once to count its
    lines and once more to read them. On a plain file a negative line is
    cheap.

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
