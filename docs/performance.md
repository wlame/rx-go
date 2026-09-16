# Performance

Measured numbers, scaling behavior, and tuning guidance for `rx` under
realistic workloads.

## Headline numbers

Measured on an Apple M4 Max (16 cores, 128 GB RAM) with real log files
already in the page cache, `rx` built from source, median of five runs
of the whole command (process start included). From disk, the scans are
bounded by read bandwidth instead.

| Scenario | Input | Median wall-clock |
|---|---|---:|
| Rare literal, no cache | 6.3 GB log, 42.5 M lines | **0.49 s** |
| Dense pattern (`" E "`, 894,264 matching lines), `--json` | 6.3 GB log | **5.07 s** |
| Three patterns (`-e ERROR -e WARN -e Exception`) | 6.3 GB log | **0.52 s** |
| `--max-results=10` on the dense pattern | 6.3 GB log | **31 ms** |
| Trace cache hit, rare literal | 6.3 GB log | **12 ms** |
| Cold index build | 6.3 GB log | **2.2 s** |
| Cold index build | 465 MB log | **142 ms** |
| `rx index` with a valid index | 465 MB log | **11 ms** |
| `samples --lines=40000000`, with index | 6.3 GB log | **20 ms** |
| `samples --lines=40000000`, no index | 6.3 GB log | **2.46 s** |
| `samples --lines=1-1000`, no index | 6.3 GB log | **12 ms** |
| `rx --version` | — | **11 ms** |

For comparison, `rg -c " E "` on the same 6.3 GB file took 0.64 s. The
dense trace spends its extra time building and printing 894,264 match
records.

## Bounded read contract

`rx` reads no more of the source file than its result requires, except
where this table says otherwise:

| Operation | What it reads |
|---|---|
| `rx samples --lines=START-END` (plain file) | with an index, from the nearest checkpoint before START to END; without one, from byte 0 to END |
| `rx samples --lines=N --context=K` (plain file) | the same, for the window `N-K` to `N+K` |
| `rx samples --offsets=A-B` (plain file) | from the nearest checkpoint before A (or byte 0 without an index) to about B, to number the lines |
| First `rx samples` lookup in a plain file of `RX_LARGE_FILE_MB` (50 MB) or more, or in any compressed file, with no cached index | the whole file, once, to build and store the index — an index build, as `rx index` does. Over HTTP it runs as a background `index` task shared by every request for the file; a request waits for it up to `RX_SAMPLES_WAIT_SECONDS` (5 s) and then answers `202` with the task, so no request reads the whole file itself. `rx samples` waits for the build. `--no-index` or `RX_NO_INDEX` turns it off |
| `rx samples --lines` on a gzip, bzip2, xz or plain zstd file | the decompressed stream from its first byte up to the last wanted line (and up to one decoder block past it); a negative line needs the line count, which an index gives, and without one costs a first pass over the whole stream |
| `rx samples --lines` on a seekable zstd file with an index | the frames holding the wanted lines |
| `rx trace --max-results=M` | **early-cancel**: as soon as the collector has M matches, in-flight ripgrep subprocesses are killed and queued chunks are skipped; numbering a match the cap left unnumbered reads from the nearest index checkpoint, or nothing without an index (or from byte 0 with `--no-index`) |
| `rx trace` (no cap) | full file — this is the design |
| `rx index` | full file — this is the design (builds the checkpoint map) |
| `rx compress` | full file — this is the design (output is the file) |
| `GET /v1/tree` | `os.Stat` per entry; 512-byte peek for text-file detection; no full-file reads |
| `GET /v1/index` (cached) | the cached JSON index file only; no source-file access |
| Webhook dispatch | non-blocking fire-and-forget; full queue = drop with metric, never blocks trace |

No row is an exception to "no more than the result requires": the
reads of a whole file are an index build, a search without a cap and a
compression, whose results need every byte. A samples request never
reads a whole file for the index it wants; that build is a background
task.

The bounds are enforced by byte-budget unit tests (for
example `internal/samples/resolver_budget_test.go`,
`internal/samples/batch_offsets_budget_test.go`,
`internal/samples/compressed_lines_budget_test.go` and
`internal/trace/maxresults_budget_test.go`) that wrap the I/O with a
counting reader and assert on bytes actually read, so a regression of
the form "the output is correct but we read more than necessary" is
caught at unit-test time.

