# `GET /v1/trace`

Search one or more files for regex patterns. HTTP equivalent of [`rx
trace`](../../cli/trace.md).

## Purpose

Execute a regex scan across one or more paths with parallel chunking,
returning match byte offsets and line numbers. Supports webhook
callbacks for progress notifications.

Every option of `rx trace` that shapes the answer has a query
parameter: the matching flags, the context window (`context`,
`before_context`, `after_context`) and the switches `no_cache`,
`no_index` and `no_recursive`. An answer is the one `rx trace --json`
gives with the same flags, and its `cli_command` carries them.

## Request

```text
GET /v1/trace?path=...&regexp=...&max_results=...
```

### Query parameters

| Parameter | Type | Required | Default | Description |
|---|---|:-:|---|---|
| `path` | `string[]` (repeatable) | yes | — | File or directory path(s) |
| `regexp` | `string[]` (repeatable) | yes | — | Regex pattern(s) |
| `max_results` | `int` | no | `0` (unlimited) | Cap on returned matches. When set, `rx` COOPERATIVELY CANCELS worker fanout once the cap is reached: in-flight ripgrep subprocesses receive SIGKILL and queued chunks are skipped. Use this for fast "first N hits" probes on large files. |
| `request_id` | `string` | no | auto | Custom UUID v7 request ID |
| `hook_on_file` | `string` | no | env | URL fired per file (overrides env) |
| `hook_on_match` | `string` | no | env | URL fired per match — requires `max_results` |
| `hook_on_complete` | `string` | no | env | URL fired once on completion |
| `ignore_case` | `bool` | no | `false` | Match case-insensitively (ripgrep `-i`) |
| `word_regexp` | `bool` | no | `false` | Match only whole words (ripgrep `-w`) |
| `line_regexp` | `bool` | no | `false` | Match only whole lines (ripgrep `-x`) |
| `fixed_strings` | `bool` | no | `false` | Treat every pattern as literal text (ripgrep `-F`) |
| `pcre2` | `bool` | no | `false` | Use the PCRE2 engine, for look-around and backreferences (ripgrep `-P`) |
| `context` | `int` | no | `0` | Lines before and after each match (`rx trace --context`), at most 100 |
| `before_context` | `int` | no | `-1` (the `context` value) | Lines before each match (`--before`, `-B`); wins over `context`, `0` included. At most 100 |
| `after_context` | `int` | no | `-1` (the `context` value) | Lines after each match (`--after`, `-A`); wins over `context`, `0` included. At most 100 |
| `no_cache` | `bool` | no | `false` | Neither read nor write the trace cache (`--no-cache`) |
| `no_index` | `bool` | no | `false` | Read and write no line index; a match a capped scan left unnumbered is numbered by counting from the start of the file (`--no-index`) |
| `no_recursive` | `bool` | no | `false` | For a directory path, search only the files directly inside it (`--no-recursive`) |

Repeat `path` and `regexp` to supply multiple values:

```text
GET /v1/trace?path=/var/log/a.log&path=/var/log/b.log&regexp=error&regexp=panic
```

Each repetition is one value, and a comma inside it is part of the
value: `regexp=a%7B2%2C5%7D` is the single pattern `a{2,5}`, and a file
named `a,b.log` is searched as one path. The OpenAPI document declares
both parameters `explode: true`.

### Matching flags

