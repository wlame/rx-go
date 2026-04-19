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

```json
{
  "status":            "ok",
  "ripgrep_available": true,
  "app_version":       "2.2.1-go",
  "go_version":        "go1.26.2",
  "os_info": {
    "system":   "linux",
    "machine":  "arm64",
    "compiler": "gc",
    "version":  "go1.26.2"
  },
  "system_resources": {
    "cpu_cores":          4,
    "cpu_cores_physical": 4,
    "ram_total_gb":       16.0,
    "ram_available_gb":   12.3,
    "ram_percent_used":   23.1
  },
  "go_packages": {
    "github.com/danielgtaylor/huma/v2":    "v2.x.x",
    "github.com/go-chi/chi/v5":            "v5.x.x",
    "github.com/klauspost/compress":       "v1.x.x",
    "github.com/prometheus/client_golang": "v1.x.x",
    "github.com/spf13/cobra":              "v1.x.x",
    "github.com/google/uuid":              "v1.x.x"
  },
  "constants": {
    "LOG_LEVEL":               "INFO",
    "DEBUG_MODE":              false,
    "LINE_SIZE_ASSUMPTION_KB": 8,
    "MAX_SUBPROCESSES":        20,
    "MIN_CHUNK_SIZE_MB":       20,
    "MAX_FILES":               1000,
    "NEWLINE_SYMBOL":          "'\\n'",
    "CACHE_DIR":               "/home/you/.cache/rx"
  },
  "environment": {
    "RX_LARGE_FILE_MB": "50",
    "RX_CACHE_DIR":     "/opt/cache"
  },
  "hooks": {
    "RX_HOOK_ON_FILE_URL":     "",
    "RX_HOOK_ON_MATCH_URL":    "",
    "RX_HOOK_ON_COMPLETE_URL": "",
    "RX_DISABLE_CUSTOM_HOOKS": false,
    "custom_hooks_enabled":    true,
    "hooks_configured":        false
  },
  "docs_url":     "https://github.com/wlame/rx-tool",
  "search_roots": ["/var/log", "/var/data/exports"]
}
```

## Response fields

| Field | Type | Description |
|---|---|---|
| `status` | string | Always `"ok"` when the server returns a response |
| `ripgrep_available` | bool | `true` if `rg` is on `PATH`. When `false`, `/v1/trace` and `/v1/samples` return `503` |
| `app_version` | string | `rx` version (`"2.2.1-go"` or `"dev"` in unreleased builds) |
| `go_version` | string | Go toolchain version the binary was built with |
| `os_info` | object | `{system, machine, compiler, version}` from `runtime.GOOS`, `runtime.GOARCH`, etc. |
| `system_resources` | object | CPU cores and RAM totals. `ram_*` fields are `null` on non-Linux hosts |
| `go_packages` | object | Key dependency versions from the embedded build info |
| `constants` | object | Current runtime tunables — see [configuration](../../configuration.md) |
| `environment` | object | Every env var prefixed with `RX_`, `UVICORN_`, `PROMETHEUS_`, plus `NEWLINE_SYMBOL` |
| `hooks` | object | Effective webhook env configuration |
| `docs_url` | string | Static URL to the `rx-tool` project |
| `search_roots` | `string[] \| null` | Configured roots, or `null` when running unsandboxed (rare in serve mode) |

### `constants` field detail

- `LOG_LEVEL`: `"DEBUG"`, `"INFO"`, `"WARN"`, or `"ERROR"`
- `DEBUG_MODE`: whether `RX_DEBUG` is truthy
- `LINE_SIZE_ASSUMPTION_KB`: max line length the chunker tolerates
- `MAX_SUBPROCESSES`: goroutine cap (despite the historical name)
- `MIN_CHUNK_SIZE_MB`: smallest chunk the chunker carves; default 20
- `MAX_FILES`: per-trace file-count cap; default 1000
- `NEWLINE_SYMBOL`: repr of the newline marker used in output, default `'\n'`
- `CACHE_DIR`: resolved cache base directory

### `system_resources` on non-Linux

On macOS and Windows, the `ram_*` fields are `null` because the server
reads `/proc/meminfo` (Linux-only) without requiring CGO. CPU core
counts always work.

## Status codes

| Code | Meaning |
|---:|---|
| `200 OK` | Server is running. `ripgrep_available` tells you whether search endpoints work. |

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
  "version": "2.2.1-go",
  "ripgrep": true,
  "cores":   4,
  "roots":   ["/var/log", "/var/data/exports"]
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
