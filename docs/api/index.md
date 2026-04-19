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
| `GET` | `/v1/index` | Read cached line index | [line-index](endpoints/line-index.md) |
| `POST` | `/v1/index` | Build a new index (background task) | [line-index](endpoints/line-index.md) |
| `POST` | `/v1/compress` | Compress to seekable zstd (background task) | [compress](endpoints/compress.md) |
| `GET` | `/v1/tasks/{task_id}` | Poll a background task | [tasks](endpoints/tasks.md) |
| `GET` | `/v1/tree` | Browse directories within search roots | [tree](endpoints/tree.md) |
| `GET` | `/v1/detectors` | List registered anomaly detectors | [detectors](endpoints/detectors.md) |
| `GET` | `/metrics` | Prometheus exposition | [metrics](endpoints/metrics.md) |

In addition, these routes serve the auto-generated docs and the SPA:

| Path | Content |
|---|---|
| `/docs` | Swagger UI |
| `/redoc` | ReDoc UI |
| `/openapi.json` | OpenAPI 3.1 spec (JSON) |
| `/openapi.yaml` | OpenAPI 3.1 spec (YAML) |
| `/` | `rx-viewer` SPA (or 307 → `/docs` if unavailable) |
| `/assets/*` | Static SPA assets |
| `/favicon.ico`, `/favicon.svg` | Embedded favicon |

## Request & response conventions

Every API endpoint follows a small set of common conventions — request
IDs, error envelope, HTTP status codes, content types. See
[conventions](conventions.md) for the full spec.

Key facts:

- JSON bodies use `application/json; charset=utf-8`
- Error bodies have shape `{"detail": "message"}` (path-sandbox errors
  have an extended shape — see [conventions](conventions.md))
- Every response includes an `X-Request-ID` header (UUID v7)
- Every endpoint returns explicit `null` for unset schema fields (not
  omitted keys)

## Authentication

None built in. `rx serve` is designed to sit behind a reverse proxy
that handles TLS, auth, rate limiting, and IP allowlisting. On an
internal network, the `--search-root` sandbox is the primary defense.

!!! warning "Do not expose `rx serve` to the public internet without a reverse proxy"
    There's no built-in auth. Anyone who can reach the socket can run
    traces against any file within `--search-root`. Use a reverse
    proxy with auth (OAuth, mTLS, basic auth, etc.) for public
    deployments.

## OpenAPI integration

`rx` generates a complete OpenAPI 3.1 specification at runtime and serves
it at `/openapi.json`. The spec drives the Swagger UI at `/docs` and
the ReDoc page at `/redoc`. It's also suitable for:

- Code generation (`openapi-generator`, `oapi-codegen`, etc.)
- API contract tests
- Ingest into API gateways (Kong, Apigee, etc.)

See [openapi](openapi.md) for details.

## Background tasks

Two endpoints are async — they return a task ID instantly and run the
actual work on a background goroutine:

- `POST /v1/index` — build a line index
- `POST /v1/compress` — encode a file as seekable zstd

Poll `GET /v1/tasks/{task_id}` until `status` is `"completed"` or
`"failed"`. See [tasks](endpoints/tasks.md).

## Webhooks

`rx` can fire HTTP POSTs to external URLs on three events:

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

- `rx_http_responses_total{method, endpoint, status}` — request counts
- `rx_http_request_duration_seconds` — request latency histogram
- `rx_trace_*` — per-trace counters (matches, files, time)
- `rx_hook_*` — webhook dispatch latency, failure counters
- Standard Go runtime metrics (GC, goroutines, memory)

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
- **[Detectors](endpoints/detectors.md)** — anomaly detector metadata  
- **[Metrics](endpoints/metrics.md)** — Prometheus exposition  
- **[Webhooks](webhooks.md)** — on_file / on_match / on_complete  
- **[OpenAPI](openapi.md)** — consuming the generated spec

</div>

## See also

- [`rx serve`](../cli/serve.md) — start the server
- [Configuration](../configuration.md) — env vars that affect serve
- [Concepts](../concepts/index.md) — chunking, caching, security
