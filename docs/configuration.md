# Configuration

Reference for every environment variable and global flag `rx`
recognizes. Environment variables are read at every call (not
memoized), so `t.Setenv(...)` in tests and `env X=Y rx ...` in shell
both work without restarting anything.

## Variable precedence

Configuration is resolved in this order, later items overriding
earlier ones:

1. **Compiled defaults** — the constants in source
2. **Environment variables** — read lazily on each call site
3. **Command-line flags** — supplied to the invocation

For the cache directory:

1. `$RX_CACHE_DIR/rx`
2. `$XDG_CACHE_HOME/rx`
3. `$HOME/.cache/rx`

## Cache directory

| Variable | Default | Description |
|---|---|---|
| `RX_CACHE_DIR` | — | Base directory for `rx` caches. Appended with `rx`, so `RX_CACHE_DIR=/tmp` yields `/tmp/rx`. |
| `XDG_CACHE_HOME` | — | Standard XDG variable; used when `RX_CACHE_DIR` is unset. |

The resolved cache base holds:

- `indexes/` — line-offset indexes
- `trace_cache/` — trace result caches
- `analyzers/<name>/v<version>/` — per-analyzer output
- `frontend/` — `rx-viewer` SPA

See [concepts/caching](concepts/caching.md).

## Chunking and workers

| Variable | Default | Description |
|---|---|---|
| `RX_WORKERS` | `NumCPU` | Number of parallel worker goroutines for trace scans. Takes precedence when positive. |
| `RX_MAX_SUBPROCESSES` | `20` | Upper bound on worker goroutines (despite the name). |
| `RX_MIN_CHUNK_SIZE_MB` | `20` | Smallest chunk the parallel scanner carves. Below this file size, scans run single-threaded. |
| `RX_MAX_LINE_SIZE_KB` | `8` | Max line length the chunker tolerates; affects the forward-scan window when finding newline-aligned boundaries. |
| `RX_MAX_FILES` | `1000` | Max files a single trace request will scan. Protects against runaway directory scans. |

See [concepts/chunking](concepts/chunking.md).

## Indexing thresholds

| Variable | Default | Description |
|---|---|---|
| `RX_LARGE_FILE_MB` | `50` | Minimum file size (MB) to be indexed automatically. Below-threshold files are skipped by `rx index` unless `--analyze` is set or `--threshold=N` is supplied. |

## Cache control

Cache disabling is done per-invocation via CLI flags — there are no
"global disable" environment variables in this release:

- `--no-cache` on `rx trace` — don't consult or write the trace cache
- `--no-index` on `rx trace` — don't consult the line-index cache

`NO_COLOR` / `RX_NO_COLOR` control color output:

| Variable | Default | Description |
|---|---|---|
| `NO_COLOR` | unset | When set to any value, disables ANSI color output (standard convention) |
| `RX_NO_COLOR` | unset | When set to any value, disables ANSI color output. Takes precedence over `NO_COLOR`. |

Both are overridden by explicit `--color=always`.

## Search-root sandbox

| Variable | Default | Description |
|---|---|---|
| `RX_SEARCH_ROOTS` | — | Path-separator-delimited list of directories. Automatically exported by `rx serve` from the `--search-root` flags. Also consumed by any child processes. |

See [concepts/security](concepts/security.md).

## Frontend cache

| Variable | Default | Description |
|---|---|---|
| `RX_FRONTEND_VERSION` | unset | Pin the `rx-viewer` SPA to a specific release tag (e.g. `v0.2.0`). When unset, `rx serve` fetches the latest release. |
| `RX_FRONTEND_URL` | unset | Direct URL to a `dist.tar.gz` of the SPA. When set, `rx serve` downloads from this URL on every start. |
| `RX_FRONTEND_PATH` | — | Alternate directory for the SPA extraction. Defaults to `{cache}/frontend`. Tilde expansion supported. |

## Webhooks

| Variable | Default | Description |
|---|---|---|
| `RX_HOOK_ON_FILE_URL` | — | URL fired per file after scan |
| `RX_HOOK_ON_MATCH_URL` | — | URL fired per match (requires `max_results`) |
| `RX_HOOK_ON_COMPLETE_URL` | — | URL fired once per trace |
| `RX_DISABLE_CUSTOM_HOOKS` | `false` | When truthy, per-request hook URLs (query params / flags) are silently ignored; only env-configured URLs fire. |
| `RX_ALLOW_INTERNAL_HOOKS` | `false` | When truthy, bypass all SSRF checks. Loopback / private / link-local URLs become valid. |
| `RX_HOOK_STRICT_IP_ONLY` | `false` | When truthy, reject every hostname; only IP-literal URLs are allowed. Strongest defense against DNS rebinding. |

