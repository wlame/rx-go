# `/v1/index` endpoints

Two endpoints share this path:

- `GET /v1/index` — fetch a cached line index
- `POST /v1/index` — build a new index (background task)

Both are HTTP counterparts to [`rx index`](../../cli/line-index.md).

## `GET /v1/index`

Return the cached `UnifiedFileIndex` for a file, or `404` if no cache
entry exists.

### Request

```text
GET /v1/index?path=<file>
```

### Query parameters

| Parameter | Type | Required | Description |
|---|---|:-:|---|
| `path` | `string` | yes | File path |

### Response — 200 OK

```json
{
  "path":               "/var/log/audit-2026-03.log",
  "file_type":          "text",
  "size_bytes":         582137856,
  "created_at":         "2026-04-18T14:22:03.501729",
  "build_time_seconds": 1.234,
  "analysis_performed": false,
  "line_index": [
    { "line_number": 1,       "byte_offset": 0 },
    { "line_number": 12500,   "byte_offset": 2097152 },
    { "line_number": 25000,   "byte_offset": 4194304 }
  ],
  "index_entries":   2187,
  "line_count":      3528914,
  "empty_line_count": null,
  "line_ending":      null,
  "line_length":      null,
  "longest_line":     null,
  "compression_format":      null,
  "decompressed_size_bytes": null,
  "compression_ratio":       null,
  "anomaly_count":   0,
  "anomaly_summary": {},
  "anomalies":       null,
  "cli_command":     "rx index --info /var/log/audit-2026-03.log"
}
```

### Response fields (key selection)

| Field | Type | Description |
|---|---|---|
| `path` | string | Source file path |
| `file_type` | string | `"text"`, `"compressed"`, `"seekable_zstd"`, or `"binary"` |
| `size_bytes` | int64 | Source file size when the index was built |
| `created_at` | string | ISO 8601 UTC with microsecond precision |
| `build_time_seconds` | number | Wall-clock build time |
| `analysis_performed` | bool | Whether `--analyze` was run |
| `line_index` | array | Sparse `{line_number, byte_offset}` checkpoints |
| `index_entries` | int | `len(line_index)` |
| `line_count` | int64 \| null | Total lines, populated when analysis ran |
| `line_length` | object \| null | `{max, avg, median, p95, p99, stddev}` when analyzed |
| `longest_line` | object \| null | `{line_number, byte_offset}` of the longest line |
| `compression_format` | string \| null | For compressed inputs only |
| `anomalies` | array \| null | Anomaly detector output — populated when `analyze=true`, `null` otherwise. See [analyzers](../../concepts/analyzers.md) for the shipped catalog |
| `cli_command` | string | Equivalent CLI command |

### Status codes

| Code | When |
|---:|---|
| `200 OK` | Cache exists and is returned |
| `403 Forbidden` | Path outside `--search-root` |
| `404 Not Found` | No cached index (use `POST /v1/index` to build one) |

### Example

```bash
curl -s 'http://127.0.0.1:7777/v1/index?path=/var/log/audit-2026-03.log' \
    | jq '{entries: .index_entries, lines: .line_count, built: .created_at}'
```

If the response is `404`:

```json
{ "detail": "No index found for /var/log/audit-2026-03.log. Use POST /v1/index to create an indexing task." }
```

---

## `POST /v1/index`

Build a line index as a background task.

### Request

```text
POST /v1/index
Content-Type: application/json
```

### Request body

| Field | Type | Required | Default | Description |
|---|---|:-:|---|---|
| `path` | string | yes | — | File path |
| `force` | bool | no | `false` | Rebuild even if a valid cache exists |
| `analyze` | bool | no | `false` | Run full analysis (line stats, anomalies) |
| `analyze_window_lines` | int | no | `0` (resolver default 128) | Sliding-window size for the anomaly coordinator. Ignored when `analyze=false`. Clamped to `[1, 2048]` |
| `threshold` | int \| null | no | `RX_LARGE_FILE_MB` (default 50) | Min file size in MB |

