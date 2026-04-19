# Troubleshooting

Common problems and how to resolve them.

## `ripgrep (rg) is not installed or not on PATH`

### Symptom

```text
Error: ripgrep (rg) is not installed or not on PATH
```

Exit code 1 on any `rx trace` call; `503 Service Unavailable` on
`/v1/trace` and `/v1/samples` (uncompressed path).

### Cause

`rx` delegates regex matching to `ripgrep`. The `rg` executable must
be reachable via `$PATH`.

### Fix

Install `ripgrep`:

```bash
# Linux (debian/ubuntu)
sudo apt install ripgrep

# Linux (rhel/fedora)
sudo dnf install ripgrep

# macOS
brew install ripgrep

# Verify
rg --version
```

On `rx serve`, check:

```bash
curl -s http://127.0.0.1:7777/health | jq '.ripgrep_available'
# Should be true.
```

If `rg` is installed but `rx` doesn't see it, the PATH at server
startup time didn't include `rg`'s location. Restart `rx serve` with
an explicit `PATH=/usr/local/bin:/usr/bin:... rx serve`.

## `path is outside all search roots`

### Symptom

```text
Access denied: path '/etc/passwd' is outside all search roots: '/var/log'
```

Exit code 4 on CLI; `403 path_outside_search_root` on HTTP.

### Cause

The requested path doesn't resolve into any configured
`--search-root`.

### Fix

Either:

- Add the relevant directory with `--search-root=/path`
- Move your test file into an already-allowed directory
- For CLI use, clear the sandbox by not setting `RX_SEARCH_ROOTS`

For CLI use outside `rx serve`, the sandbox is typically unset — if
you're hitting this error on the CLI, someone set `RX_SEARCH_ROOTS`
globally. Check:

```bash
env | grep RX_SEARCH_ROOTS
```

Unset it (`unset RX_SEARCH_ROOTS`) if you want an unsandboxed CLI run.

## Cache directory permission denied

### Symptom

```text
Error: mkdir /home/you/.cache/rx/indexes: permission denied
```

### Cause

The cache directory isn't writable by the running user.

### Fix

Check ownership:

```bash
ls -la ~/.cache/rx
# Should be owned by the running user.
```

If running `rx` under a service account without a writable `$HOME`,
set `RX_CACHE_DIR`:

```bash
sudo -u rxuser env RX_CACHE_DIR=/var/cache/rx rx serve
```

Ensure the directory exists and is writable:

```bash
sudo mkdir -p /var/cache/rx
sudo chown rxuser:rxuser /var/cache/rx
```

## Frontend SPA not loading

### Symptom

Browsing to `http://127.0.0.1:7777/` shows a redirect to `/docs`
(Swagger UI) instead of the `rx-viewer` SPA.

### Cause

The SPA download failed or was skipped:

- Network firewalled from GitHub
- Rate limited on first fetch
- `--skip-frontend` was passed
- `RX_FRONTEND_URL` or `RX_FRONTEND_VERSION` points at a bad source

### Fix

Check the startup log — `rx serve` prints a warning when the fetch
fails:

```text
Warning: frontend fetch failed (...). Continuing without SPA.
```

Either:

- Connect the host to GitHub and restart
- Pre-populate `~/.cache/rx/frontend/` from another machine:
  `scp -r machineA:~/.cache/rx/frontend ~/.cache/rx/`
- Set `RX_FRONTEND_URL` to a mirror you control
- Live with Swagger UI at `/docs` — all API functionality works
  without the SPA

## Webhook not firing

### Symptom

A configured webhook URL doesn't receive POST requests, with no
obvious error on the client side.

### Cause 1: SSRF rejection

The URL is in a blocked address range.

Check the server logs:

```text
ERROR hook_validation_failed url=http://10.0.0.5/webhook reason="points at a private-network address"
```

### Fix

Either:

- Use a public-IP URL
- Set `RX_ALLOW_INTERNAL_HOOKS=true` (bypasses SSRF guards — use
  carefully)

See [api/webhooks](api/webhooks.md) for the full address-range
policy.

### Cause 2: `on_match` without `max_results`

If `hook_on_match` is set but `max_results` isn't, the request
returns `400`:

```text
max_results is required when hook_on_match is configured.
```

### Fix

Add `max_results` (query param) or `--max-results` (CLI flag).

### Cause 3: Queue drops under load

The webhook dispatcher has a 512-event queue. If your endpoint is
slow, events can be dropped.

Check:

```promql
rate(rx_hook_calls_total{status="failure"}[5m])
```

### Fix

Make the webhook endpoint faster — accept the payload asynchronously
and respond `202` immediately.

## Index not being used

### Symptom

`rx samples --lines=1000000` is slow even after `rx index` has been
run on the file.

### Cause

- The source file's mtime changed since the index was built
- The index is being consumed but the target line is far from the
  nearest checkpoint
- `--no-index` was passed on the command line

### Diagnosis

Check cache validity:

```bash
rx index /var/log/huge.log --info
```

If it reports `no index exists` or the `created_at` predates the
file's mtime, rebuild:

```bash
rx index /var/log/huge.log --force
```

Verify by re-running with an explicit fresh invocation:

```bash
rx samples /var/log/huge.log --lines=1000000 --context=3
```

## OOM killer on large multi-pattern scans

### Symptom

`rx` is killed by the Linux OOM killer or reports allocation
failures on scans with multiple complex patterns.

### Cause

Each worker holds a copy of the compiled regex set. With 5 complex
patterns and 8 workers, peak RSS can reach several GB.