## Worker scaling

The rare literal on the 6.3 GB log, by `RX_WORKERS` (median of three):

| Workers | Wall-clock | Speedup vs 1 worker |
|---:|---:|---:|
| 1 | 1.45 s | 1.00× |
| 2 | 1.03 s | 1.41× |
| 4 | 0.52 s | 2.80× |
| 8 | 0.46 s | 3.18× |
| 16 | 0.50 s | 2.93× |

Gains flatten past 4-8 workers: the scan becomes bound by memory
bandwidth and by the work outside the scan (process start, planning,
merging).

### Tuning advice

- **Default**: don't set `RX_WORKERS` — `rx` uses the smaller of
  `NumCPU` and `RX_MAX_SUBPROCESSES` (20)
- **Limit when**: running alongside other CPU-heavy workloads. Cap to
  half the core count to leave headroom
- **Increase when**: I/O-bound scans (high-latency network mounts)
  where more requests in flight hide the latency. Try `NumCPU * 2`

## Memory profile

Peak RSS (`/usr/bin/time -l`) on the same machine:

| Command | Peak RSS |
|---|---:|
| `rx trace NullPointerException` on the 6.3 GB log (58 matches) | 22 MB |
| `rx trace " E " --json` on the 6.3 GB log (894,264 matches) | 5.9 GB |
| `rx index` on the 465 MB log | 22 MB |
| `rx index` on the 6.3 GB log | 23 MB |

A scan itself streams; what costs memory is the result. Every match is
held, with its line text, until the answer is printed, so memory grows
with the match count and the line length — about 6.6 KB per match on
that log. An index build keeps only its checkpoints.

### OOM mitigations

If you're hitting OOM on scans with many matches:

1. Add `--max-results` to bound the result
2. Make the pattern more selective
3. Split the input: one file per `rx trace` call

## Binary size and startup

- Statically-linked binary: **13.9 MB** (`-ldflags '-s -w'`,
  `CGO_ENABLED=0`, darwin/arm64)
- Start-up: **11 ms** (`rx --version`)
- No runtime dependencies except `ripgrep`
- No external shared libraries (`libzstd`, `libxz`, etc. are bundled in
  pure-Go implementations)

## Caching impact

| Operation | Without cache or index | With |
|---|---|---|
| `rx index` on the 465 MB log | 142 ms (build) | 11 ms (valid index found) |
| `rx trace` rare literal, 6.3 GB log | 0.48 s | 12 ms (trace cache hit) |
| `rx trace WARN`, 465 MB log, 51,817 matches | 331 ms | 429 ms (trace cache hit) |
| `rx trace WARN --max-results=100`, 465 MB log (`time` field) | 15 ms | 30 ms (trace cache hit) |
| `rx samples --lines=40000000`, 6.3 GB log | 2.46 s | 20 ms (index) |

