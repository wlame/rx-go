# `POST /v1/compress`

Encode a file as seekable zstd. Runs as a background task. HTTP
equivalent of [`rx compress`](../../cli/compress.md).

## Purpose

Compress a file to seekable zstd (random-access decompression) in the
background. Returns a task ID immediately; the actual encoding runs on
a detached goroutine. Poll `GET /v1/tasks/{task_id}` for progress.

## Request

```text
POST /v1/compress
Content-Type: application/json
```

### Request body

| Field | Type | Required | Default | Description |
|---|---|:-:|---|---|
| `input_path` | string | yes | — | Source file path |
| `output_path` | string \| null | no | `<input_path>.zst` | Output file path |
| `frame_size` | string | no | `"4M"` | Target frame size (e.g. `4M`, `16MB`, `1048576`) |
| `compression_level` | int | no | `3` | zstd level: 1-22 |
| `build_index` | bool | no | `false` | Register for line indexing after compression |
| `force` | bool | no | `false` | Overwrite existing output |

### Frame size syntax

- Plain integer: bytes (`1048576`)
- With suffix: `B`, `K`/`KB`, `M`/`MB`, `G`/`GB` (case-insensitive)
- Fractions allowed: `1.5M` = 1572864 bytes

### Compression level

- `1` → fastest, largest output
- `3` → default, balanced
- `19` → slow, much smaller output
- `22` → ultra, rarely worth the encode time

## Response — 200 OK

```json
{
  "task_id":    "a7c1f2e8-3b45-4a67-8d9c-e4ba1f5c9876",
  "status":     "queued",
  "message":    "Compression task started for /var/log/audit-2026-03.log",
  "path":       "/var/log/audit-2026-03.log",
  "started_at": "2026-04-18T14:25:11.872345Z"
}
```

### Response fields

| Field | Type | Description |
|---|---|---|
| `task_id` | string | UUID v4 — poll `GET /v1/tasks/{task_id}` |
| `status` | string | Always `"queued"` immediately after POST |
| `message` | string | Human-readable status text |
| `path` | string | The validated absolute input path |
| `started_at` | string | ISO 8601 UTC timestamp |

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Task queued |
| `400 Bad Request` | Output file exists and `force=false`; level out of range; bad `frame_size` |
| `403 Forbidden` | Input path outside `--search-root` |
| `404 Not Found` | Input file doesn't exist |
| `409 Conflict` | Another compress task for the same input is already running |

## Examples

### Default encoding

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/compress' \
    -H 'Content-Type: application/json' \
    -d '{"input_path":"/var/log/audit-2026-03.log"}' \
    | jq '.task_id'
```

### Custom output and level

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/compress' \
    -H 'Content-Type: application/json' \
    -d '{
      "input_path": "/var/log/audit-2026-03.log",
      "output_path": "/backup/audit-2026-03.zst",
      "compression_level": 9,
      "frame_size": "1M"
    }'
```

### Overwrite existing output

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/compress' \
    -H 'Content-Type: application/json' \
    -d '{
      "input_path": "/var/log/audit-2026-03.log",
      "force": true
    }'
```

### Full flow — kick off, poll, consume

```bash
set -eu
base=http://127.0.0.1:7777

# POST the task.
task=$(curl -sXPOST "$base/v1/compress" \
    -H 'Content-Type: application/json' \
    -d '{"input_path":"/var/log/audit-2026-03.log","compression_level":9}')
task_id=$(echo "$task" | jq -r '.task_id')

# Poll at 2-second intervals until done.
while :; do
    resp=$(curl -s "$base/v1/tasks/$task_id")
    status=$(echo "$resp" | jq -r '.status')
    case "$status" in
        completed) echo "$resp" | jq '.result' ; break ;;
        failed)    echo "$resp" | jq '.error'  ; exit 1 ;;
        *)         sleep 2 ;;
    esac
done
```

## Task result shape

When the task completes, its `result` field contains:

```json
{
  "success":           true,
  "input_path":        "/var/log/audit-2026-03.log",
  "output_path":       "/var/log/audit-2026-03.log.zst",
  "compressed_size":   40231680,
  "decompressed_size": 582137856,
  "compression_ratio": 14.47,
  "frame_count":       139,
  "total_lines":       null,
  "index_built":       false,
  "time_seconds":      12.34,
  "cli_command":       "rx compress --input-path=/var/log/audit-2026-03.log --output-path=/var/log/audit-2026-03.log.zst --frame-size=4M --compression-level=3"
}
```

### Result fields

| Field | Type | Description |
|---|---|---|
| `success` | bool | `true` on successful encode |
| `input_path` | string | Source file |
| `output_path` | string | Destination `.zst` file |
| `compressed_size` | int64 | Bytes on disk after encoding |
| `decompressed_size` | int64 | Original file size |
| `compression_ratio` | number | `decompressed / compressed` (≥ 1.0) |
| `frame_count` | int | Number of independent zstd frames |
| `total_lines` | int64 \| null | Populated when `build_index=true` |
| `index_built` | bool | Whether a line index was registered |
| `time_seconds` | number | Wall-clock encode time |
| `cli_command` | string | Equivalent CLI invocation |

## Error examples

### Output exists without force

```json
{ "detail": "Output file already exists: /var/log/audit-2026-03.log.zst" }
```

Status: `400`. Set `"force": true` or choose a different `output_path`.

### Invalid compression level

```json
{ "detail": "compression_level must be 1..22, got 25" }
```

Status: `400`. Use a valid zstd level.

### Invalid frame size

```json
{ "detail": "frame_size \"4MX\": invalid syntax" }
```

Status: `400`. See [frame size syntax](#frame-size-syntax).

### Duplicate task

```json
{ "detail": "Compression already in progress for /var/log/audit-2026-03.log (task: abc-123-def)" }
```

Status: `409`. Poll the existing task ID.

## Performance notes

- Encoding is currently single-worker in the HTTP path — full
  `--workers=N` parallelism is CLI-only in this release
- Memory overhead: one frame buffer (default 4 MiB) during encode
- Compression ratio is highly input-dependent — real-world logs
  typically hit 10×-30× at default level
- Higher levels (9, 19, 22) are slower with diminishing size returns;
  default level 3 is usually the sweet spot

## See also

- [`rx compress`](../../cli/compress.md) — CLI equivalent
- [Tasks](tasks.md) — polling contract
- [concepts/compression](../../concepts/compression.md) — seekable-zstd mechanics
- [`/v1/samples`](samples.md) — random-access reads on compressed files
