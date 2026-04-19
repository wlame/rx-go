# `rx serve`

Start the HTTP API server — exposes the full `rx` feature set over HTTP,
serves the `rx-viewer` SPA, publishes OpenAPI 3.1, and exports Prometheus
metrics.

## Synopsis

```text
rx serve [flags]
```

## Description

`rx serve` binds to a TCP port and runs an HTTP server with:

- All `/v1/*` endpoints (trace, samples, index, compress, tree, tasks, detectors, info)
- `/health` — readiness probe with system info
- `/metrics` — Prometheus exposition format
- `/docs` — Swagger UI generated from OpenAPI 3.1
- `/redoc` — ReDoc alternative
- `/openapi.json` and `/openapi.yaml` — raw OpenAPI spec
- `/` and `/assets/*` — static file serving for the `rx-viewer` SPA

The server accepts `SIGINT` and `SIGTERM` for graceful shutdown with a
10-second drain window for in-flight requests.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--host` | `string` | `127.0.0.1` | Interface to bind |
| `--port` | `int` | `7777` | TCP port to bind |
| `--search-root` | `string[]` | current directory | Restrict file access to this directory (repeatable) |
| `--skip-frontend` | `bool` | `false` | Don't attempt to download the `rx-viewer` SPA |

### Default behavior

- **`--host=127.0.0.1`** — binds to loopback only. Use `0.0.0.0` to accept
  external connections (combine with a reverse proxy for TLS).
- **`--port=7777`** — arbitrary default. Any non-privileged port works.
- **`--search-root`** — if omitted, the current working directory becomes
  the only allowed root. Every file-accepting endpoint rejects paths
  outside configured roots with `403 path_outside_search_root`.

## Examples

### Minimal local server

```bash
rx serve
```

Binds `127.0.0.1:7777` with the current directory as the only search
root. Attempts to download the `rx-viewer` SPA from GitHub on first
start (stored under `~/.cache/rx/frontend/`).

```text
Starting RX API server on http://127.0.0.1:7777
Search root: /home/you/projects/example
API docs available at http://127.0.0.1:7777/docs
Metrics available at http://127.0.0.1:7777/metrics
```

### Production-style bind

```bash
rx serve --host=0.0.0.0 --port=8080 \
    --search-root=/var/log \
    --search-root=/var/data/exports