See [concepts/security](concepts/security.md) and
[api/webhooks](api/webhooks.md).

## Tasks

| Variable | Default | Description |
|---|---|---|
| `RX_TASK_TTL_MINUTES` | `60` | How long finished (completed/failed) tasks stay in memory before the sweeper removes them. |

## Logging

| Variable | Default | Description |
|---|---|---|
| `RX_LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` (alias `WARNING`), or `ERROR`. Sets slog level on `rx serve` startup. |
| `RX_DEBUG` | `false` | When truthy, enables debug mode — writes `.debug_*` artifacts under `RX_DEBUG_DIR` during trace/index operations. |
| `RX_DEBUG_DIR` | `$TMPDIR/rx-debug` | Directory for debug artifacts. |

## HTTP server

All HTTP server settings are flags on `rx serve`, not env vars.
See [`rx serve`](cli/serve.md).

## Newline rendering

| Variable | Default | Description |
|---|---|---|
| `NEWLINE_SYMBOL` | `\n` | Character sequence used when rendering "newline" in human-readable output. Surfaced via `GET /health` under `constants.NEWLINE_SYMBOL`. |

## Unused / compatibility variables

The `UVICORN_*` and `PROMETHEUS_*` env vars are echoed back on
`/health` under `environment` if set, for operator convenience. `rx`
itself doesn't consume them.

## Global flags

`rx` has no flags that apply across all subcommands beyond cobra's
built-ins:

| Flag | Scope | Effect |
|---|---|---|
| `--help`, `-h` | all | Print help for the current command |
| `--version` | root | Print `rx version <version>` |

Per-subcommand flags are documented on each CLI page:

- [`rx trace`](cli/trace.md)
- [`rx index`](cli/line-index.md)
- [`rx samples`](cli/samples.md)
- [`rx compress`](cli/compress.md)
- [`rx serve`](cli/serve.md)

## Inspecting effective configuration

With a running `rx serve`:

```bash
# Every RX_* env var visible to the server.
curl -s http://127.0.0.1:7777/health | jq '.environment'

# Current effective constants.
curl -s http://127.0.0.1:7777/health | jq '.constants'

# Effective hook configuration.
curl -s http://127.0.0.1:7777/health | jq '.hooks'

# Configured search roots.
curl -s http://127.0.0.1:7777/health | jq '.search_roots'
```

## Boolean parsing

All boolean env vars (`RX_DEBUG`, `RX_ALLOW_INTERNAL_HOOKS`,
`RX_HOOK_STRICT_IP_ONLY`, `RX_DISABLE_CUSTOM_HOOKS`) recognize these
truthy values:

```text
true, yes, 1, on
```

And these falsy values:

```text
false, no, 0, off
```

Matching is case-insensitive. Anything else falls back to the default.

## Integer parsing

All integer env vars (`RX_WORKERS`, `RX_MAX_SUBPROCESSES`,
`RX_MIN_CHUNK_SIZE_MB`, `RX_LARGE_FILE_MB`, `RX_MAX_FILES`,
`RX_MAX_LINE_SIZE_KB`, `RX_TASK_TTL_MINUTES`) use Go's `strconv.Atoi`:

- Plain decimal digits only
- Negative values accepted but usually produce unhelpful behavior
- Non-numeric input falls back to the default

## Example environment for production

```bash
# Large-file-friendly thresholds.
export RX_LARGE_FILE_MB=100
export RX_MIN_CHUNK_SIZE_MB=50

# Dedicated cache directory.
export RX_CACHE_DIR=/var/cache/rx

# Extended task retention.
export RX_TASK_TTL_MINUTES=240

# Webhook destinations (internal).
export RX_HOOK_ON_COMPLETE_URL=https://internal.example.com/rx-log
export RX_ALLOW_INTERNAL_HOOKS=true

# Lock down per-request overrides.
export RX_DISABLE_CUSTOM_HOOKS=true

# INFO logging.
export RX_LOG_LEVEL=INFO

rx serve --host=0.0.0.0 --port=7777 \
    --search-root=/var/log \
    --search-root=/srv/data/logs
```

## See also

- [Performance](performance.md) — tuning advice grounded in benchmarks
- [Troubleshooting](troubleshooting.md) — what to check when config
  seems to be ignored
- [concepts/caching](concepts/caching.md) — cache paths
- [concepts/security](concepts/security.md) — security-related variables
