# Chunking

Parallel regex search on a single large file requires splitting the file
into byte ranges that workers can scan independently. Done naively, this
produces duplicate or missing matches at chunk seams. `rx`'s chunking
algorithm is designed to produce the **same result as a single-process
scan**, bit-for-bit, while using every CPU core available.

## The problem

A single file, 1.3 GB. A single regex, `error`. One worker takes
~100 seconds. We'd like this to finish in ~25 seconds with 4 workers.

Obvious approach: divide the file into four 325 MB ranges and scan each
in parallel. Problem: the byte range `0..325MB` might end in the middle
of a line. If a match `error` straddles that boundary, it's either
missed (if neither chunk contains the whole line) or duplicated (if
both chunks overlap it).

## The solution: newline-aligned boundaries

`rx` carves the file into contiguous, **newline-aligned** byte ranges:

- Chunk 0 starts at byte 0
- Chunk N+1 starts at the byte **immediately after** the newline closest
  to the desired boundary of chunk N

Concretely, for each target boundary `B`:

1. Seek to byte `B`
2. Read forward up to 256 KB looking for the first `\n`
3. The boundary becomes `(byte position of \n) + 1`

This guarantees:

- Every byte of the file is in exactly one chunk
- No chunk starts in the middle of a line
- No chunk ends in the middle of a line (the next chunk starts on the
  next line)
- No overlap between chunks

When ripgrep scans a chunk, every match is fully inside the chunk. No
post-processing is needed to deduplicate boundary matches.

### Pseudo-code

```text
chunk_count     = desired_worker_count
target_size     = file_size / chunk_count
max_line_length = 256 KB  // configurable via RX_MAX_LINE_SIZE_KB

boundaries = [0]
for i in 1..chunk_count-1:
    tentative = i * target_size
    final     = find_next_newline(tentative, max_line_length)
    boundaries.append(final)
boundaries.append(file_size)

chunks = [(boundaries[i], boundaries[i+1]) for i in 0..chunk_count-1]
```

## When chunking engages

Chunking only runs when the file is larger than
`RX_MIN_CHUNK_SIZE_MB` (default **20 MB**). Below that threshold, the
file is scanned by a single worker — the startup cost of spawning and
synchronizing multiple workers dominates for small files.

For a file just above threshold (25 MB), `rx` typically creates 2
chunks. As file size grows, the chunk count approaches the worker
limit.

## Worker coordination

- A goroutine pool of size `RX_WORKERS` (default: `NumCPU`) consumes
  chunks from a channel
- Each worker opens the file, seeks to its chunk start, and spawns a
  `ripgrep` subprocess bounded to the chunk byte range
- `ripgrep` outputs JSON; the worker parses it and appends matches to
  its own per-worker result slice (no shared lock)
- After all workers finish, results are merged and sorted by
  `(file, offset)`

The "no shared lock" point matters. An earlier design used a single
shared result slice guarded by a mutex; every chunk's append serialized
on the mutex and the parallelism gain was marginal. Switching to
per-worker slices with a final merge step produced a 2-3× speedup at
the same worker count.

## Line number resolution

`ripgrep` reports line numbers relative to the start of its input
(i.e. the chunk start). `rx` needs **absolute** line numbers. Two
strategies:

1. **With a cached line index**: look up the chunk's start offset in
   the index to find the absolute line number of the chunk's first
   line. Add ripgrep's relative number. O(log N) lookup, O(1) math.

2. **Without an index**: either use the line count that chunk
   boundaries naturally produce (each chunk knows how many `\n` bytes
   it read), or compute line numbers across chunks by summing prior
   chunk line counts. O(N) overall, but only runs once per trace.

With an index, absolute line numbers are effectively free. Without
one, they cost a linear pass.

## Performance characteristics

### Scaling

On a literal-dense pattern (many matches per unit of input), `rx`
scales near-linearly with worker count up to the physical core count:

- 1 worker: ~70 s on 1.3 GB
- 2 workers: ~44 s (1.59×)
- 4 workers: ~35 s (1.97×)
- 8 workers: ~36 s (plateau — hyperthreads don't help CPU-bound regex)

Beyond physical cores, gains are minimal because the workload is
memory-bandwidth-bound, not CPU-bound.

### Regex complexity

Complex regex patterns (look-around, nested quantifiers, large
character classes) don't benefit as much from added workers. The
compiled automaton becomes the bottleneck; more workers just mean
more copies of a slow state machine. Simple literal patterns scale
best.

### Multi-pattern

`rx` passes all patterns to a single `ripgrep` invocation per chunk.
Ripgrep compiles a combined matcher that scans once for any of the
patterns. Marginal cost per added pattern is small — a 5-pattern
scan is not 5× slower than a 1-pattern scan.

## Tunables

| Variable | Effect |
|---|---|
| `RX_WORKERS` | Goroutine pool size. Default: `NumCPU`. |
| `RX_MIN_CHUNK_SIZE_MB` | Smallest chunk. Default: `20`. Below-threshold files skip parallel mode entirely. |
| `RX_MAX_LINE_SIZE_KB` | Assumed largest line length the engine tolerates when choosing chunk overlap. Default: `8`. Genuinely long lines may benefit from a bump. The newline-search forward window is separately bounded to 256 KB. |
| `RX_MAX_SUBPROCESSES` | Upper bound on concurrent workers (despite the name, applies to goroutines in the Go port). Default: `20`. |

## Implications

- **Files must have newlines within 256 KB of any boundary.** Files
  with million-character-long lines (some CSV exports, some JSON
  lines with embedded blobs) won't chunk cleanly. `rx` falls back to
  a best-effort boundary at the end of the scan window, which may
  cause a match on a line that spans the boundary to be missed.
- **Compressed files can't be chunked** — the decompressor has no
  random access. Scan falls back to single-worker stream mode. For
  random access, use [`rx compress`](../cli/compress.md) to produce
  seekable zstd, which can be chunked on frame boundaries.
- **Scan time is dominated by disk bandwidth at high worker counts.**
  If your worker count maxes the disk's read throughput, adding more
  workers just creates contention.

## Related concepts

- [Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md)
- [Line indexes](line-indexes.md)
- [Caching](caching.md)
- [Performance](../performance.md) — measured numbers
