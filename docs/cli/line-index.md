# `rx index`

Build, inspect, or delete line-offset indexes for fast line-number-to-byte
lookups on large files.

## Synopsis

```text
rx index PATH [PATH ...] [flags]
rx index PATH --info                 # inspect cached index
rx index PATH --delete               # remove cached index
rx index PATH --analyze              # build with full analysis
```

## Description

A **line index** is an on-disk JSON file that maps line numbers to byte
offsets via sparse checkpoints (typically one checkpoint every few
hundred kilobytes). Once an index exists, any operation that needs to
seek to a specific line can do so in O(1) — without an index, the same
operation scans the file linearly counting `\n`s.

`rx index` builds, re-uses, inspects, or deletes these indexes. With a
valid index already stored it is effectively free: `rx index` on a
465 MB log then takes 11 ms, process start included.

By default, `rx` only indexes files **≥ 50 MB**. Smaller files don't
need the overhead — a linear scan is fast enough. The threshold is
configurable via [`RX_LARGE_FILE_MB`](../configuration.md) or the
`--threshold` flag.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--force` | `bool` | `false` | Rebuild even if a valid cached index exists |
| `-i`, `--info` | `bool` | `false` | Print cached index metadata; do not build |
| `-d`, `--delete` | `bool` | `false` | Remove the cached index file |
| `--json` | `bool` | `false` | Emit machine-readable JSON |
| `-r`, `--recursive` | `bool` | `false` | Recurse into directory paths |
| `-a`, `--analyze` | `bool` | `false` | Run full analysis (line length stats, anomalies) |
| `--analyze-window-lines` | `int` | `0` (resolver default 128) | Sliding-window size for the anomaly coordinator; only used with `--analyze`. Capped at 2048 |
| `--threshold` | `int` | `0` (env default) | Minimum file size in MB to index; ignored with `--analyze` |

Mode flags have a priority order: `--delete` > `--info` > build.


### Global flags

Two flags are declared on the root command, so every subcommand accepts
them and they may be written before or after the subcommand name:

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--hidden` | `bool` | `RX_HIDDEN` or `false` | Include entries whose name starts with a dot |
| `--search-root` | `string[]` | `RX_SEARCH_ROOTS` or no sandbox | Restrict file access to this directory (repeatable) |

A path outside every configured root exits 4. A root that does not exist
is a usage error and exits 2. With neither the flag nor the variable set
there is no sandbox. See [Security](../concepts/security.md).

Every path given is processed, and each failure is reported in the
output. The command then exits 3 when every failure was a file that does
not exist, 4 when every failure was a path outside the roots or a file
the process may not read (`permission denied`, the words and the code
`rx trace` and `rx samples` give it), and 1 when the failures were of
different kinds.

A file or subdirectory that a walk of a directory meets and may not
read is not a failure: it is listed under `skipped` with the reason
`permission denied`, and the rest of the tree is indexed, as
`rx trace` searches the rest of it.

## Output

`rx index` prints one line per indexed file. With `--analyze` the
statistics and the anomaly counts follow underneath:

```text
Indexed and analyzed 1 files in 0.1s
  /var/log/app.log: 12 lines, 245.00 B
    Lines: 12 total, 0 empty
    Line ending: LF
    Line length: max=38, avg=19.4, median=11.0, p95=36.4, p99=37.7, stddev=11.9
    Longest line: line 3, offset 50
    Anomalies: 1 secrets-scan, 1 traceback-python
    Use --json for the full anomaly list
```

The per-anomaly line ranges, severities and descriptions appear only in
`--json`. When nothing was indexed the output is `No files indexed.`
Skipped files follow the indexed ones, each with its reason:

```text
No files indexed.
Skipped 2 files:
  /var/log/tiny.log: file size 17 bytes is below threshold 52428800 bytes
  /var/log/logs.tar.gz: not a text file: a NUL byte in the first 8 KiB of its decompressed text
```

rx-python prints the same layout, except that it gives only the count of
skipped files.

## Examples

### Build an index

```bash
rx index /var/log/audit-2026-03.log
```

Builds a line-offset index and stores it under
`~/.cache/rx/indexes/audit-2026-03.log_<hash>.json`. Next time
`rx samples` or `rx trace` runs against this file, it will use the
cached index for line-number lookups.

Output:

```text
index built for 1 files in 1.234s
  /var/log/audit-2026-03.log: 3528914 lines, cache=/home/you/.cache/rx/indexes/audit-2026-03.log_a1b2c3d4....json
```

### Inspect without rebuilding

```bash
rx index /var/log/app.log-2025121008 --info
```

Output:

```text
Index for: /var/log/app.log-2025121008
  file_type: text
  size_bytes: 487603212
  created_at: 2026-10-06T02:07:47.806161Z
  analysis_performed: false
  line_count: 1418377
  index_entries: 431
  time_format: iso, at the start of each line, no zone, read as UTC
  first_timestamp: 2025-12-10 07:00:04.574 (line 1)
  last_timestamp: 2025-12-10 08:00:04.390 (line 1418377)
  timestamped_lines: 1372906
  backward_steps: 0
```

