# `GET /v1/samples`

Retrieve content lines addressed by byte offset or line number, with
configurable context. HTTP equivalent of [`rx samples`](../../cli/samples.md).

## Purpose

Given a file and a set of addresses, return the targeted lines plus
surrounding context. Supports both byte-offset and line-offset modes.
Works on uncompressed files in both modes and on compressed files
(gzip, bzip2, xz, zstd, seekable zstd) in line-offset mode only.

### What a request reads

- **First lookup in a large or compressed file:** when no index is
  cached, the request first builds and stores one — for any compressed
  file, and for a plain file of `RX_LARGE_FILE_MB` (50 MB) or more. That
  reads the whole file inside the request: 137 ms for a 465 MB log
  already in the page cache, longer from disk. `RX_NO_INDEX=true` on the
  server turns it off. The build is not shared between concurrent
  requests and has no deadline.
- **Plain file with an index:** seeks to the nearest checkpoint before
  the first wanted line and reads through the last one. Line 700000 of
  the 465 MB log took 13 ms from the CLI.
- **Plain file without an index:** reads from byte 0 to the last wanted
  line, then stops — 12 ms for lines 1-1000, 90 ms for line 700000 of
  the same log.
- **gzip, bzip2, xz, plain zstd:** streams the whole decompressed file
  for every request, whatever lines are asked, index or not — 1.6 s for
  a 113 MB `.gz` of that log.
- **Seekable zstd with an index:** decompresses only the frames that
  hold the wanted lines.

## Request

```text
GET /v1/samples?path=...&lines=...
GET /v1/samples?path=...&offsets=...
```

### Query parameters

| Parameter | Type | Required | Default | Description |
|---|---|:-:|---|---|
| `path` | `string` | yes | — | File path (must be a file, not a directory) |
| `offsets` | `string` | one of two | — | Comma-separated byte offsets / ranges |
| `lines` | `string` | one of two | — | Comma-separated 1-based line numbers / ranges |
| `context` | `int` | no | `3` | Lines before AND after each target (`-1` = default) |
| `before_context` | `int` | no | `3` | Lines before (overrides `context`) (`-1` = default) |
| `after_context` | `int` | no | `3` | Lines after (overrides `context`) (`-1` = default) |

Exactly one of `offsets` / `lines` must be provided. Both-set or
neither-set returns `400`. `offsets` is refused for a compressed file.

### Address syntax

Both `offsets` and `lines` accept the same grammar:

- Single: `100`
- Range: `100-200`
- Negative (line mode only): `-1` = last line
- Multiple: `100,500,1000-1050,-5`

No whitespace. Max practical length is limited by the URL length cap
your proxy imposes.

### Context defaults

The `-1` sentinel is the "not provided" marker for `context`,
`before_context`, and `after_context` (huma query params can't
distinguish absent from `0`). Omit the param entirely or pass `-1` to
get the default `3`. `before_context` and `after_context`, `0`
included, override `context`.

The response's `before_context` and `after_context` echo the window
the request asked for. Each window is clamped at line 1 and at the last
line of the file, so a sample near either end has fewer lines than the
echo suggests: `lines=1&context=3` answers `before_context: 3` and four
lines, lines 1-4.

## Response — 200 OK

For a 30-line file whose every line reads `LINE <n> payload`,
`lines=1,30,99&context=3` answers:

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
  "cli_command": "rx samples /var/log/lines.log --lines=1,30,99"
}
```

and `offsets=20,30-50,9999&context=0` on the same file answers
`"offsets": {"20": 2, "30-50": 3, "9999": -1}` with
`"samples": {"20": ["LINE 2 payload"], "30-50": ["LINE 3 payload", "LINE 4 payload"], "9999": null}`.

### Response fields

| Field | Type | Description |
|---|---|---|
| `path` | string | The validated absolute file path |
| `offsets` | `{key: lineNumber}` | Byte-offset mode: key is the request spec, value is the 1-based number of the line holding that byte (for a range, its first byte); `-1` past the end of the file |
| `lines` | `{key: byteOffset}` | Line-offset mode: key is the request spec, value is the byte offset where that line starts (for a compressed file, a position in the decompressed text); `-1` for a range key and for a line past the end |
| `before_context`, `after_context` | int | The context the request asked for (or the default 3), echoed; see [context defaults](#context-defaults) |
| `samples` | `{key: lines[] \| null}` | Retrieved content, keyed identically to `offsets` / `lines`; `null` for a position the file does not have |
| `is_compressed` | bool | Whether the file was compressed |
| `compression_format` | `string \| null` | `"gzip"`, `"bzip2"`, `"xz"`, `"zstd"`, `"seekable_zstd"`, or `null` |
| `cli_command` | string | Equivalent CLI invocation |

### Map ordering

Both `lines`/`offsets` and `samples` are JSON objects. Go emits keys
in alphabetical order, not the request order. If you need results in
the order you asked for them, track the original request string
client-side and iterate accordingly.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Success; a position the file does not have answers `-1` in `lines`/`offsets` and `null` in `samples` |
| `400 Bad Request` | Missing both `offsets` and `lines`; both set; byte offsets on compressed file; bad spec syntax; `path` is a directory |
| `403 Forbidden` | Path outside `--search-root` |
| `404 Not Found` | File doesn't exist |
| `500 Internal Server Error` | Resolver failure; logged with stack |

Samples reads the file itself and never runs `ripgrep`, so it answers on
a server where `rg` is missing.

## Examples

### Single line with default context

```bash
curl -s 'http://127.0.0.1:7777/v1/samples?path=/var/log/audit-2026-03.log&lines=100' \
    | jq '.samples'