`analyze_window_lines` controls how far multi-line detectors
(tracebacks, JSON blobs) can look back. The request value overrides
both the `--analyze-window-lines` CLI flag (if the caller is the
in-process CLI) and the `RX_ANALYZE_WINDOW_LINES` env var. See
[concepts/analyzers](../../concepts/analyzers.md).

### Response — 200 OK

```json
{
  "task_id":    "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":     "queued",
  "message":    "Indexing task started for /var/log/audit-2026-03.log",
  "path":       "/var/log/audit-2026-03.log",
  "started_at": "2026-04-18T14:23:45.123456Z"
}
```

### Status codes

| Code | When |
|---:|---|
| `200 OK` | Task queued. Poll `GET /v1/tasks/{task_id}` |
| `400 Bad Request` | Below size threshold; `analyze` conflicts with cache reuse |
| `403 Forbidden` | Path outside `--search-root` |
| `404 Not Found` | File doesn't exist |
| `409 Conflict` | Another index task for the same path is already running |

### Error examples

#### Below threshold

```json
{
  "detail": "File size 1024 bytes is below threshold 52428800 bytes"
}
```

Status: `400`. Override per-request with `threshold` in the body or
globally with `RX_LARGE_FILE_MB`.

#### Duplicate task

```json
{
  "detail": "Indexing already in progress for /var/log/audit-2026-03.log (task: abc-123-def)"
}
```

Status: `409`. Poll the existing task — duplicate POSTs don't start
multiple builds.

### Example — build and poll

```bash
# Kick off the task.
task=$(curl -sXPOST 'http://127.0.0.1:7777/v1/index' \
    -H 'Content-Type: application/json' \
    -d '{"path":"/var/log/audit-2026-03.log"}')

task_id=$(echo "$task" | jq -r '.task_id')

# Poll until complete.
while :; do
    resp=$(curl -s "http://127.0.0.1:7777/v1/tasks/$task_id")
    status=$(echo "$resp" | jq -r '.status')
    echo "$status"
    [ "$status" = "completed" ] && break
    [ "$status" = "failed" ] && { echo "$resp" | jq '.error'; exit 1; }
    sleep 1
done

# Consume the result.
echo "$resp" | jq '.result | {lines: .line_count, entries: .index_entries}'
```

### Example — full analysis

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/index' \
    -H 'Content-Type: application/json' \
    -d '{"path":"/var/log/audit-2026-03.log","analyze":true}' \
    | jq '.task_id'
```

### Example — full analysis with a wider detector window

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/index' \
    -H 'Content-Type: application/json' \
    -d '{"path":"/var/log/audit-2026-03.log","analyze":true,"analyze_window_lines":512}' \
    | jq '.task_id'
```

Useful when the file contains very long tracebacks or large multi-line
JSON blobs that the default 128-line window would truncate.

### Cache reuse behavior

- `force=false` + valid cache exists → task completes instantly with
  the cached data
- `force=false` + valid cache + `analyze=true` + cache lacks analysis
  → rebuild
- `force=true` → always rebuild

### Result shape

Once `status == "completed"`, the task's `result` field contains the
same JSON shape as `GET /v1/index` returns, plus:

```json
{
  "success":    true,
  "index_path": "/home/you/.cache/rx/indexes/audit-2026-03.log_<hash>.json"
}
```

See [tasks](tasks.md) for the task polling contract.

## Performance notes

- `GET /v1/index` warm reads complete in ~10 ms regardless of file size
- `POST /v1/index` without `analyze`: roughly real-time-per-GB of source
- `POST /v1/index` with `analyze`: 2-4× slower due to line-length stats
- Cache validity is based on source mtime + size. Manual mtime changes
  (`touch -t ...`) invalidate the cache and trigger a rebuild

## See also

- [`rx index`](../../cli/line-index.md) — CLI equivalent
- [Tasks](tasks.md) — polling background operations
- [concepts/line-indexes](../../concepts/line-indexes.md) — what indexes contain
- [concepts/caching](../../concepts/caching.md) — cache layout and invalidation
