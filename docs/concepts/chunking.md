# Chunking

Parallel regex search on a single large file requires splitting the file
into byte ranges that workers can scan independently. Done naively, this
produces duplicate or missing matches at chunk seams. `rx`'s chunking
algorithm is designed to produce the **same result as a single-process
scan**, bit-for-bit, while using every CPU core available.

## The problem

A single file, 6.3 GB. A single regex. One worker takes 1.45 s with
the file in the page cache; four workers take 0.52 s.

Obvious approach: divide the file into four equal byte ranges and scan
each in parallel. Problem: the byte range `0..325MB` might end in the middle
of a line. If a match `error` straddles that boundary, it's either
missed (if neither chunk contains the whole line) or duplicated (if
both chunks overlap it).

## The solution: newline-aligned boundaries

`rx` carves the file into contiguous, **newline-aligned** byte ranges:

- Chunk 0 starts at byte 0
- Chunk N+1 starts at the byte **immediately after** the first newline
  at or after the desired boundary of chunk N

Concretely, for each target boundary `B`:

1. Seek to byte `B`
2. Read forward, 256 KB at a time, until the first `\n`
3. The boundary becomes `(byte position of \n) + 1`

A line of any length stays whole: when the line across `B` is longer
than one read, the search keeps reading to its end. When that line also
covers the next desired boundary, the two boundaries collapse into one
and the file gets one chunk fewer. That later boundary needs no read of
its own, since the search before it already crossed it, so planning
reads a file that is one long line once, not once per boundary.

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
chunk_count = min(file_size / RX_MIN_CHUNK_SIZE_MB, RX_MAX_SUBPROCESSES), at least 1
target_size = file_size / chunk_count

boundaries  = [0]
searched_to = 0
for i in 1..chunk_count-1:
    tentative = i * target_size
    if tentative < searched_to:
        continue                     // inside the line the last search crossed
    final = find_next_newline(tentative, file_size)   // file_size when none
    searched_to = final
    if final < file_size:
        boundaries.append(final)
boundaries.append(file_size)

chunks = [(boundaries[i], boundaries[i+1]) for i in 0..chunk_count-1]
```

## When chunking engages

A plain file is split into `file_size / RX_MIN_CHUNK_SIZE_MB` chunks,
rounded down, at most `RX_MAX_SUBPROCESSES` and at least one. With the
defaults (20 MB and 20) a file below 40 MB is one chunk, a 45 MB file is
two, and every file of 400 MB or more is twenty — the 531 MB log of the
[quickstart](../quickstart.md) reports `Parallel chunks: 20`. Below two
chunks' worth, the startup cost of spawning and synchronizing several
workers would dominate.

## Worker coordination

- A goroutine pool of size `RX_WORKERS` (default: the smaller of
  `NumCPU` and `RX_MAX_SUBPROCESSES`) consumes chunks from a channel
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

A rare literal over a 6.3 GB log in the page cache, on a 16-core Mac,
by `RX_WORKERS`:

- 1 worker: 1.45 s
- 2 workers: 1.03 s (1.41×)
- 4 workers: 0.52 s (2.80×)
- 8 workers: 0.46 s (3.18×)
- 16 workers: 0.50 s (plateau)

Past 4-8 workers the gains stop: the scan is bound by memory bandwidth
and by the work outside it. See [performance](../performance.md#worker-scaling).

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
| `RX_WORKERS` | Goroutine pool size, from 1 to 256 (a larger value is used as 256). Default: the smaller of `NumCPU` and `RX_MAX_SUBPROCESSES`. |
| `RX_MIN_CHUNK_SIZE_MB` | Smallest chunk. Default: `20`. A file below twice this size is one chunk. |
| `RX_MAX_SUBPROCESSES` | Most chunks per file, and the pool size when `RX_WORKERS` is unset. Default: `20`. |

The newline search at a chunk boundary reads 256 KB at a time until it
finds a newline; that read size is fixed.

## Implications

- **Very long lines mean fewer chunks.** A line longer than the
  distance between two desired boundaries (some CSV exports, JSON lines
  with embedded blobs, minified bundles) merges the chunks it covers.
  The answer is the same as a scan in one piece; only the parallelism
  drops, and a file that is one line is scanned by one worker.
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