```

### Multiple lines

```bash
curl -sG 'http://127.0.0.1:7777/v1/samples' \
    --data-urlencode 'path=/var/log/audit-2026-03.log' \
    --data-urlencode 'lines=100,5000,99999-100010' \
    --data-urlencode 'context=2' \
    | jq '.samples | keys'
```

### Byte offsets

```bash
curl -sG 'http://127.0.0.1:7777/v1/samples' \
    --data-urlencode 'path=/var/log/audit-2026-03.log' \
    --data-urlencode 'offsets=1024,524288,1048576' \
    --data-urlencode 'context=1' \
    | jq '.samples'
```

### Chained with `/v1/trace`

```bash
# Get match offsets from a trace.
offsets=$(curl -sG 'http://127.0.0.1:7777/v1/trace' \
    --data-urlencode 'path=/var/log/app-2026-03.log' \
    --data-urlencode 'regexp=ERROR' \
    | jq -r '.matches | map(.offset | tostring) | join(",")')

# Retrieve context around each match.
curl -sG 'http://127.0.0.1:7777/v1/samples' \
    --data-urlencode "path=/var/log/app-2026-03.log" \
    --data-urlencode "offsets=$offsets" \
    --data-urlencode 'context=5' \
    | jq '.samples'
```

### Compressed file (line mode only)

```bash
curl -sG 'http://127.0.0.1:7777/v1/samples' \
    --data-urlencode 'path=/var/log/audit-2026-03.log.gz' \
    --data-urlencode 'lines=5000' \
    --data-urlencode 'context=2' \
    | jq '{compressed: .is_compressed, format: .compression_format, samples}'
```

### Asymmetric context

```bash
curl -sG 'http://127.0.0.1:7777/v1/samples' \
    --data-urlencode 'path=/var/log/audit-2026-03.log' \
    --data-urlencode 'lines=10000' \
    --data-urlencode 'before_context=0' \
    --data-urlencode 'after_context=20'
```

Returns line 10000 plus the next 20 lines, no preceding context.

## Error examples

### Byte offsets on compressed file

```json
{
  "detail": "Byte offsets are not supported for compressed files. Use 'lines' parameter instead."
}
```

Status: `400`. Reaching a position in the decompressed text would mean decompressing everything before it; use `lines`.

### Bad spec syntax

```json
{
  "detail": "Invalid lines format: invalid range format: 100-. Both values must be integers"
}
```

Status: `400`. Check the [address syntax](#address-syntax).

### Missing address mode

```json
{ "detail": "Must provide either 'offsets' or 'lines' parameter." }
```

Status: `400`. Supply exactly one.

### File is a directory

```json
{ "detail": "Path is a directory, not a file: /var/log/sub" }
```

Status: `400`.

## Performance notes

- With a cached line index, a line lookup reads from the nearest
  checkpoint (1 MB apart by default), whatever the line number
- Without an index, line lookups scale linearly with line number
- Byte-offset mode reads from the nearest checkpoint before the offset,
  or from byte 0 without an index, to number the line
- Multiple addresses in one request are amortized — the file is
  opened once and walked once
- Compressed line-mode (except seekable zstd with an index) streams the
  whole decompressed file

## See also

- [`rx samples`](../../cli/samples.md) — CLI equivalent
- [`/v1/index`](line-index.md) — build an index first for fast line lookups
- [`/v1/trace`](trace.md) — find match offsets to feed into samples
- [concepts/line-indexes](../../concepts/line-indexes.md) — index-aware seek explained
