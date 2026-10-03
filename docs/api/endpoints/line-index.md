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
    [1,     0],
    [12500, 2097152],
    [25000, 4194304]
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
  "anomaly_summary": null,
  "anomalies":       null,
  "cli_command":     "rx index /var/log/audit-2026-03.log --info --json"
}
```

The OpenAPI document names this shape `IndexResponse`.

### Response fields (key selection)

| Field | Type | Description |
|---|---|---|
| `path` | string | Source file path |
| `file_type` | string | `"text"`, `"compressed"`, `"seekable_zstd"`, or `"binary"` |
| `size_bytes` | int64 | Source file size when the index was built |
| `created_at` | string | ISO 8601 UTC with microsecond precision |
| `build_time_seconds` | number | Wall-clock build time |
| `analysis_performed` | bool | Whether `--analyze` was run |
| `line_index` | array | Sparse checkpoints, each an array (`LineIndexEntry`): `[line_number, byte_offset]`, or `[line_number, byte_offset, frame_index]` in a seekable-zstd index, where the third element is the 0-based frame holding the line |
| `index_entries` | int | `len(line_index)` |
| `line_count` | int64 \| null | Total lines, populated when analysis ran |
| `line_length` | object \| null | `{max, avg, median, p95, p99, stddev}` when analyzed |
| `longest_line` | object \| null | `{line_number, byte_offset}` of the longest line |
| `compression_format` | string \| null | For compressed inputs only |
| `anomalies` | array \| null | Anomaly detector output — populated when `analyze=true`, `null` otherwise. See [analyzers](../../concepts/analyzers.md) for the shipped catalog |
| `cli_command` | string | Equivalent CLI command; see below |

### The equivalent CLI command

`cli_command` is `rx index PATH --info --json`: the command that reads
the same stored index without building one. It prints the whole stored
index (`UnifiedFileIndex`, 39 members), of which this answer is a
projection:

- `path` and `size_bytes` are `source_path` and `source_size_bytes`
  there.
- `line_length` and `longest_line` group the flat `line_length_*`
  members there.
- `index_entries` and `anomaly_count` are counted here; the CLI prints
  the `line_index` and `anomalies` arrays they count.
- Other members appear only in the CLI output: `version`, the identity
  members the cache checks (`source_mtime_ns`, `source_inode`,
  `source_device`, `source_ctime_ns`, `source_fingerprint`) and their
  readable copies (`source_modified_at`, `source_changed_at`), `is_text`, `permissions`,
  `owner`, `index_step_bytes`, the seekable-zstd `frame_count`,
  `frame_size_target` and `frames`, and the `prefix_*` members.

The other members have the same name and value in both.

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
| `400 Bad Request` | Below size threshold; `analyze` conflicts with cache reuse; a directory; a file that is not text (`not a text file: …: <path>`) |
| `403 Forbidden` | Path outside `--search-root`; a file the server may not read (`Permission denied: <path>`) |
| `404 Not Found` | File doesn't exist |
| `409 Conflict` | A task for the same path is already running — an index or a compress task |

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
  "detail":  "Indexing already in progress for /var/log/audit-2026-03.log (task: 0b6c1d9e-1f4a-4c2e-9d55-7a3e8f20c4b1)",
  "task_id": "0b6c1d9e-1f4a-4c2e-9d55-7a3e8f20c4b1"
}
```

Status: `409`. Poll the running task, `GET /v1/tasks/{task_id}` with the
`task_id` of the body — duplicate POSTs don't start multiple builds.
One task holds a path whatever its operation, so a running compress of
the file also refuses an index build, and so does a running compress
that writes the file as its output; the sentence then starts with
`Compression already in progress` and `task_id` is the compress task's.

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
- `force=false` + valid cache + `analyze=true` + cache lacks analysis,
  or its analysis ran with another `analyze_window_lines` or another
  set of detectors (one added, removed or at another version) → rebuild
- `force=true` → always rebuild

### Result shape

Once `status == "completed"`, the task's `result` field contains the
same JSON shape as `GET /v1/index` returns, plus the two fields below.
The OpenAPI document names it `IndexTaskResult`.

```json
{
  "success":    true,
  "index_path": "/home/you/.cache/rx/indexes/audit-2026-03.log_<hash>.json"
}
```

Its `cli_command` is the `rx index` command for the request: `force`
and `analyze` become `--force` and `--analyze`, a given `threshold` is
always written, `0` included (`--threshold=0`; without the flag, `rx
index` uses `RX_LARGE_FILE_MB`), and a non-zero `analyze_window_lines`
becomes `--analyze-window-lines=N`.

See [tasks](tasks.md) for the task polling contract.

## Performance notes

- `GET /v1/index` reads only the stored index (9.3 KB for a 465 MB log)
- `POST /v1/index` without `analyze`: one pass over the file; 142 ms
  for a 465 MB log in the page cache
- `POST /v1/index` with `analyze`: every line goes through every
  detector; 18.2 s for the same log
- An index is valid while the file keeps its size, mtime, inode, ctime
  and fingerprint (see [caching](../../concepts/caching.md#cache-invalidation));
  `touch -t ...` invalidates it and the next request rebuilds it

## See also

- [`rx index`](../../cli/line-index.md) — CLI equivalent
- [Tasks](tasks.md) — polling background operations
- [concepts/line-indexes](../../concepts/line-indexes.md) — what indexes contain
- [concepts/caching](../../concepts/caching.md) — cache layout and invalidation
