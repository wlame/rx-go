# Performance

Measured numbers, scaling behavior, and tuning guidance for `rx` under
realistic workloads.

## Headline numbers

Measured on a linux/arm64 host (4 physical cores, NVMe storage) with
user-supplied real-world fixtures:

| Scenario | Input | Median wall-clock |
|---|---|---:|
| Literal-dense regex, 4 workers | 1.3 GB Twitter dump (10 M lines) | **38.9 s** (~40 ms/MB) |
| Multi-pattern regex, 4 workers | 1.3 GB Twitter dump | **62.2 s** |
| Warm-cache index read | 260 MB JSON file | **10.7 ms** |
| Cold index build | 260 MB JSON file | **693 ms** |
| Cold index build | 11 MB WebRTC log | **11 ms** |
| Trace cache hit | 260 MB file | **< 50 ms** |
| Samples range (`--lines=1-1000`) on 1.3 GB | 1.3 GB Twitter dump | **~5 ms** |
| Samples range mid-file (indexed) on 1.3 GB | 1.3 GB Twitter dump | **~5 ms** |
| Trace with `--max-results=10` | 1.3 GB Twitter dump | **~70 ms** |
| Server cold start | — | **~50 ms** |

The 40 ms/MB literal-scan figure is a useful rule of thumb for
extrapolation, though real throughput depends heavily on pattern
complexity, file content, and hardware.

## Bounded read contract

`rx` guarantees that every read-oriented operation consumes no more of
the source file than its result requires. Specifically:

| Operation | Guarantees we read |
|---|---|
| `rx samples --lines=START-END` | at most `END × avg_line_bytes` (+ buffer overshoot); with an index, from the nearest checkpoint to END |
| `rx samples --lines=N --context=K` | a window of about `2K+1` lines around N, starting from the index checkpoint at-or-before `N-K` |
| `rx samples --offsets=A-B` | roughly `B + avg_line` bytes (offset-to-line conversion scans from byte 0; future optimization planned) |
| `rx trace --max-results=M` | **early-cancel**: as soon as the collector has M matches, all in-flight ripgrep subprocesses receive SIGKILL and queued chunks are skipped; you do not pay for work past the cap |
| `rx trace` (no cap) | full file — this is the design |
| `rx index` | full file — this is the design (builds the checkpoint map) |
| `rx compress` | full file — this is the design (output is the file) |
| `GET /v1/tree` | `os.Stat` per entry; 512-byte peek for text-file detection; no full-file reads |
| `GET /v1/index` (cached) | the cached JSON index file only; no source-file access |
| Webhook dispatch | non-blocking fire-and-forget; full queue = drop with metric, never blocks trace |

These bounds are enforced by byte-budget unit tests (see
`internal/samples/resolver_budget_test.go` and
`internal/trace/worker_maxresults_test.go`) that wrap the I/O with a
counting reader and assert on bytes actually read. Regressions of the
form "the output is correct but we read more than necessary" are caught
at unit-test time rather than surfacing as end-user slowdowns.

### History

Stage 9 Round 5 (2026-04-18) found and fixed three bounded-read regressions:

- **R5-B1**: `samples --lines=START-END` was reading to EOF on every
  range request because the loop's break condition was unreachable when
  no target line was requested. On the 1.3 GB fixture, a request for
  lines 1-1000 took ~590 ms before the fix and ~5 ms after
  (**~140× speedup**). The fix introduces a `needTarget` flag that
  decouples the range-only path from the target-offset path.
- **R5-B2**: `ProcessAllChunks` did not propagate `max_results` to its
  worker fanout, so all chunks scanned to completion regardless. The
  fix adds a tally channel and a cooperative cancel: as soon as the
  match count reaches the cap, the errgroup context is canceled,
  in-flight ripgrep subprocesses are killed via `exec.CommandContext`,
  and queued chunks are skipped without spawning rg at all.
- **R5-B3**: `ProcessSeekable` had the same issue for seekable-zstd
  files. Fixed with the same tally+cancel pattern.

## Worker scaling

On the 1.3 GB fixture:

| Workers | Wall-clock | Speedup vs 1 worker |
|---:|---:|---:|
| 1 | 69.7 s | 1.00× |
| 2 | 43.8 s | 1.59× |
| 4 | 35.3 s | 1.97× |
| 8 | 36.0 s | plateau |

Scaling is near-linear to the physical core count (4 in this run).
Beyond physical cores, hyperthreads don't help — the workload is
memory-bandwidth-bound and sharing a core between two regex threads
adds cache pressure without adding throughput.

### Tuning advice

- **Default**: don't set `RX_WORKERS` — `rx` uses `NumCPU`, which
  matches the scaling sweet spot on most hardware
- **Limit when**: running alongside other CPU-heavy workloads. Cap to
  half the core count to leave headroom
- **Increase when**: I/O-bound scans (high-latency network mounts)
  where Go runtime will happily park blocked workers and dispatch
  more. Try `NumCPU * 2`

## Memory profile

Memory usage varies by pattern complexity and match density:

- **Single-pattern literal scan**: a few hundred MB per worker
- **Multi-pattern regex (3-5 patterns)**: peaks at several GB on
  large fixtures because each worker holds a compiled regex set
- **Index build**: peak RSS roughly equals the source file size
  (the full line-offset slice is held in memory before emission)
- **Trace with large result set**: scales with match count; a
  million-match scan holds roughly 100-200 bytes per match

