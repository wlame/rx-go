# API conventions

Shared patterns across every `/v1/*` endpoint — request/response
envelopes, error shapes, status codes, headers, and identifiers.

## Content types

- **Requests** with a body: `application/json`
- **Responses**: `application/json`
- **OpenAPI spec**: `application/openapi+json` at `/openapi.json`,
  `application/openapi+yaml` at `/openapi.yaml`
- **Metrics**: `text/plain; version=0.0.4` (Prometheus exposition format)
  at `/metrics`
- **SPA assets**: sniffed by extension — `.js` is `application/javascript`,
  `.css` is `text/css`, `.svg` is `image/svg+xml`, etc.

## Request IDs

Every request receives a UUID v7 request ID.

- If the client sends `X-Request-ID`, that value is trusted (capped at
  128 chars to prevent log flooding)
- If absent, `rx` generates a new UUID v7 (time-sortable)
- The ID is echoed in the `X-Request-ID` response header
- The ID appears in the log records of the request

A trace answer also has a `request_id` in its body, and that is a
different ID: the `request_id` query parameter, or one generated for the
trace. It is the ID webhooks carry. Sending `X-Request-ID: abc` gets
`X-Request-ID: abc` back, while the body's `request_id` stays a fresh
UUID v7 unless the request also passes `request_id=abc`.

UUID v7 is time-sortable — the first 48 bits are the creation timestamp
in milliseconds, so sorting by request ID gives you chronological order.

## Error envelope

Errors use a flat JSON body with a single `detail` field:

```json
{
  "detail": "File not found: /var/log/does-not-exist.log"
}
```

This matches the error shape most FastAPI-based ecosystems produce.
Consumers can safely assume every error response has this shape, except
the `409` of the log chain routes `/v1/logs/chain`, `/v1/logs/index`
and `/v1/logs/samples`, whose body is the chain's current description
(see the status codes below).

### Extended: path-sandbox errors

`403 path_outside_search_root` has an extended body:

```json
{
  "detail": "path_outside_search_root",
  "error":  "path_outside_search_root",
  "message": "path \"/etc/passwd\" is not within any configured --search-root",
  "path":   "/etc/passwd",
  "roots":  ["/var/log", "/var/data/exports"]
}
```

The `detail` field stays present for backward compatibility; the
extra fields let UIs build a structured "you've hit the sandbox"
message without parsing the string.

## HTTP status codes