### Fix

- Reduce `RX_WORKERS` (or `--workers` on encode): fewer parallel
  automatons
- Split into multiple invocations (one pattern at a time)
- Use `--max-results` to cap result memory

See [performance#memory-profile](performance.md#memory-profile).

## Below-threshold files skipped by `rx index`

### Symptom

```bash
rx index /tmp/small.log
```

Output:

```text
No files indexed.
Skipped 1 files (below threshold or not text)
```

### Cause

Default threshold is 50 MB. Smaller files are intentionally skipped.

### Fix

Lower the threshold per-invocation:

```bash
rx index /tmp/small.log --threshold=1
```

Or globally:

```bash
RX_LARGE_FILE_MB=1 rx index /tmp/small.log
```

Or force analysis (bypasses the threshold):

```bash
rx index /tmp/small.log --analyze
```

## Byte-offset mode rejected on compressed file

### Symptom

```text
Byte offsets are not supported for compressed files. Use 'lines' parameter instead.
```

### Cause

Byte offsets into a compressed stream have no stable decompressed
semantics. `rx` refuses the combination to avoid returning wrong
results.

### Fix

Use line-offset mode (`--lines`) instead. For random access on
compressed data, re-encode the file as seekable zstd:

```bash
rx compress /var/log/huge.log.gz --frame-size=2M
# Produces /var/log/huge.log.gz.zst
```

See [concepts/compression](concepts/compression.md).

## Server startup fails with bind error

### Symptom

```text
listen tcp 127.0.0.1:7777: bind: address already in use
```

### Cause

Another process (or another `rx serve`) is already using port 7777.

### Fix

```bash
# Find the holder.
ss -tlnp | grep 7777

# Or pick a different port.
rx serve --port=8080
```

## Request seems to hang

### Symptom

`curl http://127.0.0.1:7777/v1/trace?...` never returns.

### Cause

Long-running scan. The request runs to completion; there's no
server-side timeout.

### Diagnosis

Check the server log for this request's `request_id`:

```bash
curl -sG http://127.0.0.1:7777/v1/trace ... | jq '.request_id'
# Cross-reference with server logs.
```

Or inspect active workers:

```bash
curl -s http://127.0.0.1:7777/metrics | grep rx_active_workers
```

### Fix

For future requests:

- Add `max_results` to cap output
- Narrow the file set (`--path=<specific-file>` instead of directory)
- Build indexes so line-number resolution doesn't dominate
- Use `rx trace` with simpler patterns on a first pass

## `--force` not rebuilding an index

### Symptom

`rx index /path --force` reports "using cached index" or completes
suspiciously fast.

### Cause

This shouldn't happen — `--force` explicitly bypasses the cache-check
step. If it does:

- Make sure you're running the intended `rx` binary (`which rx`)
- The output `cache=<path>` line prints the actual file. Check its
  mtime.

### Fix

Delete the cache file manually:

```bash
rx index /path --delete
rx index /path
```

## Swagger UI shows no endpoints

### Symptom

`/docs` renders but shows zero endpoints or generic text.

### Cause

The OpenAPI JSON isn't loading. Usually a reverse proxy stripping
`/openapi.json` or a browser-side network error.

### Diagnosis

```bash
curl -sI http://127.0.0.1:7777/openapi.json
# Expect: 200 OK, Content-Type: application/json
```

Open browser DevTools → Network tab → reload `/docs`. Check the
request to `openapi.json`.

### Fix

- If behind a proxy: ensure `/openapi.json` and `/openapi.yaml` both
  proxy through unchanged
- If the request is going to a different origin: set a `Host`
  rewrite or configure Swagger UI with the right `spec-url` (this
  requires modifying the embedded UI, not trivial)

## CGO errors on older Linux distros

### Symptom

```text
./rx: error while loading shared libraries: GLIBC_X.XX not found
```

### Cause

You grabbed a dynamically-linked binary built for a newer glibc than
your host provides. The official static build shouldn't hit this.

### Fix

Use the static binary:

```bash
file rx
# Expect: statically linked
```

Or build from source on the target host:

```bash
CGO_ENABLED=0 go build -ldflags='-s -w' -o rx ./cmd/rx
```

## Debug mode

When stuck, enable debug mode:

```bash
RX_DEBUG=true rx trace "error" /var/log/app.log
```

This writes `.debug_*` artifacts under `$TMPDIR/rx-debug/` (override
via `RX_DEBUG_DIR`):

- Per-chunk ripgrep invocations
- Parsed JSON output
- Intermediate result slices

Include these files if you're filing an issue about incorrect trace
output.

## Capturing logs from `rx serve`

`rx serve` writes structured logs to stderr via `slog`. Redirect:

```bash
rx serve 2>>/var/log/rx.log
```

Or under systemd:

```ini
[Service]
ExecStart=/usr/local/bin/rx serve --host=0.0.0.0 --search-root=/var/log
StandardError=journal
```

Then `journalctl -u rx` shows structured log records.

## Getting help

Still stuck?

1. Check `GET /health` for any anomalies (`ripgrep_available: false`,
   `search_roots: null`, etc.)
2. Try with `RX_DEBUG=true` and capture the `.debug_*` files
3. File an issue at <https://github.com/wlame/rx-go/issues> with:
   - `rx --version` output
   - Exact command that reproduces
   - Full error output
   - `/health` JSON if the problem is server-side

## See also

- [Installation](installation.md)
- [Configuration](configuration.md)
- [Performance](performance.md)
- [Concepts](concepts/index.md)