```

Binds all interfaces on port 8080 with two search roots. Any request
referencing a path outside `/var/log` or `/var/data/exports` returns
`403`. For public-facing deployments, front this with Nginx, Caddy, or
an ingress controller for TLS termination and auth.

### Air-gapped / offline

```bash
rx serve --skip-frontend
```

Skips the SPA fetch. The `/` route redirects to `/docs` (Swagger UI)
when no SPA is cached. Use when:

- The host is offline or firewalled from GitHub
- You've pre-populated `~/.cache/rx/frontend/` via another mechanism
- You only need the API, not the web UI

### Pinned SPA version

```bash
RX_FRONTEND_VERSION=v0.2.0 rx serve
```

Pins to a specific `rx-viewer` release. Without this, `rx` fetches
`latest` on first run. Once cached, the SPA is reused until the
version env var changes.

### Custom SPA cache location

```bash
RX_FRONTEND_PATH=/opt/rx-frontend rx serve
```

The SPA is extracted to `/opt/rx-frontend/` instead of the default
`~/.cache/rx/frontend/`. Useful when serving from a read-only home
directory or when sharing the SPA across multiple instances.

### Webhook-emitting server

```bash
export RX_HOOK_ON_MATCH_URL=https://example.com/rx/matches
export RX_HOOK_ON_COMPLETE_URL=https://example.com/rx/complete
rx serve --search-root=/var/log
```

Every `/v1/trace` request now also fires webhooks to the configured
URLs. Per-request overrides via query parameters are allowed unless
`RX_DISABLE_CUSTOM_HOOKS=1` is set. See [api/webhooks](../api/webhooks.md).

## How it works

### Startup sequence

1. Resolve `RX_LOG_LEVEL` and configure the slog level
2. Validate every `--search-root`; abort with exit code 2 on failure
3. Export `RX_SEARCH_ROOTS` to the process environment (propagates to
   any child processes like `ripgrep` — though currently unused)
4. Look up `ripgrep` on `PATH`; remember the result for `/health`
5. Best-effort fetch of the `rx-viewer` SPA (60-second timeout, failure
   is non-fatal)
6. Register HTTP middleware (request ID, structured logging, panic
   recovery, Prometheus metrics middleware)
7. Register every endpoint and the static-file catch-all
8. Bind the listening socket and begin serving

If the socket bind fails (port in use, permission denied), the server
returns immediately with a non-zero exit code.

### Request flow

Each request is tagged with a UUID v7 request ID (generated if absent,
capped at 128 chars if client-supplied via `X-Request-ID`). The ID is:

- Added as an `X-Request-ID` response header
- Included in every `slog` log record for this request
- Propagated to webhook payloads fired as a side-effect of the request

The path-sandbox check runs before any filesystem access. If the path
escapes all configured roots, the response is `403` with a structured
body:

```json
{
  "detail": "path_outside_search_root",
  "error":  "path_outside_search_root",
  "message": "path \"/etc/passwd\" is not within any configured --search-root",
  "path":   "/etc/passwd",
  "roots":  ["/var/log", "/var/data/exports"]
}
```

### Background tasks

`POST /v1/index` and `POST /v1/compress` return a task ID immediately
(status `queued`) and run the work in a detached goroutine. Poll
`GET /v1/tasks/{id}` until `status == "completed"` or `status ==
"failed"`. See [api/endpoints/tasks](../api/endpoints/tasks.md).

One task per `(path, operation)` pair at a time. Duplicate POSTs
return the existing task's ID with `409 Conflict`.

Finished tasks are swept from memory every 5 minutes if older than
`RX_TASK_TTL_MINUTES` (default 60).

### Graceful shutdown

On `SIGINT` or `SIGTERM`:

1. The server stops accepting new connections
2. In-flight requests get up to 10 seconds to finish
3. The task manager stops its sweeper
4. The webhook dispatcher drains its queue
5. The process exits 0

Requests not completed within 10 seconds are terminated. Set a longer
timeout by modifying the shutdown constant in source (not currently
exposed as a flag).

### Performance characteristics

- Binds one listening socket, one goroutine per connection (net/http
  default). No connection-count limit — set one via a reverse proxy
  if needed.
- Prometheus metrics are updated via atomic counters; overhead per
  request is tens of nanoseconds.
- Background task concurrency is unbounded — N simultaneous
  `POST /v1/index` calls launch N goroutines. For controlled
  parallelism, enqueue with a worker on the client side.

## Tips and gotchas

!!! warning "Default bind is loopback-only"
    `--host=127.0.0.1` is the default. If you expect external clients
    to connect and `curl localhost:7777` works but `curl <host>:7777`
    doesn't, you've hit this. Set `--host=0.0.0.0` (and put a reverse
    proxy in front if the host is internet-facing).

!!! tip "Use multiple `--search-root` flags"
    Don't collapse multiple search roots into a common ancestor unless
    you mean it. `--search-root=/var --search-root=/srv` is safer than
    `--search-root=/`.

!!! warning "Search roots follow symlinks once"
    When you pass `--search-root=/path`, `rx` dereferences the symlink
    at startup to get a canonical path. Later path checks compare
    against the canonical form. This means a symlink swap after startup
    (`ln -sfn newtarget /path`) doesn't change the sandbox — restart
    the server to pick it up.

!!! warning "Frontend fetch failure is non-fatal"
    If the first-run SPA download fails (offline, firewall, rate limit),
    `rx serve` prints a warning and continues. `/` returns a redirect
    to `/docs`; API clients are unaffected. `/health` still reports
    normally.

!!! note "`/metrics` includes endpoint labels, not path values"
    The Prometheus metrics label requests by **route pattern**
    (`/v1/tasks/{task_id}`), not by path value (`/v1/tasks/abc-123`).
    This prevents cardinality explosion from variable path segments.

## See also

- [api/index](../api/index.md) — HTTP API overview
- [api/conventions](../api/conventions.md) — request/response envelopes, status codes
- [api/webhooks](../api/webhooks.md) — webhook config and payloads
- [concepts/security](../concepts/security.md) — sandbox, SSRF, tarball defenses
- [Configuration](../configuration.md) — every env var that affects serve
