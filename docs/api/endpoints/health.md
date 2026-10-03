# `GET /health`

Readiness probe plus comprehensive system introspection.

## Purpose

- Confirm the server is up and responsive
- Report whether `ripgrep` is available (affects `/v1/trace` and
  `/v1/samples`)
- Expose runtime constants, env vars, hook configuration, and search
  roots for operator dashboards
- Drive the `rx-viewer` SPA's "status" panel

## Request

```text
GET /health
```

No parameters, no body.

## Response — 200 OK

From `RX_CACHE_DIR=/var/cache RX_MAX_FILES=10 rx serve --search-root=/var/log`
on an Apple-silicon Mac:

```json
{
  "status": "ok",
  "ripgrep_available": true,
  "app_version": "dev",
  "contract_version": "1.4",
  "go_version": "go1.26.2",
  "os_info": {
    "compiler": "gc",
    "machine": "arm64",
    "system": "darwin",
    "version": "go1.26.2"
  },
  "system_resources": {
    "cpu_cores": 16,
    "cpu_cores_physical": 16,
    "ram_available_gb": null,
    "ram_percent_used": null,
    "ram_total_gb": null
  },
  "go_packages": {
    "github.com/danielgtaylor/huma/v2": "v2.37.3",
    "github.com/go-chi/chi/v5": "v5.2.5",
    "github.com/google/uuid": "v1.6.0",
    "github.com/klauspost/compress": "v1.18.4",
    "github.com/prometheus/client_golang": "v1.23.2",
    "github.com/spf13/cobra": "v1.10.2"
  },
  "constants": {
    "CACHE_DIR": "/var/cache/rx",
    "LOG_LEVEL": "INFO",
    "MAX_SUBPROCESSES": 20,
    "MIN_CHUNK_SIZE_MB": 20
  },
  "environment": {
    "RX_CACHE_DIR": "/var/cache",
    "RX_MAX_FILES": "10",
    "RX_SEARCH_ROOTS": "/var/log"
  },
  "hooks": {
    "RX_DISABLE_CUSTOM_HOOKS": false,
    "RX_HOOK_ON_COMPLETE_URL": "",
    "RX_HOOK_ON_FILE_URL": "",
    "RX_HOOK_ON_MATCH_URL": "",
    "custom_hooks_enabled": true,
    "hooks_configured": false
  },
  "docs_url": "https://github.com/wlame/rx-tool",
  "search_roots": [
    "/var/log"
  ]
}
```

`RX_MAX_FILES` appears under `environment` because it is set, not
because rx reads it: nothing does. `RX_SEARCH_ROOTS` is there because
`rx serve` exports the roots it resolved.

## Response fields

| Field | Type | Description |
|---|---|---|
| `status` | string | Always `"ok"` when the server returns a response |
| `ripgrep_available` | bool | `true` if `rg` is on `PATH`. When `false`, `/v1/trace` returns `503`; the other endpoints do not need it |
| `app_version` | string | The release tag the binary was built from, or `"dev"` for a build without one |
| `contract_version` | string | HTTP wire contract version, `MAJOR.MINOR`. A higher minor adds fields or parameters; a different major means a client written for another major cannot read this server |
| `go_version` | string | Go toolchain version the binary was built with |
| `os_info` | object | `{system, machine, compiler, version}` from `runtime.GOOS`, `runtime.GOARCH`, etc. |
| `system_resources` | object | CPU cores and RAM totals. `ram_*` fields are `null` on non-Linux hosts |
| `go_packages` | object | Key dependency versions from the embedded build info |
| `constants` | object | The settings in force — see below and [configuration](../../configuration.md) |
| `environment` | object | Every env var prefixed with `RX_`, `UVICORN_` or `PROMETHEUS_`, as it is set, whether rx reads it or not. A variable whose name contains `TOKEN`, `SECRET`, `PASSWORD` or `API_KEY` reads `<redacted>`, so `RX_API_TOKEN` never leaves the server ([security](../../concepts/security.md#opt-in-api-token)) |
| `hooks` | object | Effective webhook env configuration |
| `docs_url` | string | Static URL to the `rx-tool` project |
| `search_roots` | `string[] \| null` | Configured roots, or `null` when running unsandboxed (rare in serve mode) |

### `constants` field detail

- `LOG_LEVEL`: `"DEBUG"`, `"INFO"`, `"WARN"`, or `"ERROR"`
- `MAX_SUBPROCESSES`: most chunks per file, and the worker pool size
  when `RX_WORKERS` is unset; default 20
- `MIN_CHUNK_SIZE_MB`: smallest chunk the chunker carves; default 20

Both are the values rx uses: a variable outside its range shows the
default or the maximum (see
[integer settings](../../configuration.md#integer-settings)), while
`environment` shows the variable as it is set.
- `CACHE_DIR`: resolved cache base directory

rx-python also reports `DEBUG_MODE`, `LINE_SIZE_ASSUMPTION_KB`,
`MAX_FILES` and `NEWLINE_SYMBOL` here. rx-go reads none of the
variables behind them, so it does not report them.

### `system_resources` on non-Linux

On macOS and Windows, the `ram_*` fields are `null` because the server
reads `/proc/meminfo` (Linux-only) without requiring CGO. CPU core
counts always work.

## Status codes

| Code | Meaning |
|---:|---|
| `200 OK` | Server is running. `ripgrep_available` tells you whether `/v1/trace` works. |

`GET /health` never returns non-200 — if the server is up, the response
is 200; if down, the request fails to connect.

## Example

```bash
# Basic readiness probe.
curl -sf http://127.0.0.1:7777/health >/dev/null && echo "up"

# Inspect state with jq.
curl -s http://127.0.0.1:7777/health | jq '{
  version: .app_version,
  ripgrep: .ripgrep_available,
  cores:   .system_resources.cpu_cores,
  roots:   .search_roots
}'
```

Example output:

```json
{
  "version": "dev",
  "ripgrep": true,
  "cores": 16,
  "roots": [
    "/var/log"
  ]
}
```

## Kubernetes readiness probe

```yaml
readinessProbe:
  httpGet:
    path: /health
    port: 7777
  periodSeconds:   10
  timeoutSeconds:  2
  failureThreshold: 3
```

To make readiness fail when `ripgrep` is missing, chain an additional
check:

```yaml
readinessProbe:
  exec:
    command:
      - sh
      - -c
      - >
        curl -sf http://localhost:7777/health
        | jq -e '.ripgrep_available == true' > /dev/null
```

## See also

- [`rx serve`](../../cli/serve.md) — start the server
- [Configuration](../../configuration.md) — env vars reported under `environment` and `constants`
- [API conventions](../conventions.md) — shared response patterns