A trace cache hit parses the stored matches and reads the file on one
core from the first match it returns to the last, so it is cheap for a
selective pattern and can cost more than a scan for a dense one when
the file is already in the page cache. A capped hit stops after the
matches it returns but still parses the whole entry; see
[caching](concepts/caching.md#trace_cache). From disk, a scan of a large
file costs far more than either. For workloads that look up lines in
the same files repeatedly, pre-building the indexes pays off:

```bash
# Pre-warm at deploy time.
find /var/log -name "*.log" -size +50M -exec rx index {} \;
```

## Compression trade-offs

### Read throughput

Compressed-file `rx trace` is single-threaded for gzip, bzip2, xz and
plain zstd — parallel workers can't split a non-seekable compressed
stream. A seekable zstd file is scanned frame-parallel. `rx samples
--lines` on a 113 MB `.gz` of the 465 MB log took 1.6 s for any line,
because it streams the whole file. Use [`rx compress`](cli/compress.md)
to convert to seekable zstd for parallel scans and frame-sized
lookups.

### Seekable zstd encoding

`--level` accepts 1-22, but the encoder has four settings. On the
465 MB log, one worker, 4 MiB frames:

| Levels | Time | Output | Ratio |
|---|---:|---:|---:|
| 1 | 0.47 s | 82.9 MB | 5.88× |
| 2-5 (default 3) | 0.71 s | 47.8 MB | 10.19× |
| 6-9 | 0.97 s | 44.4 MB | 10.99× |
| 10-22 | 4.84 s | 43.2 MB | 11.27× |

`--workers=4` took the default level to 0.29 s and levels 10-22 to
1.71 s. `zstd -3`, one frame, wrote 41.6 MB.

### Frame size trade-off

Smaller frames = finer random access, worse ratio. Same log, default
level:

| Frame size | Frames | Output | Ratio |
|---|---:|---:|---:|
| 1 MiB | 429 | 95.3 MB | 5.11× |
| 4 MiB (default) | 114 | 47.8 MB | 10.19× |
| 16 MiB | 29 | 32.7 MB | 14.91× |
| 64 MiB | 8 | 29.7 MB | 16.43× |

For files queried by line number, 1-4 MiB is usually right; this log's
very long lines make small frames costlier than usual.

## When to build an index

Build an index when:

- The file is ≥ 50 MB (the default threshold) AND
- You'll run line-number lookups or range queries (`--lines=...`) AND
- The file is queried more than once

`rx samples` builds one by itself on the first lookup in such a file,
and `GET /v1/samples` starts one as a background task, so an explicit
`rx index` moves that cost to a time you choose.

Skip the index when:

- The file is small (< 50 MB) — a lookup from byte 0 is cheap
- You'll only query the file once — the build costs a full read
- You only use byte offsets near the start of the file

### Cost/benefit

| Operation (6.3 GB log) | Without index | With index |
|---|---|---|
| `rx samples --lines=40000000` | 2.46 s (reads to line 40 000 000) | 20 ms (seek to the nearest checkpoint) |
| `rx samples --lines=-50--1` | 11 ms | 11 ms |
| `rx trace --max-results=N` line numbers | `-1` for matches the cap left unnumbered | counted from the nearest checkpoint |

## I/O patterns

### Sequential reads

`rx trace` and `rx index` do sequential reads per worker. OS page
cache helps on repeated runs — a just-scanned file typically sits in
cache for subsequent queries.

### Random reads

`rx samples` does random-access reads. On SSDs and NVMe, this is
roughly the same cost as sequential. On HDD or network mounts,
expect noticeably worse latency per lookup.

### Write patterns

Caches are written via a temporary file and a rename. An index is
about one checkpoint per MB of source (9.3 KB for the 465 MB log); a
trace cache entry grows with the match count (4.9 MB for 51,817
matches).

## Extrapolation to very large files

The largest file measured here is **6.3 GB**. For files in the
10-100 GB range, a rare-pattern scan from the page cache grows with
the file size:

```text
naive projection = (file_size / 6.3 GB) × 0.49 s
```

For a 100 GB file that is about 8 s, which no machine reaches unless
the file is in memory. Caveats:

- **Disk bandwidth becomes the bottleneck.** At 3 GB/s, reading
  100 GB takes over 30 s, so a scan from disk is disk-bound, not
  CPU-bound.
- **Result set size**: memory grows with the match count (about
  6.6 KB per match on the log above). A dense pattern over a 100 GB
  file can exhaust RAM. Use `--max-results` to cap.

**Recommendation**: benchmark at your target scale before claiming
reliability there.

## Measuring your own workloads

`rx` doesn't ship a built-in benchmarking tool, but two useful
approaches:

### Wall-clock with `time`

```bash
time rx "pattern" /var/log/your-file.log > /dev/null
```

Run it five times and take the median. The first run reads the file
from disk; the others find it in the page cache, so report the two
separately.

### Server-side metrics

With `rx serve` running:

```bash
# p95 latency for traces.
curl -s http://127.0.0.1:7777/metrics \
    | grep rx_trace_duration_seconds_bucket
```

Or use a Prometheus/Grafana pairing — see
[api/endpoints/metrics](api/endpoints/metrics.md) for suggested
queries.

## Related

- [Configuration](configuration.md) — every tunable
- [Chunking](concepts/chunking.md) — parallel algorithm
- [Caching](concepts/caching.md) — cache behavior
- [Troubleshooting](troubleshooting.md) — performance issues
