# HTTP API

`rx serve` exposes the full feature surface over HTTP. Every CLI command
has a corresponding REST endpoint that accepts JSON or query parameters
and returns JSON.

## Base URL

Default bind:

```text
http://127.0.0.1:7777
```

Customize with `--host` and `--port`. See [`rx serve`](../cli/serve.md).

## Endpoint summary

| Method | Path | Purpose | Docs |
|:-:|---|---|---|
| `GET` | `/health` | Service status + system info | [health](endpoints/health.md) |
| `GET` | `/v1/trace` | Regex search | [trace](endpoints/trace.md) |
| `GET` | `/v1/samples` | Content retrieval by offset/line | [samples](endpoints/samples.md) |
| `GET` | `/v1/time-range` | A file's timestamp format and first and last timestamp | [time-range](endpoints/time-range.md) |
| `GET` | `/v1/index` | Read cached line index | [line-index](endpoints/line-index.md) |
| `POST` | `/v1/index` | Build the index of a file, or of every file of a directory (background task) | [line-index](endpoints/line-index.md) |
| `POST` | `/v1/compress` | Compress to seekable zstd (background task) | [compress](endpoints/compress.md) |
| `GET` | `/v1/tasks/{task_id}` | Poll a background task | [tasks](endpoints/tasks.md) |
| `DELETE` | `/v1/tasks/{task_id}` | Cancel a findings scan or a task over a directory | [tasks](endpoints/tasks.md#delete-v1taskstask_id) |
| `GET` | `/v1/tree` | Browse directories within search roots | [tree](endpoints/tree.md) |
| `GET` | `/v1/logs/chains` | The log chains (rotated logs) of a directory | [logs-chains](endpoints/logs-chains.md) |
| `GET` | `/v1/logs/chain` | One log chain described: parts in time order, checks, state, fingerprint | [logs-chain](endpoints/logs-chain.md) |
| `POST` | `/v1/logs/index` | Index every part of one log chain (background task) | [logs-index](endpoints/logs-index.md) |
| `GET` | `/v1/logs/samples` | Lines of a log chain by global line, by part and line, or by time | [logs-samples](endpoints/logs-samples.md) |
| `GET` | `/v1/logs/trace` | Search log chains, directories and files: each chain's parts in its order, each match with its line in the chain | [logs-trace](endpoints/logs-trace.md) |
| `GET` | `/v1/signatures` | The signature catalogue the server loaded | [signatures](endpoints/signatures.md) |
| `GET` | `/v1/findings` | A file's findings from the findings cache: filtered, paged, with text and timeline ticks | [findings](endpoints/findings.md) |
| `POST` | `/v1/findings` | Scan a file, or every file of a directory, for signatures (background task) | [findings](endpoints/findings.md#post-v1findings) |
| `GET` | `/metrics` | Prometheus exposition | [metrics](endpoints/metrics.md) |

In addition, these routes serve the auto-generated docs and the SPA:

| Path | Content |
|---|---|
| `/docs` | Swagger UI, served from the binary (it works offline) |
| `/redoc` | ReDoc UI, served from the binary (it works offline) |
| `/openapi.json` | OpenAPI 3.1 spec (JSON) |
| `/openapi.yaml` | OpenAPI 3.1 spec (YAML) |
| `/openapi-3.0.json`, `/openapi-3.0.yaml` | The same spec as OpenAPI 3.0, for tools that do not read 3.1 |
| `/schemas/{schema}` | One schema of the spec as JSON Schema, such as `/schemas/ApiError.json` |
| `/` | `rx-viewer` SPA (or 307 → `/docs` if unavailable) |
| `/assets/*` | Static SPA assets |
| `/favicon.ico` | Embedded favicon (SVG) |
| Any other path | A file of the viewer bundle (such as `/favicon.svg`), or the SPA's `index.html` so the viewer's router handles the URL; 307 → `/docs` without a viewer. An unknown path under `/v1/`, `/health`, `/metrics`, `/docs`, `/redoc`, `/openapi` or `/schemas/` answers 404 instead: under `/v1/` as JSON (`{"detail": "Not found: /v1/nope"}`), and a method the path does not serve as a JSON 405 with an `Allow` header |

## Request & response conventions

Every API endpoint follows a small set of common conventions — request
IDs, error envelope, HTTP status codes, content types. See
[conventions](conventions.md) for the full spec.

Key facts:

- JSON bodies use `application/json`
- Error bodies have shape `{"detail": "message"}` (path-sandbox errors
  have an extended shape — see [conventions](conventions.md))
- Every response includes an `X-Request-ID` header: the client's own,
  or a generated UUID v7. A trace body's `request_id` is a separate ID
- Every field a response schema marks required is present, as `null`
  when it is unset (not an omitted key); the few optional fields, such
  as a trace answer's `signatures`, `findings` and `finding_summary`,
  are left out when the request does not ask for them

## Authentication

One opt-in shared token, and nothing more: no users, no roles, no TLS.
When `RX_API_TOKEN` is set in the environment of `rx serve`, every path
that is `/v1` or starts with `/v1/` requires the header
`Authorization: Bearer <token>`. The scheme name is case-insensitive,
and the token is compared in constant time. A request without the
header, or with another token, gets `401` with
`WWW-Authenticate: Bearer realm="rx"` and the usual error body:

```json
{
  "detail": "an API token is required: send the header Authorization: Bearer <token>, with the token this server's RX_API_TOKEN holds"
}
```

`/health`, `/metrics`, `/docs`, `/redoc`, `/openapi.json` and the other
spec files, `/schemas/…`, `/favicon.ico` and the viewer's files stay
open without a token. When `RX_API_TOKEN` is unset or empty, no request
is checked. The spec declares the token as the `bearerAuth` scheme,
optional on every `/v1` operation. See
[security](../concepts/security.md#opt-in-api-token).

The token crosses plain HTTP in clear text: anyone who can read the
traffic can read it, and `rx serve` says so when it binds an address
other machines can reach. `rx serve` is designed to sit behind a
reverse proxy that handles TLS, auth, rate limiting, and IP
allowlisting. On an internal network, the `--search-root` sandbox is
the primary defense.

!!! warning "Do not expose `rx serve` to the public internet without a reverse proxy"
    The token is the only built-in check, and it travels in clear
    text. Without it, anyone who can reach the socket can run traces
    against any file within `--search-root`. Use a reverse proxy with
    auth (OAuth, mTLS, basic auth, etc.) for public deployments.

## OpenAPI integration

`rx` generates a complete OpenAPI 3.1 specification at runtime and serves
it at `/openapi.json`. The spec drives the Swagger UI at `/docs` and
the ReDoc page at `/redoc`; both pages and their scripts and styles are
embedded in the binary, so they render on a network with no route to
the internet. It's also suitable for:

- Code generation (`openapi-generator`, `oapi-codegen`, etc.)
- API contract tests
- Ingest into API gateways (Kong, Apigee, etc.)

See [openapi](openapi.md) for details.

## Background tasks

Four endpoints are async — they return a task ID instantly and run the
actual work on a background goroutine:

- `POST /v1/index` — build a line index, of a file or of every file
  under a directory
- `POST /v1/findings` — scan a file, or every file under a directory,
  for signatures
- `POST /v1/compress` — encode a file as seekable zstd
- `POST /v1/logs/index` — build the line index of every part of a log
  chain (`GET /v1/logs/chain` starts the same task for a pending chain)

Poll `GET /v1/tasks/{task_id}` until `status` is `"completed"`,
`"failed"` or `"cancelled"`. An index or findings task carries the
`rx` command that does the same in `cli_command`, and a task over a
directory reports `files_done` of `files_total`. See
[tasks](endpoints/tasks.md).

## Webhooks

`rx` can call external URLs with an HTTP `GET` on three events:

- `on_file` — per file after its scan completes
- `on_match` — per match (requires `max_results`)
- `on_complete` — once per trace request

Configure via env vars (process-wide) or query parameters (per-request).
Webhook URLs are validated against an SSRF allowlist (no loopback,
private, link-local, or CGNAT). See [webhooks](webhooks.md).

## Prometheus metrics

`GET /metrics` emits the standard Prometheus exposition format. Metrics
are only collected when running in `serve` mode — CLI invocations are
zero-overhead.

Common metric families:

- `rx_http_responses_total{method, endpoint, status_code}` — one per
  HTTP response
- `rx_trace_requests_total{status}` and `rx_trace_duration_seconds{path_kind}`
  — trace counts (`success` / `error`) and latency
- `rx_trace_cache_*` and `rx_index_cache_*` — cache hits, misses and
  load times
- `rx_hook_*` — webhook calls by outcome, and their latency
- `go_*` and `process_*` — the Go runtime and the process (GC,
  goroutines, memory, CPU, file descriptors)

See [metrics](endpoints/metrics.md) for the full list.

## Browse the endpoint docs

<div class="grid cards" markdown>

- **[Health](endpoints/health.md)** — readiness + system info  
- **[Trace](endpoints/trace.md)** — regex search  
- **[Samples](endpoints/samples.md)** — content retrieval  
- **[Line index](endpoints/line-index.md)** — build and read indexes  
- **[Compress](endpoints/compress.md)** — seekable zstd encoding  
- **[Tasks](endpoints/tasks.md)** — background task polling  
- **[Tree](endpoints/tree.md)** — file system navigation  
- **[Log chains](endpoints/logs-chains.md)** — rotated logs read as one: [chains](endpoints/logs-chains.md), [chain](endpoints/logs-chain.md), [index](endpoints/logs-index.md), [samples](endpoints/logs-samples.md), [trace](endpoints/logs-trace.md)  
- **[Metrics](endpoints/metrics.md)** — Prometheus exposition  
- **[Webhooks](webhooks.md)** — on_file / on_match / on_complete  
- **[OpenAPI](openapi.md)** — consuming the generated spec

</div>

## See also

- [`rx serve`](../cli/serve.md) — start the server
- [Configuration](../configuration.md) — env vars that affect serve
- [Concepts](../concepts/index.md) — chunking, caching, security
