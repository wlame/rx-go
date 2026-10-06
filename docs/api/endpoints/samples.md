# `GET /v1/samples`

Retrieve content lines addressed by byte offset, line number or time,
with configurable context. HTTP equivalent of [`rx samples`](../../cli/samples.md).

## Purpose

Given a file and a set of addresses, return the targeted lines plus
surrounding context. Supports byte-offset, line-offset and time modes, on uncompressed files
and on compressed files (gzip, bzip2, xz, zstd, seekable zstd).

### What a request reads

- **First lookup in a large or compressed file:** when no index is
  cached, one is built and stored first — for any compressed file, and
  for a plain file of `RX_LARGE_FILE_MB` (50 MB) or more. The build runs
  as a background `index` task, and the request waits for it. With
  `Prefer: respond-async` it waits up to `RX_SAMPLES_WAIT_SECONDS` (5 s
  by default): a small file's build ends in time and the request answers
  `200` with the lines; a large one's does not, and the request answers
  [`202`](#response-202-accepted) with the task. Without the header the
  request waits for the build and answers `200`. `RX_NO_INDEX=true` on
  the server turns the build off, and the lookup reads no stored index
  either.
- **Plain file with an index:** seeks to the nearest checkpoint before
  the first wanted line and reads through the last one. Line 700000 of
  the 465 MB log took 13 ms from the CLI.
- **Plain file without an index:** reads from byte 0 to the last wanted
  line, then stops — 12 ms for lines 1-1000, 90 ms for line 700000 of
  the same log.
- **gzip, bzip2, xz, plain zstd:** decompresses from the first byte up
  to the last wanted line, then stops. A line counted from the end takes
  the line count from the index; without one it costs a full pass to
  count the lines first.
- **Seekable zstd with an index:** decompresses only the frames that
  hold the wanted lines.
- **A sample whose first line has no timestamp of its own** (a
  traceback line) in a file with a timestamp format: the text before
  that line is read back, at most `RX_TIMESTAMP_LOOKBACK_KB` KiB (64 by
  default), for `line_timestamps`. A plain file and a seekable zstd file
  read just that; a gzip, bzip2, xz or plain zstd file decompresses its
  text up to the line once more. Never with the index.

## Request

```text
GET /v1/samples?path=...&lines=...
GET /v1/samples?path=...&offsets=...
GET /v1/samples?path=...&timestamps=...&timestamps=...
```

### Query parameters

| Parameter | Type | Required | Default | Description |
|---|---|:-:|---|---|
| `path` | `string` | yes | — | File path (must be a file, not a directory) |
| `offsets` | `string` | one of three | — | Comma-separated byte offsets / ranges |
| `lines` | `string` | one of three | — | Comma-separated 1-based line numbers / ranges |
| `timestamps` | `string`, repeatable | one of three | — | A time or time range (`T`, `T1..T2`, `..T2`, `T1..`); repeat the parameter for several, at most 1,000. Never split at commas. See [by time](#by-time) |
| `context` | `int` | no | `3` | Lines before AND after each target, at most 100 (`-1` = default) |
| `before_context` | `int` | no | `3` | Lines before (overrides `context`), at most 100 (`-1` = default) |
| `after_context` | `int` | no | `3` | Lines after (overrides `context`), at most 100 (`-1` = default) |

### Request header

| Header | Value | Description |
|---|---|---|
| `Prefer` | `respond-async` | Lets the server answer [`202`](#response-202-accepted) with the task building the file's line index when the build outlasts `RX_SAMPLES_WAIT_SECONDS` ([RFC 7240](https://www.rfc-editor.org/rfc/rfc7240)). Without it the request waits for the build, however long, and answers `200`. |

Exactly one of `offsets` / `lines` / `timestamps` must be provided.
More than one, or none, returns `400`. For a compressed file both are positions in
its decompressed text: `offsets` names bytes of that text, and the
`lines` map reports where each line starts in it.

### Address syntax

Both `offsets` and `lines` accept the same grammar:

- Single: `100`
- Range: `100-200`
- Negative (line mode only): `-1` = last line
- Multiple: `100,500,1000-1050,-5`

No whitespace. Max practical length is limited by the URL length cap
your proxy imposes.

### By time

`timestamps=T` answers the first line, in file order, whose own
timestamp is T or later, with its context, as `lines=N` answers line N.
`timestamps=T1..T2` answers the lines from the line at T1 to the line
before the first line later than T2, without context; `..T2` and `T1..`
leave an end open. The rule, the accepted forms of a time and the zone
settings (`RX_LOG_TZ`, `RX_QUERY_TZ`) are in
[Timestamps](../../concepts/timestamps.md). The parameter repeats, one
query per value, because a comma is part of some timestamp formats
(`2025-12-10 12:34:56,123`):

```bash
curl -sG 'http://127.0.0.1:7777/v1/samples' --data-urlencode 'path=/var/log/app.log' \
    --data-urlencode 'timestamps=2025-12-10 12:34:56,123' --data-urlencode 'timestamps=12:34:56..12:34:57' \
    --data-urlencode 'context=1'
```

```json
{
  "path": "/var/log/app.log",
  "offsets": {},
  "lines": {},
  "before_context": 1,
  "after_context": 1,
  "samples": {
    "12:34:56..12:34:57": [
      "2025-12-10 12:34:56,123 ERROR LINE 2",
      "Traceback LINE 3",
      "2025-12-10 12:34:57,000 INFO LINE 4"
    ],
    "2025-12-10 12:34:56,123": [
      "2025-12-10 12:34:55,000 INFO LINE 1",
      "2025-12-10 12:34:56,123 ERROR LINE 2",
      "Traceback LINE 3"
    ]
  },
  "is_compressed": false,
  "compression_format": null,
  "cli_command": "rx samples /var/log/app.log --timestamps='2025-12-10 12:34:56,123' --timestamps=12:34:56..12:34:57 --context=1",
  "timestamps": {"12:34:56..12:34:57": 2, "2025-12-10 12:34:56,123": 2},
  "time_format": {"format": "iso", "has_zone": false, "assumed_zone": "UTC"},
  "line_timestamps": {
    "12:34:56..12:34:57": [1765370096123, 1765370096123, 1765370097000],
    "2025-12-10 12:34:56,123": [1765370095000, 1765370096123, 1765370096123]
  }
}
```

A time no line reaches answers `-1` and a `null` sample, like a line
past the end. A value that is not a time, a time of day on a file whose
first and last timestamps fall on two dates (the message names both),
and a time query on a file with no timestamp format ("no timestamp
format recognized in the first 1 MiB") answer `400`. With an index the
search reads at most one index step; the request builds or waits for
an index exactly as a `lines` request does.

### Context defaults

The `-1` sentinel is the "not provided" marker for `context`,
`before_context`, and `after_context` (huma query params can't
distinguish absent from `0`). Omit the param entirely or pass `-1` to
get the default `3`. `before_context` and `after_context`, `0`
included, override `context`. Each is capped at 100 lines, the cap
`/v1/trace` has; a larger value is a `422`. A wider read is a line
range (`lines=100-1100`), which has no cap.

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
  "cli_command": "rx samples /var/log/lines.log --lines=1,30,99",
  "timestamps": {},
  "time_format": null,
  "line_timestamps": null
}
```

and `offsets=20,30-50,9999&context=0` on the same file answers
`"offsets": {"20": 2, "30-50": 3, "9999": -1}` with
`"samples": {"20": ["LINE 2 payload"], "30-50": ["LINE 3 payload", "LINE 4 payload"], "9999": null}`.

## Response — 202 Accepted

When the request sends `Prefer: respond-async` and the file's line index
is still being built after `RX_SAMPLES_WAIT_SECONDS`, the answer names
the build's task instead of the lines, with the header
`Preference-Applied: respond-async`:

```json
{
  "task_id": "1b9e4c1e-3f7a-4c55-9a2e-6d0f7b1c2a10",
  "status": "running",
  "message": "Building the line index of /var/log/core.log; poll GET /v1/tasks/1b9e4c1e-3f7a-4c55-9a2e-6d0f7b1c2a10 and ask again when it completes",
  "path": "/var/log/core.log",
  "started_at": "2026-10-03T18:12:04.512330Z"
}
```

Poll [`GET /v1/tasks/{task_id}`](tasks.md) until its `status` is
`completed` or `failed` (its `progress` says how far the build has
read), then send the same request again. A failed build does not fail
the lookup: the next request reads the file without an index, slower
and with the same answer.

There is one build per file at a time, shared by every request for it:
a second request while it runs gets the same `task_id`, and so does a
request while a `POST /v1/index` task for the file runs. A request that
gives up waiting, or whose client disconnects, leaves the build running;
its index serves every later request. A request for a file that changed
after the build started does not wait for that build and is answered
from the file.

A request without `Prefer: respond-async` never gets a `202`: it waits
for the same shared build and answers `200`, as a client written before
contract 1.4 expects. `rx samples` never answers this way either: the
CLI waits for the build.

```bash
curl -sG -H 'Prefer: respond-async' 'http://127.0.0.1:7777/v1/samples' \
    --data-urlencode 'path=/var/log/core.log.gz' --data-urlencode 'lines=1-100' \
    -o answer.json -w '%{http_code}\n'
```

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
| `timestamps` | `{query: lineNumber}` | Time mode: key is the query, value is the line it found (a range's first line); `-1` when no line is at the time, or the range holds none. `{}` in the other modes |
| `time_format` | `{format, has_zone, assumed_zone} \| null` | The file's timestamp format, in every mode: the family (`iso`, `clf`, `ctime`, `syslog`, `slash`, `dotted`, `epoch`), whether most timestamps carry a zone, and the zone a timestamp without one is read in (`RX_LOG_TZ` for a zone-less file, `UTC` otherwise). `null` when no format is recognized in the first mebibyte of the text. Costs no read with an index, at most a mebibyte without |
| `line_timestamps` | `{key: (ms \| null)[] \| null} \| null` | The effective timestamp of each sample line, in every mode, keyed and ordered as `samples`: milliseconds since the Unix epoch as a UTC instant (a zone-less file's wall clock read in `RX_LOG_TZ`). A line without a timestamp of its own carries that of the nearest earlier line with one, when that line starts at most `RX_TIMESTAMP_LOOKBACK_KB` KiB (64) before it; otherwise `null`, and `null` too when the text before the sample cannot be read back (a damaged frame the sample itself does not need; the server logs `line_timestamps_read_back_failed`). A key whose sample is `null` maps to `null`; the whole field is `null` when `time_format` is. See [effective timestamps](../../concepts/timestamps.md#effective-timestamps) |

### Map ordering

Both `lines`/`offsets` and `samples` are JSON objects. Go emits keys
in alphabetical order, not the request order. If you need results in
the order you asked for them, track the original request string
client-side and iterate accordingly.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Success; a position the file does not have answers `-1` in `lines`/`offsets` and `null` in `samples` |
| `202 Accepted` | Only with `Prefer: respond-async`: the file's index is being built and did not finish within `RX_SAMPLES_WAIT_SECONDS`; the body names the task (see [above](#response-202-accepted)) |
| `400 Bad Request` | None of `offsets`, `lines` and `timestamps`; more than one; bad spec syntax; a value of `timestamps` that is not a time, a time of day on a file of two dates, a time query on a file without timestamps, more than 1,000 values; `path` is a directory; the file is not text; an answer of more lines than `RX_SAMPLES_MAX_LINES` (100,000 by default) allows |
| `403 Forbidden` | Path outside `--search-root`; a file the server may not read (`Permission denied: <path>`, as `rx samples` exits 4) |
| `404 Not Found` | File doesn't exist |
| `422 Unprocessable Entity` | A context count above 100, or a missing `path` |
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

### Compressed file

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

### Bad spec syntax

```json
{
  "detail": "Invalid lines format: invalid range format: 100-. Both values must be integers"
}
```

Status: `400`. Check the [address syntax](#address-syntax).

### Missing address mode

```json
{ "detail": "Must provide one of 'offsets', 'lines' or 'timestamps'." }
```

Status: `400`. Supply exactly one.

### Answer over the line limit

```json
{ "detail": "too many lines for one samples answer: the answer reached 100001 lines, more than the 100000 allowed; RX_SAMPLES_MAX_LINES sets the limit: ask for fewer positions, shorter ranges or less context" }
```

Status: `400`. The lines of all the samples are counted as they are
read (a line in two samples counts twice), and the request stops
reading when the count passes `RX_SAMPLES_MAX_LINES`. `rx samples` has
no limit.

### File is a directory

```json
{ "detail": "Path is a directory, not a file: /var/log/sub" }
```

Status: `400`.

### File is not text

```json
{ "detail": "not a text file: a NUL byte in the first 8 KiB of its decompressed text: /var/log/logs.tar.gz" }
```

Status: `400`. A file whose text holds a NUL byte in its first 8 KiB
(decompressed for a compressed file) has no lines; see
[Compression](../../concepts/compression.md#how-rx-decides-what-a-file-is).

## Performance notes

- With a cached line index, a line lookup reads from the nearest
  checkpoint (1 MB apart by default), whatever the line number
- Without an index, line lookups scale linearly with line number
- Byte-offset mode reads from the nearest checkpoint before the offset,
  or from byte 0 without an index, to number the line
- Multiple addresses in one request are amortized — the text is
  walked once, in order of position, up to the last address; with an
  index the walk seeks over the gaps between addresses
- A client that disconnects stops the read: the lookup ends at its
  next buffer of text
- Compressed line-mode (except seekable zstd with an index) streams the
  decompressed file up to the last wanted line

## See also

- [`rx samples`](../../cli/samples.md) — CLI equivalent
- [`/v1/index`](line-index.md) — build an index first for fast line lookups
- [`/v1/trace`](trace.md) — find match offsets to feed into samples
- [concepts/line-indexes](../../concepts/line-indexes.md) — index-aware seek explained
