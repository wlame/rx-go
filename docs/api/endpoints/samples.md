# `GET /v1/samples`

Retrieve content lines addressed by byte offset or line number, with
configurable context. HTTP equivalent of [`rx samples`](../../cli/samples.md).

## Purpose

Given a file and a set of addresses, return the targeted lines plus
surrounding context. Supports both byte-offset and line-offset modes.
Works on uncompressed files in both modes and on compressed files
(gzip, bzip2, xz) in line-offset mode only.

### Performance contract

`/v1/samples` reads ONLY the portion of the file needed for the
response — never the whole file. For line-number ranges:

- With a cached line index (`POST /v1/index` built at least once):
  seeks to the nearest checkpoint before the requested start line,
  then reads through the range. On a 1.3 GB file, a mid-file range
  request typically completes in ~5-10 ms (HTTP round-trip included).
- Without an index: reads from byte 0 to the end of the requested
  range, then stops. Still bounded, just slower for ranges that start
  deep in the file.

A request for `?lines=1-1000` on a 1.3 GB file reads roughly 150 KB
of file content; the response latency is dominated by HTTP
framing, not disk I/O.

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
neither-set returns `400`.

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
get the default `3`.

## Response — 200 OK

```json
{
  "path": "/var/log/audit-2026-03.log",
  "offsets": {},
  "lines": {
    "100":       1024,
    "200-205":   2048,
    "99999":     524288000
  },
  "before_context": 3,
  "after_context":  3,
  "samples": {
    "100": [
      "line 97 content",
      "line 98 content",
      "line 99 content",
      "line 100 content",
      "line 101 content",
      "line 102 content",
      "line 103 content"
    ],
    "200-205": [
      "line 197 content",
      "line 198 content",
      "line 199 content",
      "line 200 content",
      "line 201 content",
      "line 202 content",
      "line 203 content",
      "line 204 content",
      "line 205 content",
      "line 206 content",
      "line 207 content",
      "line 208 content"
    ],
    "99999": [ /* ... */ ]
  },
  "is_compressed":       false,
  "compression_format":  null,
  "cli_command":         "rx samples /var/log/audit-2026-03.log --lines=100,200-205,99999"
}
```

### Response fields

| Field | Type | Description |
|---|---|---|
| `path` | string | The validated absolute file path |
| `offsets` | `{key: byteOffset}` | Byte-offset mode: key is the request spec, value is the resolved byte offset |
| `lines` | `{key: byteOffset}` | Line-offset mode: key is the request spec, value is the starting byte offset of the first line in the range |
| `before_context`, `after_context` | int | Resolved context values used |
| `samples` | `{key: lines[]}` | Retrieved content, keyed identically to `offsets` / `lines` |
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
| `200 OK` | Success (returns empty arrays for any missing line numbers) |
| `400 Bad Request` | Missing both `offsets` and `lines`; both set; byte offsets on compressed file; bad spec syntax; `path` is a directory |
| `403 Forbidden` | Path outside `--search-root` |
| `404 Not Found` | File doesn't exist |
| `500 Internal Server Error` | Resolver failure; logged with stack |
| `503 Service Unavailable` | `ripgrep` not available |

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

Status: `400`. Byte offsets are undefined after partial decompression.

### Bad spec syntax

```json
{
  "detail": "Invalid lines format: could not parse 100-..."
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
{ "detail": "Path is a directory, not a file: /var/log" }
```

Status: `400`.

## Performance notes

- With a cached line index, line-mode lookups are low single-digit
  milliseconds regardless of line number
- Without an index, line lookups scale linearly with line number
- Byte-offset mode is always O(1) per offset on uncompressed files
- Multiple addresses in one request are amortized — the file is
  opened once and walked once
- Compressed line-mode streams the decompressor from the start — high
  line numbers take longer

## See also

- [`rx samples`](../../cli/samples.md) — CLI equivalent
- [`/v1/index`](line-index.md) — build an index first for fast line lookups
- [`/v1/trace`](trace.md) — find match offsets to feed into samples
- [concepts/line-indexes](../../concepts/line-indexes.md) — index-aware seek explained
