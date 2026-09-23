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
| `--skip-frontend` | `bool` | `false` | Don't attempt to download or check the `rx-viewer` SPA; serve what is cached |
| `--update-viewer` | `bool` | `false` | Check GitHub for a newer `rx-viewer` release inside the supported range now, instead of once a day. Exits 2 with `--skip-frontend`, and when `RX_FRONTEND_PATH` is set |

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
root. Downloads the `rx-viewer` SPA from GitHub on first start (stored
under `~/.cache/rx/frontend/`) and checks for a newer release once a day
after that (see [Viewer updates](#viewer-updates)).

```text
Starting RX API server on http://127.0.0.1:7777
Search root: /home/you/projects/example
Viewer: v0.6.0 (cached)
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

Pins to a specific `rx-viewer` release, with no range check and no daily
check. Once cached, the SPA is reused until the version env var changes.
Without it, `rx` serves the newest release inside the range it supports
(see [Viewer updates](#viewer-updates)).

### Check for a newer viewer now

```bash
rx serve --update-viewer
```

Asks GitHub for a newer `rx-viewer` release inside the supported range at
this start, whatever the last check says, and installs it before the
server binds.

### Serve a viewer build you manage

```bash
RX_FRONTEND_PATH=~/src/rx-viewer/dist rx serve
```

Serves the viewer in that directory as it is — a local build of
`rx-viewer`, or a copy you keep on a host without GitHub access. The
directory belongs to you: `rx` asks GitHub nothing for it, downloads
nothing into it, writes no `.metadata.json` in it and does not create it.
There is no range check either. The banner names it:

```text
Viewer: v0.6.0-6-g03013b5 from /home/you/src/rx-viewer/dist (RX_FRONTEND_PATH, served as it is)
```

The version is the one the build's `version.json` stamps (else the one a
copied cache's `.metadata.json` records). A directory without
`index.html` and `assets/`, or one that does not exist, serves no viewer
(`/` redirects to `/docs`) and prints one warning. `RX_FRONTEND_URL` and
`RX_FRONTEND_VERSION` are ignored beside it, with a warning, and
`--update-viewer` exits 2: there is nothing to update.

To move the cache `rx` manages itself, set `RX_CACHE_DIR`; the viewer
then lives in `$RX_CACHE_DIR/rx/frontend/`.

### Webhook-emitting server

```bash
export RX_HOOK_ON_MATCH_URL=https://example.com/rx/matches
export RX_HOOK_ON_COMPLETE_URL=https://example.com/rx/complete
rx serve --search-root=/var/log
```

Every `/v1/trace` request now also fires webhooks to the configured
URLs. A request's `hook_on_*` query parameter replaces the matching
variable for that request alone, unless `RX_DISABLE_CUSTOM_HOOKS=1` is
set. See [api/webhooks](../api/webhooks.md).

## How it works

### Startup sequence

1. Resolve `RX_LOG_LEVEL` and configure the slog level
2. Validate every `--search-root`; abort with exit code 2 on failure
3. Export `RX_SEARCH_ROOTS` to the process environment (propagates to
   any child processes like `ripgrep` — though currently unused)
4. Print a warning to stderr for a bind other machines can reach, and
   for a search root that is `/` or your home directory (see below)
5. Look up `ripgrep` on `PATH`; remember the result for `/health`
6. Best-effort install or check of the `rx-viewer` SPA (see
   [Viewer updates](#viewer-updates); 60-second budget, failure is
   non-fatal), then the banner names the viewer served and why
7. Register HTTP middleware (request ID, structured logging, panic
   recovery, Prometheus metrics middleware)
8. Register every endpoint and the static-file catch-all
9. Bind the listening socket and begin serving

If the socket bind fails (port in use, permission denied), the server
returns immediately with a non-zero exit code.

### Request flow

Each request is tagged with a UUID v7 request ID (generated if absent,
capped at 128 chars if client-supplied via `X-Request-ID`). The ID is:

- Added as an `X-Request-ID` response header
- Included in the `slog` log records for this request

Webhooks carry the trace's own `request_id` (the body field), not this
ID; see [request IDs](../api/conventions.md#request-ids).

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

One task per path at a time, whatever the operation: while an index
task runs for a file, a second index request for it *and* a compress
request for it get `409 Conflict`, and the other way round. A compress
task holds its output path as well as its input, so a second
compression into the same output (`app.log` and `app.log.gz` both
default to `app.log.zst`) and an index request for that output are
refused too. The `409` body names the running task in `task_id`, and
its `detail` sentence names the running task's operation and the path
it holds: a compress refused because an index task runs reads
`Indexing already in progress for … (task: <the index task's ID>)`.

Finished tasks are swept from memory every 5 minutes if older than
`RX_TASK_TTL_MINUTES` (default 60). A finished index task keeps its
whole result, line index included, until then, so the table is capped
at 256 tasks: starting a task past the cap drops the oldest finished
ones, and `GET /v1/tasks/{id}` for a dropped task answers `404`.
Running and queued tasks are never dropped.

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

!!! warning "A wide bind or a wide root is announced at startup"
    `rx serve` has no authentication, so two configurations print a
    warning to stderr before the server comes up:

    - a `--host` other than a loopback address or `localhost`, such as
      `0.0.0.0` or a private address, which other machines can reach —
      the warning suggests `RX_API_TOKEN`, or, when the token is set,
      says that it crosses plain HTTP in clear text;
    - a search root that is `/` or your home directory, which serves
      every file in it except hidden ones.

    Both are allowed — a VPN or an authenticating proxy makes the first
    reasonable — but whoever reads the log sees that the choice was made.

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
    If the SPA download or the daily check fails (offline, firewall,
    rate limit, a release without `dist.tar.gz`), `rx serve` prints one
    warning and continues: with the cached viewer when there is one,
    otherwise with `/` redirecting to `/docs`. API clients are
    unaffected. `/health` still reports normally.

### Viewer updates

With no `RX_FRONTEND_URL` or `RX_FRONTEND_VERSION` set, `rx serve` serves
the newest published `rx-viewer` release inside the range this `rx` was
built against (`0.2.0 <= v < 0.7.0` today). Before the server binds:

| Cache | What `rx serve` does | Banner |
|---|---|---|
| None | Reads GitHub's release list and installs the newest release inside the range | `v0.6.0 (installed)` |
| Inside the range, `last_check` under a day old | Serves it; asks GitHub nothing | `v0.6.0 (cached)` |
| Inside the range, `last_check` a day old, missing or unreadable | Reads the release list; installs a newer release inside the range, or keeps the cache; records `last_check` | `v0.6.0 (updated from v0.2.0)` or `v0.2.0 (cached)` |
| Outside the range | Counts as no cache: installs the newest release inside the range, or serves no viewer | `v0.6.0 (updated from v0.7.1)` or `none (/ redirects to /docs)` |

`--update-viewer` runs the check at once, whatever `last_check` says.
An update that replaces a cache whose `.metadata.json` records no version
says `v0.6.0 (replaced a cached viewer of unrecorded version)`.

The release list is one page of GitHub's
`/repos/wlame/rx-viewer/releases`; drafts, pre-releases and tags that are
not a plain `vX.Y.Z` are skipped. When a cached viewer exists, the list
request has a 10-second limit, so an offline host starts at most that
much later. A failed check keeps the cached viewer, prints one warning
and records `last_check`, so the next try is a day later (or at the next
`--update-viewer`). A newer release past the range is logged as
`frontend_newer_release_outside_range`: upgrading `rx` brings it in.

The check runs before the listener binds, never in the background:
replacing the bundle's files under a running server could hand a browser
an `index.html` whose assets are already gone.

`RX_FRONTEND_URL` downloads its URL on every start, and
`RX_FRONTEND_VERSION` serves its pinned release; neither reads the
release list, and both leave `--update-viewer` without effect. The
banner says `(set by RX_FRONTEND_URL or RX_FRONTEND_VERSION)`.
`--skip-frontend` serves whatever is cached and asks nothing.
`RX_FRONTEND_PATH` serves its directory as it is and wins over all of
them (see [Serve a viewer build you manage](#serve-a-viewer-build-you-manage)).

!!! note "`/metrics` includes endpoint labels, not path values"
    The Prometheus metrics label requests by **route pattern**
    (`/v1/tasks/{task_id}`), not by path value (`/v1/tasks/abc-123`),
    and a request no route matches as `unmatched`. This keeps the number
    of series fixed whatever paths clients try.

## See also

- [api/index](../api/index.md) — HTTP API overview
- [api/conventions](../api/conventions.md) — request/response envelopes, status codes
- [api/webhooks](../api/webhooks.md) — webhook config and payloads
- [concepts/security](../concepts/security.md) — sandbox, SSRF, tarball defenses
- [Configuration](../configuration.md) — every env var that affects serve