The five boolean parameters are the matching flags of [`rx
trace`](../../cli/trace.md#ripgreps-matching-flags), named after
ripgrep's long flags with `_` for `-`. They apply to every pattern in
the request, an answer is the one `rx trace` gives with the same flags,
and the response's `cli_command` carries them:

```text
GET /v1/trace?path=/var/log/app.log&regexp=connection%20reset&ignore_case=true
```

```json
"cli_command": "rx trace /var/log/app.log --regexp='connection reset' --ignore-case"
```

A value is `true` or `false` (`1` and `0` also work); anything else is a
`422`. A look-around or backreference without `pcre2=true` is a `400`,
like any other pattern ripgrep cannot compile. So is a pattern PCRE2
cannot compile under `pcre2=true`, with PCRE2's reason in `detail`, and
`pcre2=true` against a ripgrep built without PCRE2. The patterns are
checked before any file is read. These parameters arrived
with contract version 1.3; a client can read the version from
[`GET /health`](health.md) before relying on them.

### Context window

`context`, `before_context` and `after_context` are the `-C`, `-B` and
`-A` of [`rx trace`](../../cli/trace.md#context-flags-precedence), resolved the same
way: a given `before_context` or `after_context` wins over `context`,
`0` included, and `-1` (the default) means "take `context`". The window
fills `context_lines`, and the answer echoes it in `before_context` and
`after_context`:

```text
GET /v1/trace?path=/var/log/app.log&regexp=panic&context=3&after_context=10
```

```json
"before_context": 3,
"after_context": 10,
"cli_command": "rx trace /var/log/app.log --regexp=panic --context=3 --after=10"
```

Every match carries its own window, so the window multiplies the size
of the answer. Each side is capped at 100 lines; a larger value, or a
value below the minimum, is a `422`, and the OpenAPI document declares
the bound as the parameter's `maximum`. For a wider read around one
place in a file, pass its offset to [`GET /v1/samples`](samples.md).

### Cache, index and recursion

`no_cache`, `no_index` and `no_recursive` are the switches of the same
name in `rx trace`. `no_cache` and `no_index` change how rx reaches an
answer, never what it is, apart from the one difference stated under
[the line numbers](#matches-shape): with `no_index`, a capped scan
numbers every match it returns by counting lines from the start of the
file, where it would otherwise use a line index or report `-1`.
`no_recursive` changes which files a directory path covers.

These six parameters arrived with contract version 1.4.

### Required parameter validation

Missing `path` or `regexp` produces `422 Unprocessable Entity` with a
description of the missing field.

### Webhook URL validation

Hook URLs are validated against the SSRF policy (no loopback, private,
link-local, or CGNAT addresses). See [webhooks](../webhooks.md).

## Response — 200 OK

```json
{
  "request_id": "01936c8e-7b2a-7000-8000-000000000001",
  "path": ["/var/log/app-2026-03.log"],
  "time": 2.341,
  "patterns": {
    "p1": "timeout"
  },
  "files": {
    "f1": "/var/log/app-2026-03.log"
  },
  "matches": [
    {
      "pattern":               "p1",
      "file":                  "f1",
      "offset":                1024581,
      "relative_line_number":  9812,
      "absolute_line_number":  9812,
      "line_text":             "2026-03-14 08:23:51 ERROR timeout 5023ms",
      "submatches": [
        { "text": "timeout", "start": 29, "end": 36 }
      ],
      "line_text_truncated":   false,
      "submatches_truncated":  false
    }
  ],
  "scanned_files": ["/var/log/app-2026-03.log"],
  "skipped_files": [],
  "max_results":   null,
  "file_chunks": {
    "f1": 4
  },
  "context_lines":   {},
  "before_context":  null,
  "after_context":   null,
  "cli_command":     "rx trace /var/log/app-2026-03.log --regexp=timeout"
}
```

### Response fields

| Field | Type | Description |
|---|---|---|
| `request_id` | string | The `request_id` parameter, or a generated UUID v7. It is not the `X-Request-ID` header, which the HTTP layer sets on its own |
| `path` | `string[]` | The requested paths (validated & absolute) |
| `time` | number | Wall-clock elapsed seconds for the scan |
| `patterns` | `{id: pattern}` | Pattern ID → original string map |
| `files` | `{id: path}` | File ID → absolute path map |
| `matches` | array | See below |
| `scanned_files` | `string[]` | The files found by walking a directory named in `path`; empty when every path is a file. `files` lists every file searched either way |
| `skipped_files` | `string[]` | Paths passed over (not text, unreadable, a link the walk refuses, a subdirectory it cannot list) or not searched in full (a truncated stream, a damaged seekable frame: their matches are kept) |
| `skip_reasons` | `{path, reason}[]` | Why each path of `skipped_files` is there, in the same order; the reasons are listed in [`rx trace`](../../cli/trace.md#skipped-files-and-their-reasons). Contract 1.4 |
| `max_results` | `int \| null` | The cap that was applied, or null |
| `file_chunks` | `{fileId: N}` | How many chunks each file was split into (frames, for a seekable-zstd file); an answer from the trace cache reports the count of the scan that wrote it, whatever the chunk settings are now. It describes how the answer was produced and is not compared when an index or the cache answers |
| `context_lines` | `{matchKey: [...]}` | Keyed `pattern:file:offset` (`"p1:f1:60"`); each entry is the match's window, the match's own line included, as `{relative_line_number, absolute_line_number, line_text, absolute_offset, line_text_truncated}`. Without a context window an entry holds only the match's own line; see [`rx trace`](../../cli/trace.md#match-with-prepost-context) for how a window is built |
| `before_context`, `after_context` | `int \| null` | The window used on each side, or `null` when it is `0` |
| `cli_command` | string | Equivalent CLI command. A `request_id` and `hook_on_*` URLs the request gave appear as `--request-id` and `--hook-on-*`; a generated ID and the `RX_HOOK_*` fallbacks do not, since the command reads its own environment. See [conventions](../conventions.md#the-equivalent-cli-command) |

### `matches[]` shape

| Field | Type | Description |
|---|---|---|
| `pattern` | string | Pattern ID (key into `patterns`). A line that several patterns match appears once per pattern |
| `file` | string | File ID (key into `files`) |
| `offset` | int64 | Byte offset of the line start |
| `relative_line_number` | int | The same number as `absolute_line_number` when that is known; otherwise the line's number within the chunk or frame that found it, counted from its first line |
| `absolute_line_number` | int | The line's 1-based number in the file, or `-1` when a scan cut short by `max_results` did not read the bytes before the match |
| `line_text` | string | The matched line without its line break: the whole line, or its first `RX_MAX_LINE_TEXT_BYTES` bytes (see [Long lines](#long-lines)). A byte that is not part of a valid UTF-8 character reads as U+FFFD (see [Lines that are not valid UTF-8](#lines-that-are-not-valid-utf-8)) |
| `submatches` | array | `{text, start, end}` per match of this `pattern` on the line, as a search for that pattern alone reports them, at most `RX_MAX_SUBMATCHES_PER_LINE` of them. `start` and `end` are byte positions in the line |
| `line_text_truncated` | bool | `true` when `line_text` holds only the first bytes of a longer line |
| `submatches_truncated` | bool | `true` when `submatches` may leave some out: the line had more than the cap, or `line_text` is cut |

### Long lines

One line cannot make an answer, or the server's memory, unbounded.
ripgrep reports a matched line whole, plus about 50 bytes for every
submatch on it, so a 100 MB line on which the pattern matches every
character would otherwise cost about 5 GB to read. rx reads ripgrep's
output without holding it and keeps, per line:

- at most `RX_MAX_LINE_TEXT_BYTES` bytes of its text (1 MiB by
  default). A longer line is cut at the start of the character that
  holds that byte and marked `line_text_truncated: true`, in
  `matches` and in `context_lines` alike;
- at most `RX_MAX_SUBMATCHES_PER_LINE` submatches (10,000 by default),
  and only those that start inside the text kept. A submatch that runs
  past the cut keeps its true `start` and `end` and the part of its text
  that `line_text` holds. `submatches_truncated: true` says the list may
  be incomplete; it is always `true` on a cut line.

`offset`, both line numbers and every other match are exact whatever
the line's length. A cut line is reported under every pattern of the
request, since the part left out may match any of them. A scan whose
answer cuts a line is not written to the trace cache. To read the whole
line, ask [`GET /v1/samples`](samples.md) for its offset.

### Lines that are not valid UTF-8

A log line may hold bytes that are not valid UTF-8: a stray Latin-1
byte, a character cut short. rx searches the line's bytes and reports
its text as JSON can carry it: each byte that is not part of a valid
UTF-8 character becomes U+FFFD (`\ufffd`), one per byte, and every
valid character is kept. `matches`, `context_lines`, a trace-cache hit
and [`GET /v1/samples`](samples.md) give the line the same text.

`offset` and the submatch `start` and `end` count the line's bytes in
the file, as ripgrep counts them, not the bytes of `line_text` as JSON
carries it: each U+FFFD stands for one byte of the file and takes three
in UTF-8. On a line that holds no such byte the two are the same. On
the line `\xff\xfe ERR bad`, searched for `ERR`, the submatch is
`{"text": "ERR", "start": 3, "end": 6}`, and `line_text` is
`"\ufffd\ufffd ERR bad"`. Read a submatch's text from its `text`
field rather than by slicing `line_text`.

A cut line ends at the start of a character: a valid character is
never split, and each U+FFFD stands for one byte.

Every chunk counts the newlines it reads, so a scan that runs to the
end numbers every match. A scan cut short by `max_results` can stop a
chunk part-way; a match in a later chunk then has an offset but no
file line number yet. rx numbers it from the file's line index when
one exists; without one it reports `absolute_line_number: -1`, and
`relative_line_number` is then a chunk-relative number, not a file line
number. Resolve such offsets with `GET /v1/samples?offsets=…`, which
answers a batch in one pass. Captured from a capped search of a 465 MB
log with no index:

```json
{"offset": 365779981, "absolute_line_number": -1, "relative_line_number": 70}
```

The same request can come back with such a line numbered once the file
has a line index, or when the trace cache answers it. That is the only
way an index or the cache changes an answer: every other field is
equal, apart from those that describe how the answer was produced
(`request_id`, `time`, `cli_command` and `file_chunks`), `-1` means
"not computed", and a number rx fills in is always the line that holds
the match's offset.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Search completed (zero or more matches) |
| `400 Bad Request` | `max_results` missing when `hook_on_match` is set; invalid hook URL; bad regex |
| `403 Forbidden` | Path outside `--search-root` |
| `404 Not Found` | A requested path doesn't exist |
| `422 Unprocessable Entity` | Missing required `path` or `regexp` param; a parameter of the wrong type; a context count above 100 or below its minimum |
| `500 Internal Server Error` | Engine failure; logged with stack |
| `503 Service Unavailable` | `ripgrep` not available |

## Examples

### Basic scan

```bash
curl -s 'http://127.0.0.1:7777/v1/trace?path=/var/log/nginx/access.log&regexp=5%5B0-9%5D%7B2%7D+%5B0-9%5D%2B%24' \
    | jq '.matches | length'
```

### Multi-pattern, capped at 100 matches

```bash
curl -sG 'http://127.0.0.1:7777/v1/trace' \
    --data-urlencode 'path=/var/log/app-2026-03.log' \
    --data-urlencode 'regexp=error' \
    --data-urlencode 'regexp=panic' \
    --data-urlencode 'regexp=timeout.*ms' \
    --data-urlencode 'max_results=100' \
    | jq '.matches[] | {pattern: .pattern, line: .absolute_line_number}'
```

### Multi-path

```bash
curl -sG 'http://127.0.0.1:7777/v1/trace' \
    --data-urlencode 'path=/var/log/app-2026-03.log' \
    --data-urlencode 'path=/var/log/nginx/access.log' \
    --data-urlencode 'regexp=500|502|504' \
    | jq '.scanned_files'
```

### With webhook

```bash
curl -sG 'http://127.0.0.1:7777/v1/trace' \
    --data-urlencode 'path=/var/log/app-2026-03.log' \
    --data-urlencode 'regexp=ERROR' \
    --data-urlencode 'max_results=50' \
    --data-urlencode 'hook_on_match=https://example.com/rx-alerts' \
    | jq '.request_id'
```

Each match of the response also calls `https://example.com/rx-alerts`
with a `GET` whose query parameters carry the match (`event=match_found`,
`file_path`, `pattern`, `offset`, and `line_number`, which is the
match's `absolute_line_number`, `-1` included) and the `request_id` of
this response. See [webhooks](../webhooks.md).

### Sandbox rejection

```bash
curl -sG 'http://127.0.0.1:7777/v1/trace' \
    --data-urlencode 'path=/etc/passwd' \
    --data-urlencode 'regexp=root' \
    | jq '.'
```

Response (`403`):

```json
{
  "detail": "path_outside_search_root",
  "error":  "path_outside_search_root",
  "message": "path \"/etc/passwd\" is not within any configured --search-root",
  "path":   "/etc/passwd",
  "roots":  ["/var/log"]
}
```

## Error examples

### Missing `ripgrep`

```json
{ "detail": "ripgrep is not available on this system" }
```

Status: `503`. Install `ripgrep` on the host and restart.

### Invalid webhook URL

```json
{
  "detail": "invalid hook URL: http://10.0.0.5/webhook (points at a private-network address — set RX_ALLOW_INTERNAL_HOOKS=true to allow)"
}
```

Status: `400`. The target is in RFC 1918 space and SSRF protection is
active.

### Missing `max_results` with `hook_on_match`

```json
{
  "detail": "max_results is required when hook_on_match is configured. This prevents accidentally triggering millions of HTTP calls."
}
```

Status: `400`. Add `&max_results=N` to the request.

## Performance notes

- A plain file of 40 MB or more (twice `RX_MIN_CHUNK_SIZE_MB`) is
  split into chunks scanned in parallel; a seekable zstd file is
  scanned frame-parallel
- The trace cache is consulted first for a plain file of
  `RX_LARGE_FILE_MB` or more, and for a seekable zstd file — a hit
  returns in milliseconds regardless of file size
- The body's `request_id` is the one webhooks carry. The
  `X-Request-ID` response header is a separate ID: the one the client
  sent in that header, or one the server generated

## See also

- [`rx trace`](../../cli/trace.md) — CLI equivalent
- [Webhooks](../webhooks.md) — payload shapes
- [concepts/chunking](../../concepts/chunking.md) — parallel algorithm
- [concepts/caching](../../concepts/caching.md) — trace cache behavior