Rule of thumb: budget at least **file size ÷ workers** in RAM,
plus a few hundred MB per worker for overhead.

### OOM mitigations

If you're hitting OOM on large multi-pattern scans:

1. Reduce `RX_WORKERS` (fewer automaton copies)
2. Reduce `RX_MIN_CHUNK_SIZE_MB` to work with smaller in-flight buffers
3. Split the scan: one pattern per `rx trace` call
4. Add `--max-results` to bound result memory

## Binary size and startup

- Statically-linked binary: **13 MB** (stripped, `CGO_ENABLED=0`)
- Cold start overhead: **~50 ms** (`time rx --version`)
- No runtime dependencies except `ripgrep`
- No external shared libraries (`libzstd`, `libxz`, etc. are bundled in
  pure-Go implementations)

## Caching impact

The trace cache and index cache have large effects on repeated
operations:

| Operation | Cold | Warm |
|---|---|---|
| `rx index` on 260 MB | ~700 ms | ~10 ms |
| `rx trace` on 260 MB with matching patterns | ~1.7 s | ~50 ms (cache hit) |
| `rx samples --lines=N` on 260 MB | index-dependent | ~10 ms |

The warm-cache index read is particularly fast — a single JSON parse.
For workloads that query the same files repeatedly, pre-warming the
cache has outsized leverage:

```bash
# Pre-warm at deploy time.
find /var/log -name "*.log" -size +50M -exec rx index {} \;
```

## Compression trade-offs

### Read throughput

Decompressor throughput bounds scan speed for compressed files:

| Format | Approximate decompress rate |
|---|---|
| gzip | 200-400 MB/s |
| bzip2 | 20-80 MB/s |
| xz | 60-120 MB/s |
| zstd | 500-1500 MB/s |

Compressed-file `rx trace` is single-threaded — parallel workers
can't split a non-seekable compressed stream. Use
[`rx compress`](cli/compress.md) to convert to seekable zstd for
parallelism on future scans.

### Seekable zstd encoding

- **Level 3 (default)**: ~200-300 MB/s per worker
- **Level 9**: ~30-50 MB/s per worker
- **Level 19**: ~5 MB/s per worker
- **Size ratio**: typically 10×-30× on real-world log data

Seekable zstd is ~4-5% larger than monolithic zstd at the same level
due to per-frame dictionary restarts.

### Frame size trade-off

Smaller frames = better random access, worse ratio:

| Frame size | Ratio penalty vs monolithic | Seek granularity |
|---|---|---|
| 1 MiB | ~8% | 1 MiB |
| 4 MiB (default) | ~4% | 4 MiB |
| 16 MiB | ~2% | 16 MiB |
| 64 MiB | ~1% | 64 MiB |

For files queried by line number, 1-4 MiB is usually right.

## When to build an index

Build an index when:

- The file is ≥ 50 MB (the default threshold) AND
- You'll run line-number lookups or range queries (`--lines=...`) AND
- The file is queried more than once

Skip the index when:

- The file is small (< 50 MB) — below-threshold files stream-scan
  fast enough without help
- You'll only query the file once — the build cost exceeds the query
  cost
- You only use byte offsets — they don't need an index

### Cost/benefit

| Operation | Without index | With index |
|---|---|---|
| `rx samples --lines=1000000` on 1.3 GB | linear scan to line 1 000 000 (~150 MB read, ~200 ms) | ~5 ms (seek to nearest checkpoint) |
| `rx samples --lines=-50--1` on 1.3 GB | full-file line count (~1.3 GB read, ~1 s) | ~5 ms (total line count from index) |
| `rx trace` absolute line numbers | linear scan per chunk | O(log N) index lookup |

Without an index, `samples` still respects the bounded-read contract:
it reads from byte 0 up to the requested end line and stops there. The
index turns that linear portion into a sub-second random-access seek.

Pay once at index-build time; every subsequent query is near-free.

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

Caches are written atomically via temp-file + rename. Each
cache-write is a few hundred KB to a few MB for indexes; trace caches
scale with match count.

## Extrapolation to very large files

The largest measured fixture was **1.3 GB**. For files in the
10-100 GB range, the scaling behavior is expected to hold for
literal-dense patterns and `NumCPU=4`:

```text
naive projection = (file_size / 1.3 GB) × 38.9 s
```

For a 100 GB file: ~50 minutes of wall-clock. Caveats:

- **Memory scaling is not verified** at this size. Single-pattern
  scans should be fine (bounded per-worker buffers), but multi-pattern
  scans at peak match density could exceed typical RAM.
- **Disk bandwidth becomes the bottleneck**. At NVMe speeds (3 GB/s),
  minimum read time for 100 GB is ~33 s, so the 50 min estimate
  assumes the scan is CPU-bound, not disk-bound.
- **Result set size**: a literal-dense scan of a 100 GB file could
  produce many millions of matches, exceeding JSON output buffers.
  Use `--max-results` to cap.

**Recommendation**: benchmark at your target scale before claiming
reliability there. File an issue if you do — we'd like data points.

## Measuring your own workloads

`rx` doesn't ship a built-in benchmarking tool, but two useful
approaches:

### Wall-clock with `time`

```bash
time rx "pattern" /var/log/your-file.log > /dev/null
```

Median over 5 runs: discard the first (cold cache), average the
remaining 4.

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
