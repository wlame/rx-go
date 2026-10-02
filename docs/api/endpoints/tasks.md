# `GET /v1/tasks/{task_id}`, `DELETE /v1/tasks/{task_id}`

Poll the status of a background task created by `POST /v1/index`,
`POST /v1/compress`, `POST /v1/logs/index` or `POST /v1/findings`, or of
the index build a `GET /v1/samples` request answered `202` for; cancel a
findings scan.

## Purpose

Return the current state (queued, running, completed, failed,
cancelled) of a detached task, along with its result on completion or
error message on failure.

## Request

```text
GET /v1/tasks/{task_id}
```

### Path parameters

| Parameter | Type | Description |
|---|---|---|
| `task_id` | string | UUID v4 returned from the task-creation endpoint |

## Response — 200 OK

### Queued

```json
{
  "task_id":     "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":      "queued",
  "path":        "/var/log/audit-2026-03.log",
  "operation":   "index",
  "started_at":  "2026-04-18T14:23:45.123456Z",
  "completed_at": null,
  "error":       null,
  "progress":    null,
  "bytes_read":  null,
  "bytes_total": null,
  "result":      null
}
```

### Running

```json
{
  "task_id":     "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":      "running",
  "path":        "/var/log/audit-2026-03.log",
  "operation":   "index",
  "started_at":  "2026-04-18T14:23:45.123456Z",
  "completed_at": null,
  "error":       null,
  "progress":    0.42,
  "bytes_read":  null,
  "bytes_total": null,
  "result":      null
}
```

### Completed

```json
{
  "task_id":     "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":      "completed",
  "path":        "/var/log/audit-2026-03.log",
  "operation":   "index",
  "started_at":  "2026-04-18T14:23:45.123456Z",
  "completed_at": "2026-04-18T14:23:48.456789Z",
  "error":       null,
  "progress":    1,
  "bytes_read":  null,
  "bytes_total": null,
  "result": {
    "success":    true,
    "path":       "/var/log/audit-2026-03.log",
    "line_count": 3528914,
    "index_entries": 2187,
    "file_type":  "text",
    "size_bytes": 582137856,
    "created_at": "2026-04-18T14:23:48.456789Z",
    "build_time_seconds": 2.456,
    "line_index": [
      {"line_number": 1, "byte_offset": 0},
      {"line_number": 12500, "byte_offset": 2097152}
    ],
    "index_path": "/home/you/.cache/rx/indexes/audit-2026-03.log_<hash>.json",
    "cli_command": "..."
  }
}
```

### Failed

```json
{
  "task_id":     "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":      "failed",
  "path":        "/var/log/audit-2026-03.log",
  "operation":   "index",
  "started_at":  "2026-04-18T14:23:45.123456Z",
  "completed_at": "2026-04-18T14:23:46.789012Z",
  "error":       "build index: permission denied reading /var/log/audit-2026-03.log",
  "progress":    null,
  "bytes_read":  null,
  "bytes_total": null,
  "result":      null
}
```

## Response fields

