# `GET /metrics`

Prometheus-format exposition of runtime counters, histograms, and
gauges. Standard for scraping by a Prometheus server or any
OpenMetrics-compatible collector.

## Purpose

- Monitor request rates and latency per endpoint
- Track webhook dispatch success/failure
- Expose scan counters (matches, files scanned, bytes processed)
- Surface Go runtime health (goroutines, memory, GC)

## Request

```text
GET /metrics
```

No parameters. Response content type is `text/plain; version=0.0.4`
(Prometheus exposition format, not JSON).

## Availability

Metrics are only populated when `rx` is running in **serve mode**. CLI
invocations use a no-op metrics sink and never allocate counters.

## Metric families

### Request-level

| Metric | Type | Labels | Description |
|---|---|---|---|
| `rx_trace_requests_total` | counter | `status` | Total trace requests (`ok` / `error`) |
| `rx_samples_requests_total` | counter | `status` | Total samples requests |
| `rx_analyze_requests_total` | counter | `status` | Total `--analyze` requests |
| `rx_trace_duration_seconds` | histogram | `path_kind` | Trace duration, labeled `regular`/`compressed`/`seekable` |
| `rx_samples_duration_seconds` | histogram | — | Samples request duration |
| `rx_http_responses_total` | counter | `method`, `endpoint`, `status_code` | HTTP responses |

### Work counters

| Metric | Type | Labels | Description |
|---|---|---|---|
| `rx_files_processed_total` | counter | — | Files opened and scanned |
| `rx_files_skipped_total` | counter | — | Files skipped (binary, inaccessible) |
| `rx_bytes_processed_total` | counter | — | Total bytes read |
| `rx_matches_found_total` | counter | — | Total matches returned |
| `rx_max_results_limited_total` | counter | — | Requests that hit `max_results` cap |

### Cache

| Metric | Type | Labels | Description |
|---|---|---|---|
| `rx_index_cache_hits_total` | counter | — | Index cache hits |
| `rx_index_cache_misses_total` | counter | — | Index cache misses |
| `rx_trace_cache_hits_total` | counter | — | Trace cache hits |
| `rx_trace_cache_misses_total` | counter | — | Trace cache misses |
| `rx_trace_cache_writes_total` | counter | — | Trace cache writes |
| `rx_index_build_duration_seconds` | histogram | — | Index build time |

### Workers

| Metric | Type | Labels | Description |
|---|---|---|---|
| `rx_active_workers` | gauge | — | Live count of worker goroutines |
| `rx_worker_tasks_completed_total` | counter | — | Chunk-level tasks completed |
| `rx_worker_tasks_failed_total` | counter | — | Chunk-level tasks failed |

### Hooks (webhooks)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `rx_hook_calls_total` | counter | `kind`, `status` | Webhook POSTs by kind and `success`/`failure` |
| `rx_hook_call_duration_seconds` | histogram | `kind` | Webhook POST latency |

### Shape & distribution

| Metric | Type | Labels | Description |
|---|---|---|---|
| `rx_errors_total` | counter | `error_type` | Errors by category |
| `rx_file_size_bytes` | histogram | — | Distribution of file sizes at scan time |
| `rx_patterns_per_request` | histogram | — | How many patterns per request |
| `rx_matches_per_request` | histogram | — | How many matches per request |
| `rx_parallel_tasks_created` | histogram | — | Concurrent tasks per request |

### Go runtime (standard)

The default `promhttp` registry also exposes:

- `go_goroutines`
- `go_threads`
- `go_gc_duration_seconds`
- `go_memstats_*`
- `process_*`

## Label cardinality

- `endpoint` uses the **route pattern** (e.g. `/v1/tasks/{task_id}`),
  not the concrete path (`/v1/tasks/abc-123`). This prevents
  cardinality explosion from variable path parameters.
- `status_code` is the exact HTTP status code as a string (`"200"`,
  `"404"`, etc.)
- `method` is uppercase HTTP method (`"GET"`, `"POST"`)
- `kind` is one of `on_file`, `on_match`, `on_complete`
- `status` on `rx_hook_calls_total` is `success` or `failure`
- `error_type` values are enumerated internally

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Always |

## Example

### Raw scrape

```bash
curl -s 'http://127.0.0.1:7777/metrics' | head -30
```

Output excerpt:

```text
# HELP rx_http_responses_total HTTP responses by status code
# TYPE rx_http_responses_total counter
rx_http_responses_total{method="GET",endpoint="/v1/trace",status_code="200"} 42
rx_http_responses_total{method="GET",endpoint="/v1/trace",status_code="400"} 1
rx_http_responses_total{method="GET",endpoint="/health",status_code="200"} 287

# HELP rx_trace_duration_seconds Time spent serving trace requests
# TYPE rx_trace_duration_seconds histogram
rx_trace_duration_seconds_bucket{path_kind="regular",le="0.005"} 3
rx_trace_duration_seconds_bucket{path_kind="regular",le="0.01"} 12
```

### Prometheus scrape config

```yaml
scrape_configs:
  - job_name: rx
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:7777']
    metrics_path: /metrics
```

### Useful queries

```promql
# Request rate by endpoint.
sum by (endpoint) (rate(rx_http_responses_total[5m]))

# p95 trace latency.
histogram_quantile(0.95,
  sum by (le) (rate(rx_trace_duration_seconds_bucket[5m])))

# Trace cache hit ratio.
  rate(rx_trace_cache_hits_total[5m])
/ (rate(rx_trace_cache_hits_total[5m]) + rate(rx_trace_cache_misses_total[5m]))

# Webhook failure rate by kind.
sum by (kind) (rate(rx_hook_calls_total{status="failure"}[5m]))

# Currently-running worker goroutines.
rx_active_workers
```

## Performance notes

- Each metric update is a single atomic increment or histogram
  observation — tens of nanoseconds per request
- `/metrics` response size scales with unique label combinations; with
  the cardinality constraints above, a typical response is a few KB
- No separate metrics port — served on the same bind address as the
  rest of the API

## See also

- [`rx serve`](../../cli/serve.md) — start the server to enable metrics
- [Configuration](../../configuration.md) — `PROMETHEUS_*` env vars exposed via `/health`
- [API conventions](../conventions.md)