| Code | Meaning | When returned |
|-----:|---|---|
| `200 OK` | Success | Every happy-path response on `GET` endpoints and some `POST`s |
| `201 Created` | Not used (huma conventions) | — |
| `202 Accepted` | Task started, or the answer waits for one | `POST /v1/findings` (the scan task, started or joined); `GET /v1/samples` and `GET /v1/logs/samples` with `Prefer: respond-async`, when the index build they wait for outlasts `RX_SAMPLES_WAIT_SECONDS` |
| `400 Bad Request` | Invalid input | Bad regex, malformed offsets/lines, unknown flags, conflicting options; webhook URL that fails SSRF check |
| `401 Unauthorized` | API token missing or wrong | Any `/v1` request without `Authorization: Bearer <token>` when the server sets `RX_API_TOKEN`; the answer carries `WWW-Authenticate: Bearer realm="rx"` (see [authentication](index.md#authentication)) |
| `403 Forbidden` | Path sandbox violation or permission denied | Any path outside `--search-root` |
| `404 Not Found` | Path, task, or cache entry missing | File doesn't exist; task ID unknown; `GET /v1/index` with no cache |
| `409 Conflict` | Conflict with a running task, or a log chain's files changed | `POST /v1/index` or `/v1/compress` while one is already running for the same path (a compress task holds its input and its output); `POST /v1/findings` while the scan of the same directory is `cancelling`; `DELETE /v1/tasks/{task_id}` on a task that cannot be cancelled; `GET /v1/logs/chain`, `POST /v1/logs/index` and `GET /v1/logs/samples` when the chain's files changed (the fingerprint differs, or a part was renamed or replaced during the read); `GET /v1/logs/trace` when a part was renamed or replaced during the read |
| `422 Unprocessable Entity` | Schema validation failure | Missing required query param, wrong type |
| `500 Internal Server Error` | Unhandled error or panic | Always logged with stack trace |
| `503 Service Unavailable` | Required dependency missing, or no room for a task | `ripgrep` not on `PATH` (`/v1/trace`, `/v1/logs/trace`); 256 signature scans queued or running (`POST /v1/findings`); 128 log chain index tasks running or waiting (`POST /v1/logs/index`, and `GET /v1/logs/samples` when the request needs the chain ready) |

The [OpenAPI document](openapi.md) declares, on every operation, each of
these statuses that the operation can answer, with its body schema:
`ApiError` (the `detail` envelope) for all of them, except `403`, which
is `oneOf` `SandboxError` (the extended body above) and `ApiError` (a
hidden or unreadable path), and `409`. On `POST /v1/compress`,
`POST /v1/index`, `POST /v1/findings` and `DELETE /v1/tasks/{task_id}`,
a `409` is `TaskConflictError`: the `detail` envelope plus `task_id`,
the ID of the task the conflict is about. On `GET /v1/logs/chain`,
`POST /v1/logs/index` and `GET /v1/logs/samples` it is `ChainResponse`,
the chain's current description; on `GET /v1/logs/trace` it is
`ApiError`. Each operation's `default` response, also `ApiError`,
covers anything rarer, such as `413` for an oversized body, `415` for a
body sent without `Content-Type: application/json`, or a `500` from a
recovered panic.

## Query parameter conventions

- **Repeated params**: `?path=a&path=b` — many endpoints accept multiple
  values this way (any parameter typed as a list)
- **Comma-separated**: for `offsets` and `lines` on `/v1/samples`, values
  are in a single string: `?lines=100,200-300,500`
- **Booleans**: `true` / `false`, `True` / `False`, `TRUE` / `FALSE`,
  `t` / `f`, `T` / `F`, `1` / `0`; anything else, such as `yes` or
  `on`, is a `422`
- **Integers**: decimal, non-negative unless documented otherwise
- **Sentinel `-1`**: on `/v1/samples`, `context`, `before_context`, and
  `after_context` accept `-1` to mean "not provided, use default"; on
  `/v1/trace`, `before_context` and `after_context` accept `-1` to mean
  "not provided, take `context`"

## Path validation

Every endpoint that accepts a path:

1. Resolves the path to absolute form (relative paths from the server's
   CWD)
2. Follows symlinks (`filepath.EvalSymlinks`)
3. Checks the canonical form against the configured `--search-root`
   list
4. Rejects with `403` if outside all roots
5. Returns `404` if the path doesn't exist

See [concepts/security](../concepts/security.md) for the threat model.

## Background task pattern

Four endpoints are asynchronous:

- `POST /v1/index`
- `POST /v1/compress`
- `POST /v1/findings` (answers `202`)
- `POST /v1/logs/index`

Each returns immediately with a task ID:

```json
{
  "task_id": "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":  "queued",
  "message": "Indexing task started for /var/log/app.log",
  "path":    "/var/log/app.log",
  "started_at": "2026-04-18T14:23:45.123456Z",
  "cli_command": "rx index /var/log/app.log"
}
```

Poll `GET /v1/tasks/{task_id}`:

```json
{
  "task_id":   "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":    "running",
  "path":      "/var/log/app.log",
  "operation": "index",
  "started_at": "2026-04-18T14:23:45.123456Z",
  "completed_at": null,
  "error":     null,
  "progress":  0.42,
  "bytes_read":  null,
  "bytes_total": null,
  "files_done":  null,
  "files_total": null,
  "cli_command": "rx index /var/log/app.log",
  "result":    null
}
```

Possible statuses: `queued` → `running` → (`completed` | `failed`).
A findings scan, or a task over a directory, that
`DELETE /v1/tasks/{task_id}` stops ends `cancelled`; a task over a
directory is `cancelling` until its worker stops.

On completion, `result` is populated with the operation's full output
(the same shape as the corresponding synchronous command's JSON).

See [api/endpoints/tasks](endpoints/tasks.md).

## Time format

Timestamp strings use ISO 8601 with microsecond precision: the times
of a task, and `modified_at` of a tree entry or a log chain part:

```text
2026-04-18T14:23:45.123456Z
```

The one exception is `created_at` of a line index (`GET /v1/index` and
a finished index task), which is RFC 3339 with up to nine fractional
digits and trailing zeros dropped, such as
`2026-04-18T14:23:45.123456789Z`.

Times are always UTC (trailing `Z`). The microsecond subsecond format
matches most JSON-dump conventions; clients can parse with
`time.Parse(time.RFC3339Nano, ...)` in Go or `datetime.fromisoformat`
in Python 3.11+.

## The equivalent CLI command

The answers of `/v1/trace`, `/v1/samples`, `/v1/time-range`,
`GET /v1/index`, `GET /v1/findings`, `/v1/logs/chain`,
`/v1/logs/samples` and `/v1/logs/trace`, the result of a finished index
or compress task, and every answer about a task of `POST /v1/index`
or `POST /v1/findings` (the `202` or `200` that starts it and
`GET /v1/tasks/{task_id}`) carry `cli_command`: the `rx` command that
does what the request did, ready to paste into a shell. A task of
`POST /v1/index` is `rx index PATH`, one of `POST /v1/findings` is
`rx scan PATH --signatures=SEL`, and a task over a directory adds
`--recursive` (`rx scan /var/log/app --recursive --signatures=default`).
A server that serves hidden entries (`rx serve --hidden`) adds
`--hidden` to a task's command, so the command lists the files the task
listed.

- The shape is `rx <subcommand> <positionals...> <--long=value...>`.
  A value that needs shell quoting is quoted after the `=`; a switch
  that is on is written bare (`--force`), and one turned off from a
  default of on is written `--name=false`.
- A flag appears when the request gave a value and that value is not
  what `rx` uses without the flag. A field the request left out never
  appears, because every HTTP default is the CLI default; a value equal
  to the default is left out as well. For example, a compress request
  with `"compression_level": 3` renders no `--level`, and a samples
  request with `context=2&before_context=0` renders
  `--context=2 --before=0`.
- Paths are the validated, absolute paths the server used.

A test parses every rendered command with the real `rx` command tree,
and runs the commands of real trace and samples requests to check that
their `--json` answer equals the HTTP answer, and those of index and
findings tasks, on a fresh cache, to check that they index or scan the
same files and find the same findings.

## Explicit null vs omitted keys

Every field the response schema marks required is always present —
either with its typed value or with `null`. Unset or not-applicable
fields are emitted as `null`, not omitted.

```json
{
  "compression_format": null,   // explicit null, always present
  "cli_command":        null
}
```

This keeps parsing code simpler — consumers can always access
`response.compression_format` without an existence check.

A few fields the schema marks optional are left out when they do not
apply. For example, `signatures`, `findings` and `finding_summary` of a
trace answer appear only when the request names `signatures`. The Go-specific
`index_path`, where an index is stored in the cache, is part of a
finished index task's `result` and always present there;
`GET /v1/index` has no such field.

## Stable field ordering

Go's `encoding/json` emits map keys in alphabetical order, not
insertion order. If you're consuming responses that include
maps (e.g. `SamplesResponse.samples` keyed by offset string),
iterate with a sort or by the known request order — don't assume
insertion order.

## Pagination

`GET /v1/findings` is the one paged endpoint: `offset` (0 or more)
passes over that many filtered findings, `limit` (1 to 10000, default
1000) caps the page, and `total` counts the findings the page is cut
from. Elsewhere `max_results` on `/v1/trace` and `/v1/logs/trace` is
the result-bounding mechanism.

## Rate limiting

Not built in. Use a reverse proxy for rate limiting.

## CORS

Not built in. All requests share a single origin. Combine with a
reverse proxy or add CORS middleware if cross-origin access is needed.

## Compression

Not built in on the response side. HTTP clients that request
compression via `Accept-Encoding` receive plain responses. Add `gzip`
compression at a reverse-proxy layer if response sizes matter.

## See also

- [API overview](index.md)
- [OpenAPI integration](openapi.md)
- [Webhooks](webhooks.md)
- [Health endpoint](endpoints/health.md)