| Field | Type | Description |
|---|---|---|
| `task_id` | string | UUID v4 of the task |
| `status` | string | `"queued"`, `"running"`, `"completed"`, `"failed"`, or `"cancelled"` (a findings task stopped by [`DELETE`](#delete-v1taskstask_id)) |
| `path` | string | The validated absolute path the task operates on; for a `chain_index` task, the chain's handle |
| `operation` | string | `"index"`, `"compress"`, `"chain_index"` (the index task of a log chain) or `"findings"` (a signature scan, [`POST /v1/findings`](findings.md#post-v1findings)) |
| `started_at` | string | ISO 8601 UTC — when the task was created |
| `completed_at` | string \| null | Set once the task reaches `completed`, `failed` or `cancelled` |
| `error` | string \| null | Populated only when `status == "failed"` |
| `progress` | number \| null | Share of an index task's input read so far, from 0 to 1 (a stream-compressed or plain file counts its bytes on disk, a seekable zstd file its decompressed text); for a `chain_index` task, the share of its parts done, a part whose build runs counted by that build's progress. `null` for a compress task, for an index task that reused a stored index, and before the build has started |
| `bytes_read` | int64 \| null | Bytes of the file a findings task has read so far, as stored on disk. `null` for the other operations, and for a findings task that has not started reading (one waiting in the scan queue, or one the findings cache answers whole) |
| `bytes_total` | int64 \| null | The size of the file a findings task reads, as stored on disk; `null` when `bytes_read` is. A findings task's `progress` is `bytes_read / bytes_total` |
| `result` | object \| null | Populated only when `status == "completed"`, `null` before; `IndexTaskResult`, `CompressTaskResult`, `ChainIndexTaskResult` or `FindingsResponse` by operation |

### Result shape by operation

- `operation == "index"`: `IndexTaskResult`, the same shape as
  [`GET /v1/index`](line-index.md) plus a `success` bool and an
  `index_path` string
- `operation == "compress"`: `CompressTaskResult`, see the
  [compress task result](compress.md#task-result-shape)
- `operation == "chain_index"`: `ChainIndexTaskResult`, see
  [`POST /v1/logs/index`](logs-index.md#task-result)
- `operation == "findings"`: `FindingsResponse`, the answer
  [`GET /v1/findings`](findings.md) gives for the scanned selection
  without filters: the summary and the first page of 1,000 findings

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Task exists; current state returned |
| `404 Not Found` | Task ID unknown or already swept |

## Status transitions

```text
queued → running → (completed | failed)
queued | running → cancelled        (a findings task, by DELETE)
```

- `queued` is the initial state right after the POST returns; a
  findings task stays `queued` while it waits for the scan queue
  (`RX_MAX_SIGNATURE_SCANS`) or for a scan of the same file
- `running` once the background goroutine calls `MarkRunning`; a
  findings task, once it reads the file (a findings task the cache
  answers whole goes from `queued` to `completed`)
- `completed` on success with a populated `result`
- `failed` on error with a populated `error` string
- `cancelled` when `DELETE` stopped a findings task: no `error`, no
  `result`

Transitions are monotonic — a task never moves back to an earlier
state.

## `DELETE /v1/tasks/{task_id}`

Cancels a queued or running findings task ([`POST /v1/findings`](findings.md#post-v1findings)):
the scan stops at its next read and caches nothing, and the task ends at
once with status `cancelled`, which the answer shows. A client that
joined the same scan sees it `cancelled` and may start it again; a new
`POST` for the file starts a new task at once.

```bash
curl -s -X DELETE 'http://127.0.0.1:7777/v1/tasks/440cb10b-c583-42ca-9283-840f514c8bd4'
```

```json
{
  "task_id":     "440cb10b-c583-42ca-9283-840f514c8bd4",
  "status":      "cancelled",
  "path":        "/var/log/app.log",
  "operation":   "findings",
  "started_at":  "2026-10-11T08:49:44.199639Z",
  "completed_at": "2026-10-11T08:49:45.012345Z",
  "error":       null,
  "progress":    0.41,
  "bytes_read":  52428800,
  "bytes_total": 127926272,
  "result":      null
}
```

| Code | When |
|---:|---|
| `200 OK` | The task is cancelled; its status is returned |
| `404 Not Found` | Task ID unknown or already swept |
| `409 Conflict` | The task is not a findings task (an index, compress or chain_index task cannot be cancelled), it has already ended (completed, failed or cancelled), or its scan has finished reading and its answer is being written: from that point a cancel is refused, so a task never ends `cancelled` with findings written. The body is `{"detail", "task_id"}` |

## Sweeper behavior

Finished tasks (`completed`, `failed` and `cancelled`) are swept from memory by
a background goroutine that runs every 5 minutes. A task is swept when
its `completed_at` is older than `RX_TASK_TTL_MINUTES` (default 60).

The table also keeps at most 256 finished tasks: starting one past that
drops the oldest finished ones first, before their TTL. The part builds
of a log chain's index task (`chain_index`) are counted apart, at most
256 finished ones among themselves, so the parts of a large chain drop
only older part builds, never another task. A queued or running task is
never dropped and does not count toward the 256, so however many tasks
run or wait at once, a finished task stays until 256 later ones of its
kind have finished, or its TTL has passed.

After a task is swept, `GET /v1/tasks/{task_id}` returns `404`. If you
need to retain results longer:

- Set `RX_TASK_TTL_MINUTES` higher
- Persist the result somewhere else after polling
- Use the webhook `on_complete` pattern (not currently wired for
  index/compress tasks — only trace)

## Examples

### Basic poll

```bash
curl -s 'http://127.0.0.1:7777/v1/tasks/6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53' \
    | jq '.status'
```

### Poll until done

```bash
poll_until_done() {
    local task_id="$1"
    while :; do
        resp=$(curl -s "http://127.0.0.1:7777/v1/tasks/$task_id")
        status=$(echo "$resp" | jq -r '.status')
        case "$status" in
            completed)        echo "$resp"; return 0 ;;
            failed|cancelled) echo "$resp"; return 1 ;;
            *)                sleep 1 ;;
        esac
    done
}

task_id=$(curl -sXPOST 'http://127.0.0.1:7777/v1/index' \
    -H 'Content-Type: application/json' \
    -d '{"path":"/var/log/audit-2026-03.log"}' | jq -r '.task_id')

poll_until_done "$task_id" | jq '.result | {lines: .line_count}'
```

### Compute elapsed

```bash
curl -s "http://127.0.0.1:7777/v1/tasks/$task_id" | jq '
  (.completed_at // now | fromdateiso8601) - (.started_at | fromdateiso8601)
'
```

## Error examples

### Unknown task

```json
{ "detail": "Task not found: 00000000-0000-0000-0000-000000000000" }
```

Status: `404`. Either the ID is wrong or the task was swept. The
default retention is 60 minutes after completion.

## Tips and gotchas

!!! tip "Poll interval"
    A 1-2 second poll interval is usually appropriate. Short enough
    to feel responsive for sub-10-second tasks, long enough to avoid
    hammering the server.

!!! warning "Don't rely on long-lived task IDs"
    Task IDs are valid for at most `RX_TASK_TTL_MINUTES` after
    completion. If you need a durable record of what ran, log the
    `result` body to your own store as soon as polling returns.

!!! note "Only a findings task can be cancelled"
    [`DELETE /v1/tasks/{task_id}`](#delete-v1taskstask_id) stops a
    signature scan. An index, compress or `chain_index` task runs to
    completion or failure; to stop one, restart the server.

## See also

- [`POST /v1/index`](line-index.md#post-v1index) — create an indexing task
- [`POST /v1/compress`](compress.md) — create a compression task
- [`POST /v1/findings`](findings.md#post-v1findings) — create a signature scan task
- [API conventions](../conventions.md) — request IDs, time format
- [concepts/caching](../../concepts/caching.md) — cache layout
