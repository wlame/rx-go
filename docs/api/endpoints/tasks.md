# `GET /v1/tasks/{task_id}`

Poll the status of a background task created by `POST /v1/index` or
`POST /v1/compress`.

## Purpose

Return the current state (queued, running, completed, failed) of a
detached task, along with its result on completion or error message on
failure.

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
  "result": {
    "success":    true,
    "path":       "/var/log/audit-2026-03.log",
    "line_count": 3528914,
    "index_entries": 2187,
    "file_type":  "text",
    "size_bytes": 582137856,
    "created_at": "2026-04-18T14:23:48.456789Z",
    "build_time_seconds": 2.456,
    "analysis_performed": false,
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
  "result":      null
}
```

## Response fields

| Field | Type | Description |
|---|---|---|
| `task_id` | string | UUID v4 of the task |
| `status` | string | `"queued"`, `"running"`, `"completed"`, or `"failed"` |
| `path` | string | The validated absolute path the task operates on |
| `operation` | string | `"index"` or `"compress"` |
| `started_at` | string | ISO 8601 UTC — when the task was created |
| `completed_at` | string \| null | Set once the task reaches `completed` or `failed` |
| `error` | string \| null | Populated only when `status == "failed"` |
| `result` | object \| null | Populated only when `status == "completed"`; shape depends on operation |

### Result shape by operation

- `operation == "index"`: same shape as [`GET /v1/index`](line-index.md) plus a
  `success` bool and `index_path` string
- `operation == "compress"`: see the [compress task result](compress.md#task-result-shape)

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Task exists; current state returned |
| `404 Not Found` | Task ID unknown or already swept |

## Status transitions

```text
queued → running → (completed | failed)
```

- `queued` is the initial state right after the POST returns
- `running` once the background goroutine calls `MarkRunning`
- `completed` on success with a populated `result`
- `failed` on error with a populated `error` string

Transitions are monotonic — a task never moves back to an earlier
state.

## Sweeper behavior

Finished tasks (both `completed` and `failed`) are swept from memory by
a background goroutine that runs every 5 minutes. A task is swept when
its `completed_at` is older than `RX_TASK_TTL_MINUTES` (default 60).

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
            completed) echo "$resp"; return 0 ;;
            failed)    echo "$resp"; return 1 ;;
            *)         sleep 1 ;;
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

!!! note "No cancel endpoint"
    Once a task starts, it runs to completion or failure. There's no
    cancel API in this release. If you need to kill a misbehaving
    task, restart the server.

## See also

- [`POST /v1/index`](line-index.md#post-v1index) — create an indexing task
- [`POST /v1/compress`](compress.md) — create a compression task
- [API conventions](../conventions.md) — request IDs, time format
- [concepts/caching](../../concepts/caching.md) — cache layout
