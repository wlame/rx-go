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
  "line_index": [
    [1,     0],
    [12500, 2097152],
    [25000, 4194304]
  ],
  "index_entries":   2187,
  "line_count":      3528914,
  "empty_line_count": 0,
  "line_ending":      "LF",
  "line_length": {
    "max": 2048, "avg": 164.2, "median": 151.0,
    "p95": 301.0, "p99": 412.0, "stddev": 61.7
  },
  "longest_line":     { "line_number": 1207733, "byte_offset": 198812004 },
  "compression_format":      null,
  "decompressed_size_bytes": null,
  "compression_ratio":       null,
  "time_summary": {
    "format":            "iso",
    "has_zone":          false,
    "first_ms":          1765350004574,
    "last_ms":           1765353604390,
    "timestamped_lines": 3402771,
    "backward_steps":    0,
    "max_backward_ms":   0
  },
  "journal":         null,
  "cli_command":     "rx index /var/log/audit-2026-03.log --info --json"
}
```

The OpenAPI document names this shape `IndexResponse`.

The `journal` member of a systemd journal's index (a journal whose
entry 3 is damaged and whose entry list breaks off after entry 4):

```json
"journal": {
  "entries":             12,
  "shown":               3,
  "without_message":     0,
  "left_out":            1,
  "not_reached":         8,
  "first_damaged_entry": 3,
  "damage_reason":       "the ENTRY object at offset 1160 has size 0",
  "state":               "archived",
  "compact":             false,
  "seqnum_id":           "5e910d000102030405060708090a0b0c",
  "head_seqnum":         1,
  "tail_seqnum":         12
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
| `line_index` | array | Sparse checkpoints, each an array (`LineIndexEntry`): `[line_number, byte_offset]`, or `[line_number, byte_offset, frame_index]` in a seekable-zstd index, where the third element is the 0-based frame holding the line |
| `index_entries` | int | `len(line_index)` |
| `line_count` | int64 \| null | Total lines |
| `empty_line_count`, `line_ending` | int64, string \| null | Lines with no character other than white space; `LF`, `CRLF`, `CR` or `mixed`. `null` for a seekable zstd file |
| `line_length` | object \| null | `{max, avg, median, p95, p99, stddev}`; `null` for a seekable zstd file |
| `longest_line` | object \| null | `{line_number, byte_offset}` of the longest line; `null` for a seekable zstd file |
| `compression_format` | string \| null | For compressed inputs only |
| `time_summary` | object \| null | The timestamps of the file's lines, from the index's [time section](../../concepts/line-indexes.md#the-time-section): `format` (`iso`, `clf`, `ctime`, `syslog`, `slash`, `dotted` or `epoch`), `has_zone`, `first_ms` and `last_ms` (the first and the last timestamped line, `null` when no line carries one), `timestamped_lines`, `backward_steps` and `max_backward_ms`. Values are milliseconds since the Unix epoch: UTC instants when `has_zone` is true, the wall-clock time as written, read as if it were UTC, when it is false. `null` when no timestamp format is recognized in the first mebibyte of the text. The per-checkpoint `max_before` array and the `max` line of the stored index are not included; `rx index --json` gives them. Since contract 1.5 |
| `journal` | object \| null | For a systemd journal, what the build found reading its text: `entries` (the entries the header counts), `shown` (the entries in the text), `without_message` (entries with no `MESSAGE` field, which journalctl does not show either), `left_out` (damaged entries left out of the text), `not_reached` (the entries after the place where the entry list breaks off), with `entries` = `shown` + `without_message` + `left_out` + `not_reached`; `first_damaged_entry` (the 1-based number, in the order of the entry list, of the first entry left out or not reached) and `damage_reason`, both `null` when nothing was damaged; and from the header `state` (`online`, `offline` or `archived`), `compact`, `seqnum_id` (32 lowercase hex digits) and `head_seqnum` and `tail_seqnum`. A damaged journal is indexed all the same: its text, and every line number, is a fixed function of its bytes. `null` for a file that is not a journal. A `POST /v1/index` task's result carries it too. Since contract 2.1 |
| `cli_command` | string | Equivalent CLI command; see below |

### The equivalent CLI command

`cli_command` is `rx index PATH --info --json`: the command that reads
the same stored index without building one. It prints the whole stored
index (`UnifiedFileIndex`, 41 members), of which this answer is a
projection:

- `path` and `size_bytes` are `source_path` and `source_size_bytes`
  there.
- `line_length` and `longest_line` group the flat `line_length_*`
  members there.
- `index_entries` is counted here; the CLI prints the `line_index`
  array it counts.
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

Build a line index as a background task: of one file, or of every file
under a directory.

### Request

```text
POST /v1/index
Content-Type: application/json
```

### Request body

| Field | Type | Required | Default | Description |
|---|---|:-:|---|---|
| `path` | string | yes | — | A file, or a directory: every file under it is indexed ([Tasks over a directory](tasks.md#tasks-over-a-directory)) |
| `force` | bool | no | `false` | Rebuild even if a valid cache exists; under a directory, for every file |
| `threshold` | int \| null | no | `RX_LARGE_FILE_MB` (default 50) | Min file size in MB; under a directory, a smaller file is skipped with the reason |

The body allows no other member. `analyze` and `analyze_window_lines`
were removed in contract 2.0 with the index analysis, and a body that
holds either answers `422`, as any member the schema does not declare
does:

```json
{ "detail": "validation failed; unexpected property (body.analyze: …)" }
```

A file's findings come from [`POST /v1/findings`](findings.md) now.

### Response — 200 OK

```json
{
  "task_id":     "6e9b0cb4-7d86-4a56-9c56-3b5a19e67d53",
  "status":      "queued",
  "message":     "Indexing task started for /var/log/audit-2026-03.log",
  "path":        "/var/log/audit-2026-03.log",
  "started_at":  "2026-04-18T14:23:45.123456Z",
  "cli_command": "rx index /var/log/audit-2026-03.log"
}
```

`cli_command` is the `rx index` command that does what the task does,
the same in every status of the task and in its result.

### A directory

Given a directory, the task indexes every file the file tree lists
under it, recursively, one at a time, as `rx index DIR --recursive`
does, which is its `cli_command`
(`rx index /var/log/app --recursive --threshold=0`). A file below the
threshold, one whose text is not text and one whose index is current
(unless `force`) is skipped; one whose build fails is recorded and the
task goes on. The status has `files_done` and `files_total`, the
result is a `FolderTaskResult`, and `DELETE /v1/tasks/{task_id}`
cancels the task: the build of the current file stops at its next read
and stores nothing, and no later file is built; see
[Tasks over a directory](tasks.md#tasks-over-a-directory). A second
`POST` for the directory answers `409` while its task runs, and also
while a cancelled one is `cancelling` (its worker has not stopped).

### Status codes

| Code | When |
|---:|---|
| `200 OK` | Task queued. Poll `GET /v1/tasks/{task_id}` |
| `400 Bad Request` | A file below the size threshold; a file that is not text (`not a text file: …: <path>`) |
| `403 Forbidden` | Path outside `--search-root`; a file the server may not read (`Permission denied: <path>`) |
| `404 Not Found` | File doesn't exist |
| `409 Conflict` | A task for the same path is already running — an index or a compress task, or the index task of the same directory, running or `cancelling` |
| `422 Unprocessable Entity` | The body is not the schema's: a missing `path`, a member it does not declare (`analyze`, `analyze_window_lines`) |

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

### Cache reuse behavior

- `force=false` + valid cache exists → task completes instantly with
  the cached data
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
becomes `--force`, and a given `threshold` is always written, `0`
included (`--threshold=0`; without the flag, `rx index` uses
`RX_LARGE_FILE_MB`).

See [tasks](tasks.md) for the task polling contract.

## Performance notes

- `GET /v1/index` reads only the stored index (15.8 KB for a 465 MB log)
- `POST /v1/index`: one pass over the file; 120 ms for a 465 MB log in
  the page cache
- An index is valid while the file keeps its size, mtime, inode, ctime
  and fingerprint (see [caching](../../concepts/caching.md#cache-invalidation));
  `touch -t ...` invalidates it and the next request rebuilds it

## See also

- [`rx index`](../../cli/line-index.md) — CLI equivalent
- [Tasks](tasks.md) — polling background operations
- [concepts/line-indexes](../../concepts/line-indexes.md) — what indexes contain
- [concepts/caching](../../concepts/caching.md) — cache layout and invalidation
