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

`rx index` builds, re-uses, inspects, or deletes these indexes. On a
warm cache it's effectively free: reading the cached index for a 260 MB
file takes ~10 ms.

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
rx index /var/log/audit-2026-03.log --info
```

Output:

```text
Index for: /var/log/audit-2026-03.log
  file_type: text
  size_bytes: 582137856
  created_at: 2026-04-18T14:22:03.501729
  analysis_performed: false
  line_count: 3528914
  index_entries: 2187
```

`index_entries` is the number of checkpoints in the sparse line-index —
a function of file size and line density, not of line count directly.

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
detector coordinator. A cached non-analyze index cannot satisfy an
`--analyze` call; `rx` falls through to a full rebuild.

### Tuning the anomaly window

Detectors that span multiple lines (tracebacks, multi-line JSON blobs)
look back through a fixed-size sliding window. The default is 128
lines, which covers most real-world stacks. Increase it if you see
multi-line patterns getting truncated:

```bash
rx index /var/log/app.log --analyze --analyze-window-lines=512
```

The resolver precedence is URL param > CLI flag > `RX_ANALYZE_WINDOW_LINES`
env var > default (128). Values outside `[1, 2048]` are clamped.

### Multiple files, recursive

```bash
rx index /var/log/ --recursive
```

Recursively walks `/var/log/`, indexes any file meeting the size
threshold, and reports a wrapped JSON envelope:

```bash
rx index /var/log/ --recursive --json | jq '.indexed | length, .skipped | length, .errors | length'
```

The response envelope:

```json
{
  "indexed": [ { ... }, ... ],
  "skipped": [ "/var/log/tiny.log", "/var/log/also-tiny.log" ],
  "errors":  [ { "path": "/var/log/broken", "error": "permission denied" } ],
  "total_time": 12.34
}
```

Below-threshold files land in `skipped` (not `errors`) — rx-go treats
this as a normal outcome, not a failure.

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

## How it works

### Index structure

The on-disk format is a JSON file containing:

- Source path, mtime, size (for cache validation)
- File type (`text`, `compressed`, `seekable_zstd`)
- A `line_index` slice of `{line: N, offset: byteOffset}` entries
- Optional line-length and anomaly statistics (when `--analyze` ran)

Checkpoints are sparse — not every line is in the index. The builder
emits a checkpoint every fixed byte distance (`index_step_bytes`),
which keeps the JSON small while still enabling O(1) seeks.

### Cache validation

When loading a cached index, `rx` checks:

1. Source path mtime matches the cached `source_modified_at`
2. Source path size matches the cached `source_size_bytes`

Either mismatch triggers a rebuild. There is no TTL-based expiry — a
cache entry is valid as long as the source file hasn't changed. See
[concepts/caching](../concepts/caching.md).

### Performance characteristics

- **Warm read** (valid cache): ~10 ms regardless of file size. JSON
  parsing dominates.
- **Cold build**: single-threaded. Roughly real-time-per-GB on fast
  NVMe disks; slower on HDD or network mounts.
- **`--analyze`**: 2-4x slower than a plain build due to line-length
  gathering.
- **Memory**: the builder holds the full line-offset slice in memory
  before emit. On a 260 MB source file this peaks around 256 MB RSS.
  On a 100 GB file, memory would be significant — future work.

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

!!! warning "Compressed files need special handling"
    Indexing a `.zst` file only works when it's a *seekable* zstd — one
    produced by [`rx compress`](compress.md). Plain zstd, gzip, bzip2,
    and xz files don't have random access, so their "indexes" just
    point at the compressed stream start. Line-number lookups then fall
    back to streaming decompression.

## See also

- [concepts/line-indexes](../concepts/line-indexes.md) — what indexes are and when to build one
- [concepts/caching](../concepts/caching.md) — cache layout and invalidation
- [`rx trace`](trace.md) — search with index-aware line numbers
- [`rx samples`](samples.md) — retrieve content by line number
- [api/endpoints/line-index](../api/endpoints/line-index.md) — indexing over HTTP
