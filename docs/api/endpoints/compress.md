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
| `output_path` | string \| null | no | beside the input, the default name of `rx compress` | Output file path. Validated against `--search-root` like the input. The default name replaces a compression suffix by `.zst` (`app.log.gz` gives `app.log.zst`) and otherwise appends `.zst` (`app.log` gives `app.log.zst`) |
| `frame_size` | string | no | `"4M"` | Target frame size (e.g. `4M`, `16MB`, `1048576`) |
| `compression_level` | int | no | `3` | zstd level: 1-22 |
| `build_index` | bool | no | `true` | Build the line index of the compressed file after compressing it |
| `force` | bool | no | `false` | Overwrite existing output, and re-encode an input that is already seekable zstd |

Every default is the default of the matching
[`rx compress`](../../cli/compress.md) flag (`--output`, `--frame-size`,
`--level`, `--build-index`, `--force`), so a request that leaves a field
out compresses the way the CLI does without that flag.

### Compressed input

The task writes the input's text, exactly as
[`rx compress`](../../cli/compress.md#compressed-input) does: a gzip,
bzip2, xz or plain zstd input is decompressed on the fly, so the output
traces like the decompressed file, and `decompressed_size` in the
result is the size of that text. An input whose text is not text (a
compressed tar archive, a binary file, UTF-16), a seekable zstd input without `"force": true`, and an
output path that is the input file are refused with `400` before a
task is created. The last one includes a plain zstd input named
`app.log.zst` without `output_path`, whose default name is its own:
`<input>: the output path is the input file (set "output_path" to
another file)`, with or without `"force": true`.

The task writes the output the way `rx compress` does: to a hidden
temporary file in the output's directory, put under the output name
only once it is whole. Without `"force": true` a file that appears at
the output name while the task runs is left alone and the task fails
with `output file already exists: <path> (set "force": true to
overwrite)`; with it, the output replaces whatever holds the name (a
symbolic link there is replaced itself, never written through). A task
that fails leaves the output name as it was, so a forced task that
fails keeps the file it would have replaced. `compressed_size` is the
size of the file the task wrote.

### Frame size syntax

- Plain integer: bytes (`1048576`)
- With suffix: `B`, `K`/`KB`, `M`/`MB`, `G`/`GB` (case-insensitive)
- Fractions allowed: `1.5M` = 1572864 bytes

### Compression level

Any level from 1 to 22 is accepted, but the encoder has four settings:
`1` (fastest), `2`-`5` (default), `6`-`9` (better compression) and
`10`-`22` (best compression). Levels in one group write identical
files. See [compression levels](../../concepts/compression.md#compression-level)
for measured sizes and times.

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
| `400 Bad Request` | Output file exists and `force=false`; the input is not text, or seekable zstd and `force=false`; `output_path` is the input file; bad `frame_size`; body is not valid JSON |
| `403 Forbidden` | Input path or output path outside `--search-root` |
| `404 Not Found` | Input file doesn't exist |
| `409 Conflict` | A task that holds the input path or the output path is already running: a compress or an index task of the input, or a compress task writing the same output |
| `422 Unprocessable Entity` | Body fails the schema: `input_path` missing, a field of the wrong type, an unknown field, or `compression_level` outside 1-22 |

## Path sandbox

Both `input_path` and the effective output path are validated against
the configured `--search-root` directories before any filesystem access.
The effective output path is `output_path` when it is set, otherwise
the default name beside the input (`app.log.gz` gives `app.log.zst`,
`app.log` gives `app.log.zst`). A path that resolves outside every root — including
one that escapes through a symlink inside a root — is rejected with
`403 Forbidden`, and no file is created, truncated or removed. This holds
with `force=true` as well.

## Examples

### Default encoding

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/compress' \
    -H 'Content-Type: application/json' \
    -d '{"input_path":"/var/log/audit-2026-03.log"}' \
    | jq '.task_id'
```

### Custom output and level

The output path must be inside a `--search-root` as well, so this example
assumes the server was started with `--search-root=/var/log`.

```bash
curl -sXPOST 'http://127.0.0.1:7777/v1/compress' \
    -H 'Content-Type: application/json' \
    -d '{
      "input_path": "/var/log/audit-2026-03.log",
      "output_path": "/var/log/archive/audit-2026-03.zst",
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

When the task completes, its `result` field contains a
`CompressTaskResult`:

```json
{
  "success":           true,
  "input_path":        "/var/log/audit-2026-03.log",
  "output_path":       "/var/log/audit-2026-03.log.zst",
  "compressed_size":   40231680,
  "decompressed_size": 582137856,
  "compression_ratio": 14.47,
  "frame_count":       139,
  "total_lines":       2846193,
  "index_built":       true,
  "index_error":       null,
  "time_seconds":      12.34,
  "cli_command":       "rx compress /var/log/audit-2026-03.log"
}
```

### Result fields

| Field | Type | Description |
|---|---|---|
| `success` | bool | `true` on successful encode |
| `input_path` | string | Source file |
| `output_path` | string | Destination `.zst` file |
| `compressed_size` | int64 | Bytes on disk after encoding |
| `decompressed_size` | int64 | Size of the text the output holds: the input's size for a plain file, its decompressed size for a compressed one |
| `compression_ratio` | number | `decompressed / compressed`; below 1 when the text does not compress, such as random bytes |
| `frame_count` | int | Number of independent zstd frames |
| `total_lines` | int64 \| null | Line count from the index; `null` when no index was built |
| `index_built` | bool | Whether the line index was built and saved |
| `index_error` | string \| null | Why building the index failed; `null` when it was built or not asked for. The compressed file is usable either way, and `POST /v1/index` can build the index later |
| `time_seconds` | number | Wall-clock encode time |
| `cli_command` | string | Equivalent CLI invocation. `--output` appears when the request named an output, `--build-index=false` when it turned the index off, `--force` when it asked to overwrite; a value equal to the `rx compress` default is left out (see [conventions](../conventions.md#the-equivalent-cli-command)) |

## Error examples

### Output exists without force

```json
{ "detail": "Output file already exists: /var/log/audit-2026-03.log.zst" }
```

Status: `400`. Set `"force": true` or choose a different `output_path`.

### Input that is already seekable zstd

```json
{ "detail": "/var/log/audit-2026-03.zst: already a seekable zstd file (set \"force\": true to re-encode it)" }
```

Status: `400`. With `"force": true` the text is decompressed and
encoded again with the request's `frame_size` and `compression_level`.

### Invalid compression level

```json
{ "detail": "validation failed; expected number <= 22 (body.compression_level: 25)" }
```

Status: `422`. Use a valid zstd level.

### Invalid frame size

```json
{ "detail": "frame_size \"4MX\": invalid syntax" }
```

Status: `400`. See [frame size syntax](#frame-size-syntax).

### Duplicate task

```json
{
  "detail":  "Compression already in progress for /var/log/audit-2026-03.log (task: 0b6c1d9e-1f4a-4c2e-9d55-7a3e8f20c4b1)",
  "task_id": "0b6c1d9e-1f4a-4c2e-9d55-7a3e8f20c4b1"
}
```

Status: `409`. Poll `GET /v1/tasks/{task_id}` with the `task_id` of the
body; the sentence names the same task. One task holds a path whatever
its operation, so a running index build also refuses a compress of
the file; the sentence then starts with `Indexing already in progress`
and `task_id` is the index task's.

A compress task also holds its output path until it ends, so two
compressions into one output cannot run at once. `app.log` and
`app.log.gz` both default to `app.log.zst`; while one of them is being
compressed, a request for the other is refused, and the sentence names
the output:

```json
{
  "detail":  "Compression already in progress for /var/log/app.log.zst (task: 0b6c1d9e-1f4a-4c2e-9d55-7a3e8f20c4b1)",
  "task_id": "0b6c1d9e-1f4a-4c2e-9d55-7a3e8f20c4b1"
}
```

An index request for the output gets the same `409` while the task
runs. Two spellings of one output that differ in a symbolic link are
two paths here; the task's final rename or link still keeps their
files from mixing (see how the task writes its output, above).

## Performance notes

- Encoding is currently single-worker in the HTTP path — full
  `--workers=N` parallelism is CLI-only in this release
- Memory overhead: one frame buffer (default 4 MiB) during encode
- Compression ratio is highly input-dependent — a 465 MB application
  log gave 10.19× at the default level with 4 MiB frames
- Levels 10-22 took about 7× the time of the default level on that log
  for 10% less output; see
  [compression levels](../../concepts/compression.md#compression-level)

## See also

- [`rx compress`](../../cli/compress.md) — CLI equivalent
- [Tasks](tasks.md) — polling contract
- [concepts/compression](../../concepts/compression.md) — seekable-zstd mechanics
- [`/v1/samples`](samples.md) — random-access reads on compressed files
