# `GET /v1/trace`

Search one or more files for regex patterns. HTTP equivalent of [`rx
trace`](../../cli/trace.md).

## Purpose

Execute a regex scan across one or more paths with parallel chunking,
returning match byte offsets, line numbers, and optional context
lines. Supports webhook callbacks for progress notifications.

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

Repeat `path` and `regexp` to supply multiple values:

```text
GET /v1/trace?path=/var/log/a.log&path=/var/log/b.log&regexp=error&regexp=panic
```

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
      ]
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
  "cli_command":     "rx trace --path=/var/log/app-2026-03.log --regexp=timeout"
}
```

### Response fields

| Field | Type | Description |
|---|---|---|
| `request_id` | string | UUID v7, echoed from request or auto-generated |
| `path` | `string[]` | The requested paths (validated & absolute) |
| `time` | number | Wall-clock elapsed seconds for the scan |
| `patterns` | `{id: pattern}` | Pattern ID → original string map |
| `files` | `{id: path}` | File ID → absolute path map |
| `matches` | array | See below |
| `scanned_files` | `string[]` | Files actually scanned (vs skipped) |
| `skipped_files` | `string[]` | Files skipped (binary, size limit, etc.) |
| `max_results` | `int \| null` | The cap that was applied, or null |
| `file_chunks` | `{fileId: N}` | How many chunks each file was split into |
| `context_lines` | `{matchKey: [...]}` | Context lines when `--samples` mode was used |
| `before_context`, `after_context` | `int \| null` | Requested context size |
| `cli_command` | string | Equivalent CLI command |

### `matches[]` shape

| Field | Type | Description |
|---|---|---|
| `pattern` | string | Pattern ID (key into `patterns`) |
| `file` | string | File ID (key into `files`) |
| `offset` | int64 | Byte offset of the line start |
| `relative_line_number` | int | Line number within the chunk |
| `absolute_line_number` | int | File-absolute line number |
| `line_text` | string | Full matched line |
| `submatches` | array | `{text, start, end}` per regex submatch |

When the file was scanned in a single chunk, `relative_line_number ==
absolute_line_number`. In multi-chunk scans, the absolute number is
computed from the line index (if available) or by counting.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Search completed (zero or more matches) |
| `400 Bad Request` | `max_results` missing when `hook_on_match` is set; invalid hook URL; bad regex |
| `403 Forbidden` | Path outside `--search-root` |
| `404 Not Found` | A requested path doesn't exist |
| `422 Unprocessable Entity` | Missing required `path` or `regexp` param |
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

Each match also fires a POST to `https://example.com/rx-alerts` with
the match details. See [webhooks](../webhooks.md).

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

- For files above 20 MB (configurable via `RX_MIN_CHUNK_SIZE_MB`),
  the engine parallelizes across goroutines
- The trace cache is consulted first — a warm cache returns in
  milliseconds regardless of file size
- `request_id` is returned in both the body and the `X-Request-ID`
  header; use it to correlate server logs with your client

## See also

- [`rx trace`](../../cli/trace.md) — CLI equivalent
- [Webhooks](../webhooks.md) — payload shapes
- [concepts/chunking](../../concepts/chunking.md) — parallel algorithm
- [concepts/caching](../../concepts/caching.md) — trace cache behavior
