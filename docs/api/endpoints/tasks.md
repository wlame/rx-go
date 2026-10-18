# `GET /v1/tasks/{task_id}`, `DELETE /v1/tasks/{task_id}`

Poll the status of a background task created by `POST /v1/index`,
`POST /v1/compress`, `POST /v1/logs/index` or `POST /v1/findings`, or of
the index build a `GET /v1/samples` request answered `202` for; cancel a
findings scan or a task over a directory.

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
  "files_done":  null,
  "files_total": null,
  "cli_command": "rx index /var/log/audit-2026-03.log",
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
  "files_done":  null,
  "files_total": null,
  "cli_command": "rx index /var/log/audit-2026-03.log",
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
  "files_done":  null,
  "files_total": null,
  "cli_command": "rx index /var/log/audit-2026-03.log",
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
      [1,     0],
      [12500, 2097152]
    ],
    "empty_line_count": 0,
    "line_ending": "LF",
    "line_length": {
      "max": 2048, "avg": 164.2, "median": 151.0,
      "p95": 301.0, "p99": 412.0, "stddev": 61.7
    },
    "longest_line": { "line_number": 1207733, "byte_offset": 198812004 },
    "compression_format": null,
    "decompressed_size_bytes": null,
    "compression_ratio": null,
    "time_summary": {
      "format":            "iso",
      "has_zone":          false,
      "first_ms":          1765350004574,
      "last_ms":           1765353604390,
      "timestamped_lines": 3402771,
      "backward_steps":    0,
      "max_backward_ms":   0
    },
    "journal": null,
    "index_path": "/home/you/.cache/rx/indexes/audit-2026-03.log_<hash>.json",
    "cli_command": "rx index /var/log/audit-2026-03.log"
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
  "files_done":  null,
  "files_total": null,
  "cli_command": "rx index /var/log/audit-2026-03.log",
  "result":      null
}
```

## Response fields

| Field | Type | Description |
|---|---|---|
| `task_id` | string | UUID v4 of the task |
| `status` | string | `"queued"`, `"running"`, `"completed"`, `"failed"`, `"cancelled"` (a findings task or a task over a directory stopped by [`DELETE`](#delete-v1taskstask_id)), or `"cancelling"` (a task over a directory that `DELETE` cancelled and whose worker has not stopped yet; not an end, keep polling) |
| `path` | string | The validated absolute path the task operates on; for a `chain_index` task, the chain's handle |
| `operation` | string | `"index"`, `"compress"`, `"chain_index"` (the index task of a log chain) or `"findings"` (a signature scan, [`POST /v1/findings`](findings.md#post-v1findings)) |
| `started_at` | string | ISO 8601 UTC — when the task was created |
| `completed_at` | string \| null | Set once the task reaches `completed`, `failed` or `cancelled` |
| `error` | string \| null | Populated only when `status == "failed"` |
| `progress` | number \| null | Share of an index task's input read so far, from 0 to 1 (a stream-compressed or plain file counts its bytes on disk, a seekable zstd file its decompressed text); for a `chain_index` task, the share of its parts done, a part whose build runs counted by that build's progress. `null` for a compress task, for an index task that reused a stored index, and before the build has started |
| `bytes_read` | int64 \| null | How far into the file a findings task has read so far: the highest byte of the file, as stored on disk, it has read, not the sum of its reads. `null` for the other operations, and for a findings task that has not started reading (one waiting in the scan queue, or one the findings cache answers whole) |
| `bytes_total` | int64 \| null | How far into the file a findings task reads to reach the end of its text: the size of the file as stored on disk, except for a systemd journal, where it is the end of the journal's last object (a live journal is preallocated, and the zeros after its objects are never read); `null` when `bytes_read` is. A findings task's `progress` is `bytes_read / bytes_total` |
| `files_done` | int64 \| null | A task over a directory: the entries of `files_total` it has ended (indexed or scanned, skipped, or failed). `null` for a task over one file, and while the task lists the directory |
| `files_total` | int64 \| null | A task over a directory: the entries its walk listed. `null` when `files_done` is |
| `cli_command` | string | The `rx` command that does what the task does: `rx index PATH` for an index task of `POST /v1/index`, `rx scan PATH --signatures=SEL` for a findings task, with `--recursive` for a directory, `--hidden` when the server serves hidden entries (`rx serve --hidden`, `RX_HIDDEN`), `--force` when the request forced, `--mask-secrets` when the server masks secrets, and `--threshold=N` as the index request gave it; `rx compress PATH` for a compress task (with `--output=…`, `--force`, `--build-index=false`, and a frame size or level that differs from the default, as the request gave them); `rx logs index HANDLE` for a chain_index task (with `--force`). `""` for the index builds `GET /v1/samples` and the log chains start |
| `result` | object \| null | Populated only when `status == "completed"`, `null` before; `IndexTaskResult`, `CompressTaskResult`, `ChainIndexTaskResult` or `FindingsResponse` by operation, `FolderTaskResult` for a task over a directory |

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
- an `index` or `findings` task over a directory (`files_total` is set):
  `FolderTaskResult`, see [Tasks over a directory](#tasks-over-a-directory)

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Task exists; current state returned |
| `401 Unauthorized` | The server sets `RX_API_TOKEN` and the request has no valid `Authorization: Bearer` header ([Authentication](../index.md#authentication)) |
| `404 Not Found` | Task ID unknown or already swept |

## Status transitions

```text
queued → running → (completed | failed)
queued | running → cancelled                  (a findings task over one file, by DELETE)
queued | running → cancelling → cancelled     (a task over a directory, by DELETE, then its worker's stop)
```

- `queued` is the initial state right after the POST returns; a
  findings task stays `queued` while it waits for the scan queue
  (`RX_MAX_SIGNATURE_SCANS`) or for a scan of the same file
- `running` once the background goroutine calls `MarkRunning`; a
  findings task, once it reads the file (a findings task the cache
  answers whole goes from `queued` to `completed`)
- `completed` on success with a populated `result`
- `failed` on error with a populated `error` string
- `cancelling` when `DELETE` cancelled a task over a directory whose
  worker has not stopped yet: it still holds the directory
- `cancelled` when `DELETE` stopped a findings task, or once the worker
  of a cancelling task has stopped: no `error`, no `result`

Transitions are monotonic — a task never moves back to an earlier
state.

## Tasks over a directory

`POST /v1/index` and `POST /v1/findings` given a directory start one
task that takes every file the [file tree](tree.md) lists under it,
recursively, one at a time, in the walk's order: the files
`rx index DIR --recursive` and `rx scan DIR --recursive` take, in the
same order (the task's `cli_command`). Each entry ends in one of three
ways:

- **done**: indexed, or scanned for the selection;
- **skipped**: its index, or the findings of every selected signature,
  are current (counted in `current` too; `force` passes none over this
  way), or its text is not text, it is below the index threshold, rx
  refuses to decompress it, the walk refused it (a link out of the
  roots or into a hidden entry) or it cannot be read;
- **failed**: its build or scan failed. The error is recorded and the
  task goes on with the next file.

While it runs the status has `files_done` of `files_total`, and
`progress` is the files ended plus the share of the current file read,
over `files_total`. A findings task's `bytes_read` and `bytes_total`
follow the file it reads (`0` between files); an index task's are
`null`. The files of a findings task go through the scan queue one at a
time, so `RX_MAX_SIGNATURE_SCANS` still bounds the files read at once.

```json
{
  "task_id":     "9f2c4f7e-0b1d-4d8e-a6a1-2b9c1e4d5f60",
  "status":      "running",
  "path":        "/var/log/app",
  "operation":   "findings",
  "started_at":  "2026-10-12T09:14:02.120044Z",
  "completed_at": null,
  "error":       null,
  "progress":    0.0572,
  "bytes_read":  1048576,
  "bytes_total": 4194304,
  "files_done":  12,
  "files_total": 214,
  "cli_command": "rx scan /var/log/app --recursive --signatures=default",
  "result":      null
}
```

The result, `FolderTaskResult`:

```json
{
  "path":          "/var/log/app",
  "files_total":   214,
  "done":          201,
  "skipped":       12,
  "current":       9,
  "failed":        1,
  "findings":      3812,
  "skipped_files": [
    {"path": "/var/log/app/core.bin", "reason": "not a text file: a NUL byte in its first 8 KiB"}
  ],
  "failed_files":  [
    {"path": "/var/log/app/worker.log.3.gz", "error": "…"}
  ],
  "cli_command":   "rx scan /var/log/app --recursive --signatures=default"
}
```

`files_total` = `done` + `skipped` + `failed`. `skipped_files` lists the
entries skipped for another reason than being current, and
`failed_files` the failed ones, each the first 1,000 in the walk's
order; the counts are exact. `findings` is the findings of every file
scanned or current, `null` for an index task.

A second `POST` for the same directory while its task runs returns that
task (findings) or answers `409` naming it (index), as for a file.
`DELETE` cancels the task: the file it is on stops at its next read (an
index build as a scan) and writes nothing, and no later file is read;
the files ended before the cancel keep their index or findings. The
task shows `cancelling` until its worker has stopped, and keeps the
directory meanwhile: a `POST` for it answers `409` (it is not joined),
so a `POST`+`DELETE` loop never runs two workers for one directory.
Then it ends `cancelled`. A task over a directory can be cancelled
until it ends; a second `DELETE` while it is cancelling answers `200`
with the same status.

## `DELETE /v1/tasks/{task_id}`

Cancels a queued or running findings task ([`POST /v1/findings`](findings.md#post-v1findings)):
the scan stops at its next read and caches nothing, and the task ends at
once with status `cancelled`, which the answer shows. A client that
joined the same scan sees it `cancelled` and may start it again; a new
`POST` for the file starts a new task at once. It cancels a task over a
directory, an index or a findings one, as
[Tasks over a directory](#tasks-over-a-directory) says: that answer
shows `cancelling`, and the task ends `cancelled` once its worker has
stopped.

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
  "files_done":  null,
  "files_total": null,
  "cli_command": "rx scan /var/log/app.log --signatures=default",
  "result":      null
}
```

| Code | When |
|---:|---|
| `200 OK` | The task is cancelled (`cancelled`), or, for a task over a directory, is stopping (`cancelling`); its status is returned |
| `401 Unauthorized` | The server sets `RX_API_TOKEN` and the request has no valid `Authorization: Bearer` header ([Authentication](../index.md#authentication)) |
| `404 Not Found` | Task ID unknown or already swept |
| `409 Conflict` | The task is neither a findings task nor a task over a directory (an index task over one file, a compress or a chain_index task cannot be cancelled), it has already ended (completed, failed or cancelled), or it is a findings task over one file whose scan has finished reading and whose answer is being written: from that point a cancel is refused, so such a task never ends `cancelled` with findings written. The body is `{"detail", "task_id"}` |

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

!!! note "Only a findings task and a task over a directory can be cancelled"
    [`DELETE /v1/tasks/{task_id}`](#delete-v1taskstask_id) stops a
    signature scan and a task over a directory. An index task over one
    file, a compress or a `chain_index` task runs to completion or
    failure; to stop one, restart the server.

## See also

- [`POST /v1/index`](line-index.md#post-v1index) — create an indexing task
- [`POST /v1/compress`](compress.md) — create a compression task
- [`POST /v1/findings`](findings.md#post-v1findings) — create a signature scan task
- [API conventions](../conventions.md) — request IDs, time format
- [concepts/caching](../../concepts/caching.md) — cache layout
