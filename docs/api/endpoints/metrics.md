# `GET /metrics`

Prometheus-format exposition of runtime counters, histograms, and
gauges. Standard for scraping by a Prometheus server or any
OpenMetrics-compatible collector.

## Purpose

- Monitor request rates and latency per endpoint
- Track webhook dispatch success/failure
- Expose scan counters (matches, files scanned, bytes processed)
- Surface Go runtime and process health (goroutines, memory, GC, CPU,
  file descriptors)

## Request

```text
GET /metrics
```

No parameters. Response content type is `text/plain; version=0.0.4`
(Prometheus exposition format, not JSON).

## Availability

Metrics are only updated when `rx` is running in **serve mode**. CLI
invocations skip every update, so they pay nothing for them.

A family with labels (`status`, `kind`, `endpoint` and so on) appears in
a scrape only after its first update; a family without labels appears
from the start, at zero.

## Metric families

These are the 37 `rx_*` families `/metrics` reports, with the Go
runtime and process families listed after them.

### Requests

| Metric | Type | Labels | Updated |
|---|---|---|---|
| `rx_trace_requests_total` | counter | `status` | Once per `GET /v1/trace`: `success` when it answered, `error` otherwise |
| `rx_samples_requests_total` | counter | `status` | Once per `GET /v1/samples`, `success` or `error` |
| `rx_analyze_requests_total` | counter | `status` | Once per `POST /v1/index` with `analyze: true`, `success` or `error` (whether the task was accepted) |
| `rx_trace_duration_seconds` | histogram | `path_kind` | Per answered trace; `regular`, `compressed` or `seekable` after the most expensive path the request named |
| `rx_samples_duration_seconds` | histogram | — | Per answered samples request |
| `rx_offsets_per_samples_request` | histogram | — | The offsets or lines each answered samples request asked for |
| `rx_context_lines_before` | histogram | — | The context before each position, per answered samples request |
| `rx_context_lines_after` | histogram | — | The context after each position, per answered samples request |
| `rx_analyze_duration_seconds` | histogram | — | Per index build with analysis, from the task's start to the saved index; reusing an analyzed index is not counted |
| `rx_http_responses_total` | counter | `method`, `endpoint`, `status_code` | Once per HTTP response, with the status the client received |
| `rx_errors_total` | counter | `error_type` | Per failed trace, samples or analyze request |

### Trace work

| Metric | Type | Labels | Updated |
|---|---|---|---|
| `rx_files_processed_total` | counter | — | Per file a trace scanned or answered from its cache |
| `rx_files_skipped_total` | counter | — | Per file a trace answer lists in `skipped_files` |
| `rx_bytes_processed_total` | counter | — | The on-disk size of each file `rx_files_processed_total` counts |
| `rx_file_size_bytes` | histogram | — | The on-disk size of each file `rx_files_processed_total` counts |
| `rx_matches_found_total` | counter | — | The matches each trace returned |
| `rx_patterns_per_request` | histogram | — | The pattern count of each answered trace |
| `rx_matches_per_request` | histogram | — | The match count of each answered trace |
| `rx_max_results_limited_total` | counter | — | Per answered trace whose match count reached `max_results` |
| `rx_parallel_tasks_created` | histogram | — | The chunks of each plain file scanned, or the frame batches of each seekable `.zst` |
| `rx_ripgrep_processing_seconds` | histogram | — | The duration of each ripgrep run on a chunk |
| `rx_large_file_threshold_mb` | gauge | — | Set when `serve` starts: `RX_LARGE_FILE_MB`, the size from which a plain file's trace is cached and an index is built |

### Workers

| Metric | Type | Labels | Updated |
|---|---|---|---|
| `rx_active_workers` | gauge | — | Up when a chunk, frame batch or compressed-stream scan starts, down when it ends |
| `rx_worker_tasks_completed_total` | counter | — | Per scan task that ended without an error |
| `rx_worker_tasks_failed_total` | counter | — | Per scan task that ended with an error, an invalid pattern included. A task stopped by a `max_results` cap or an abandoned request counts in neither family |

### Trace cache

| Metric | Type | Labels | Updated |
|---|---|---|---|
| `rx_trace_cache_hits_total` | counter | — | Per lookup that found a valid cache (plain files above the large-file threshold, and seekable `.zst` files) |
| `rx_trace_cache_misses_total` | counter | — | Per lookup that found none, or one that no longer matches its file |
| `rx_trace_cache_writes_total` | counter | — | Per cache file written |
| `rx_trace_cache_skip_total` | counter | — | Per scan the cache is on for but that does not qualify: below the size threshold for its kind, or capped by `max_results` |
| `rx_trace_cache_load_duration_seconds` | histogram | — | Per cache file read and parsed |
| `rx_trace_cache_reconstruction_seconds` | histogram | — | Per answer rebuilt from a cache hit |

### Line index

| Metric | Type | Labels | Updated |
|---|---|---|---|
| `rx_index_cache_hits_total` | counter | — | Per index lookup that found a valid index (trace, samples, `GET /v1/index`, `POST /v1/index`) |
| `rx_index_cache_misses_total` | counter | — | Per index lookup that found none, or a stale one |
| `rx_index_load_duration_seconds` | histogram | — | Per index file read and checked |
| `rx_index_build_duration_seconds` | histogram | — | Per index built |

A `GET /v1/tree` listing shows whether each file has an index without
using it, so it does not count as a lookup.

### Webhooks

| Metric | Type | Labels | Updated |
|---|---|---|---|
| `rx_hook_calls_total` | counter | `kind`, `status` | Per webhook event: `success`, `failure` or `dropped` (queue full or dispatcher closed) |
| `rx_hook_call_duration_seconds` | histogram | `kind` | Per webhook call made |

### Go runtime and process

The registry also holds the standard collectors of the Prometheus Go
client, read at scrape time:

- `go_*`: `go_goroutines`, `go_threads`, `go_info`,
  `go_gc_duration_seconds`, `go_gc_gogc_percent`,
  `go_gc_gomemlimit_bytes`, `go_sched_gomaxprocs_threads` and the
  `go_memstats_*` family
- `process_*`: CPU seconds, open and maximum file descriptors,
  resident and virtual memory, and start time; the exact set depends on
  the platform
- `promhttp_metric_handler_errors_total`: failed scrapes

## Label values

- `endpoint` is the **route pattern** (`/v1/tasks/{task_id}`), never
  the concrete path (`/v1/tasks/abc-123`). A request no route matches
  (a path nothing serves, or a method a path does not accept) is
  `unmatched`. The number of series stays fixed whatever paths clients
  try.
- `status_code` is the HTTP status as a string (`"200"`, `"404"`).
- `method` is the uppercase HTTP method (`"GET"`, `"POST"`).
- `status` on the request counters is `success` or `error`, as in
  rx-python.
- `path_kind` is `regular`, `compressed` or `seekable`.
- `kind` on the webhook families is `on_file`, `on_match` or
  `on_complete`; `status` on `rx_hook_calls_total` is `success`,
  `failure` or `dropped`.
- `error_type` is `invalid_regex`, `invalid_params`, `access_denied`,
  `file_not_found`, `service_unavailable` or `internal_error`.

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
rx_http_responses_total{endpoint="/health",method="GET",status_code="200"} 287
rx_http_responses_total{endpoint="/v1/trace",method="GET",status_code="200"} 42
rx_http_responses_total{endpoint="/v1/trace",method="GET",status_code="400"} 1

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
- [Configuration](../../configuration.md)
- [API conventions](../conventions.md)