`index_entries` is the number of checkpoints in the sparse line-index —
a function of file size and line density, not of line count directly.

The last five lines are the index's
[time section](../concepts/line-indexes.md#the-time-section), which every
build records:

- `time_format` names the detected format, where the timestamp sits
  (`at the start of each line` or `inside the line`), the day order of a
  slash date, and how a timestamp's zone is read: `no zone, read as UTC`
  for a file whose timestamps carry none, or `with zones (first +02:00)`.
  A format without a year adds `year from the file's mtime`. A file with
  no timestamp format shows `time_format: none recognized` and no other
  time line.
- `first_timestamp` and `last_timestamp` are the first and the last
  timestamped line, with their line numbers. A file without zones shows
  the wall-clock time as written (`2025-12-10 07:00:04.574`); a file
  with zones shows the UTC instant (`2026-10-06T10:34:56.123Z`).
- `timestamped_lines` counts the lines that carry a timestamp.
- `backward_steps` counts the lines whose timestamp is more than one
  second earlier than the latest one before them, with the largest step
  when there is one: `backward_steps: 3 (largest 2500 ms)`.

### Force a rebuild

```bash
rx index /var/log/audit-2026-03.log --force
```

Rebuilds even if a valid index exists. Useful after a file was modified
in place without the mtime changing (rare; happens with some rsync
configurations).

### Full analysis

```bash
rx index /var/log/audit-2026-03.log --analyze
```

In addition to the line-offset checkpoints, `--analyze` populates:

- Line-length statistics (max, avg, median, p95, p99, stddev)
- Byte offset of the longest line
- Detected line ending (LF vs CRLF)
- Compression info (for compressed inputs)
- Anomaly report (nine detectors run by default — see [concepts/analyzers](../concepts/analyzers.md))

`--analyze` is slower than a plain build because it walks every byte
of the file to gather statistics and dispatches each line to the
detector coordinator. A cached index satisfies an `--analyze` call only
when its analysis ran with the same `--analyze-window-lines` and the same
detectors at the same versions; the index records both
(`analysis_window_lines`, `analysis_detector_set`). Otherwise `rx` falls
through to a full rebuild.

### Tuning the anomaly window

Detectors that span multiple lines (tracebacks, multi-line JSON blobs)
look back through a fixed-size sliding window. The default is 128
lines, which covers most real-world stacks. Increase it if you see
multi-line patterns getting truncated:

```bash
rx index /var/log/app.log --analyze --analyze-window-lines=512
```

A negative value is a usage error (exit 2 on the CLI, 400 over HTTP),
because 0 and an absent flag already mean "use the default". rx-python
accepts the same flag and the same request field, with the same
precedence and the same refusal.

The resolver precedence is URL param > CLI flag > `RX_ANALYZE_WINDOW_LINES`
env var > default (128). Values outside `[1, 2048]` are clamped.

### Multiple files, recursive

```bash
rx index /var/log/ --recursive
```

Recursively walks `/var/log/`, indexes any file meeting the size
threshold, and reports a wrapped JSON envelope. The walk skips entries
whose name starts with a dot unless `--hidden`, and follows a symbolic
link only when naming its target would be allowed: with `--search-root`,
a link that leads outside every root, into a hidden entry, back to a
directory being walked or into a directory already walked is listed in
`skipped` and `skip_reasons` with the reason, and nothing is indexed
for it. See
[Symlinks inside a directory search](../concepts/security.md#symlinks-inside-a-directory-search).

```bash
rx index /var/log/ --recursive --json | jq '.indexed | length, .skipped | length, .errors | length'
```

The response envelope:

```json
{
  "indexed": [ { ... }, ... ],
  "skipped": [ "/var/log/tiny.log", "/var/log/logs.tar.gz" ],
  "skip_reasons": [
    { "path": "/var/log/tiny.log", "reason": "file size 17 bytes is below threshold 52428800 bytes" },
    { "path": "/var/log/logs.tar.gz", "reason": "not a text file: a NUL byte in the first 8 KiB of its decompressed text" }
  ],
  "errors":  [ { "path": "/var/log/broken", "error": "permission denied" } ],
  "total_time": 12.34
}
```

Below-threshold files, files that are not text, files rx refuses to
decompress (`decompressing it needs more than 128 MiB at once: …`, see
[Compression](../concepts/compression.md#how-rx-decides-what-a-file-is)),
and files or subdirectories a walk may not read land in `skipped` (not `errors`) — rx-go treats this as a normal outcome, not a failure.
`skip_reasons` gives the reason for each, in the same order, in the
words `POST /v1/index` answers its `400` with.

### Threshold override

```bash
rx index /tmp/small.log --threshold=1
```

Index a 1 MB file even though the default threshold is 50 MB. Useful
for testing or small-file benchmarks.

### Delete a cached index

```bash
rx index /var/log/audit-2026-03.log --delete
```

Removes the cache file. A subsequent build command rebuilds from
scratch.

### JSON output for automation

```bash
rx index /var/log/audit-*.log --json \
    | jq '.indexed[] | {path, line_count, index_path}'
```

The `indexed[].index_path` field is the absolute path to the on-disk
cache — useful when piping to other tools that need to open it.

`indexed[].time_index` is the whole time section, `max` (the line with
the highest timestamp), `max_before` and `zone_offsets` included, or
`null` for a file with no timestamp format:

```bash
rx index /var/log/app.log-2025121008 --json \
    | jq '.indexed[0].time_index | {format, timestamped_lines, first, last, max, backward_steps}'
```

## How it works

### Index structure

The on-disk format is a JSON file containing:

- Source path, mtime, size (for cache validation)
- File type (`text`, `compressed`, `seekable_zstd`)
- A `line_index` slice of `{line: N, offset: byteOffset}` entries
- The time section, `time_index`: the timestamp format of the lines,
  the first and last timestamps, and the latest timestamp before each
  checkpoint (`null` when no format is recognized)
- Optional line-length and anomaly statistics (when `--analyze` ran)

Checkpoints are sparse — not every line is in the index. The builder
emits a checkpoint every fixed byte distance (`index_step_bytes`),
which keeps the JSON small while still enabling O(1) seeks.

### Cache validation

When loading a cached index, `rx` checks:

1. Source path size matches the cached `source_size_bytes`
2. Source path mtime matches the cached `source_mtime_ns`, in
   nanoseconds since the Unix epoch, whatever the time zone
3. The inode, its device, the ctime and a fingerprint of the file
   match `source_inode`, `source_device`, `source_ctime_ns` and
   `source_fingerprint`

Any mismatch triggers a rebuild. There is no TTL-based expiry — a
cache entry is valid as long as the source file hasn't changed. See
[concepts/caching](../concepts/caching.md).

### Performance characteristics

Measured with the files in the page cache:

- **Warm read** (valid cache): 11 ms for the 465 MB log.
- **Cold build**: one sequential pass; 120 ms for the 465 MB log,
  2.7 s for a 6.3 GB log, the time section included. Slower when the
  file comes from disk.
- **`--analyze`**: far slower than a plain build, because every line
  goes through every detector: 18.2 s for the 465 MB log.
- **Memory**: the builder keeps only the sparse checkpoints; peak RSS
  was 16 MB for the 465 MB log, 17 MB for the 6.3 GB log and 26 MB for
  the analyzed build.

## Tips and gotchas

!!! tip "Index once, query many"
    `rx index` costs seconds to minutes depending on file size. Every
    subsequent `rx trace` or `rx samples` on the same file pays zero
    index cost. Always index files you'll query repeatedly.

!!! warning "Analyze requires full file walk"
    `--analyze` bypasses the file-size threshold and always walks the
    whole file. Don't run it in a batch script against thousands of
    files unless you actually need the line-length stats.

!!! note "Below-threshold ≠ error"
    When a file is smaller than the threshold, its path appears in the
    `skipped` list of the JSON output and the command still exits 0.
    Scripts that check exit codes won't break on small files.

!!! note "A seekable `.zst` is indexed by its frames"
    A *seekable* zstd — one produced by [`rx compress`](compress.md) —
    is indexed frame by frame rather than by a byte step. The index
    records which lines each frame holds, so
    `rx samples big.log.zst --lines=25000` decompresses the frame that
    holds line 25,000 instead of the whole stream. `file_type` is
    `seekable_zstd`, the checkpoints carry a frame number, and `frames`
    holds each frame's line range.

    `rx compress` builds this index for you unless you pass
    `--no-index`. That index has no analysis: `rx index --analyze
    big.log.zst` adds the line-length statistics and the anomalies,
    computed on the decompressed text in the same pass that reads the
    frames. They are the same as for the decompressed file, line numbers
    and byte offsets included. Without `--analyze` a seekable index
    leaves the line-length statistics out.

!!! warning "Other compressed formats have no random access"
    gzip, bzip2, xz and plain (non-seekable) zstd are indexed through
    their decompressor, so the line numbers and byte offsets describe
    the text inside — but a lookup still streams from the start,
    because the format offers nowhere else to begin. `--analyze` works
    on every compressed format and analyses the text inside. A
    compressed tar archive (`.tar.gz`, `.tgz` and the like) is not
    text, by the rule every command applies (see
    [Compression](../concepts/compression.md#how-rx-decides-what-a-file-is)):
    `rx index` lists it under `skipped` with a reason that starts with
    `not a text file`, and `POST /v1/index` answers `400` with
    "not a text file: …: <path>". No index is built for it.

## See also

- [concepts/line-indexes](../concepts/line-indexes.md) — what indexes are and when to build one
- [concepts/caching](../concepts/caching.md) — cache layout and invalidation
- [`rx trace`](trace.md) — search with index-aware line numbers
- [`rx samples`](samples.md) — retrieve content by line number
- [api/endpoints/line-index](../api/endpoints/line-index.md) — indexing over HTTP
