# Line indexes

A **line index** is a JSON file stored under `~/.cache/rx/indexes/`
that maps line numbers to byte offsets via sparse checkpoints. Once
built, an index lets `rx` answer "where does line N start?" in O(1)
regardless of file size.

## The file format

An index is stored as a JSON document with the full `UnifiedFileIndex`
schema. The critical field is `line_index`:

```json
{
  "version":          1,
  "source_path":      "/var/log/audit-2026-03.log",
  "source_modified_at": "2026-04-18T13:42:10.123456",
  "source_size_bytes":  582137856,
  "created_at":       "2026-04-18T14:22:03.501729",
  "file_type":        "text",
  "line_index": [
    { "line_number": 1,       "byte_offset": 0 },
    { "line_number": 12500,   "byte_offset": 2097152 },
    { "line_number": 25000,   "byte_offset": 4194304 },
    { "line_number": 37500,   "byte_offset": 6291456 }
  ],
  "index_step_bytes": 2097152,
  "line_count":       3528914
}
```

Each checkpoint is a `(line_number, byte_offset)` pair. Between
checkpoints, no data is recorded — to find an offset between
`line 12500` and `line 25000`, you seek to the earlier checkpoint and
scan forward.

### Sparse checkpoints

The index is **sparse**, not dense. Dense would mean one entry per
line: for a 3.5-million-line file, that's 3.5 million entries and the
index file would be huge. Sparse means one entry every
`index_step_bytes` (default typically 2 MB).

Trade-off:

- Smaller step → more entries, larger index file, shorter forward
  scan from checkpoint to target
- Larger step → smaller index, longer forward scan

The default balances disk size against seek latency. For a 260 MB
file with typical line lengths, the index ends up with ~130
checkpoints and loads in ~10 ms.

## How `rx` uses the index

### Line-number-to-byte seek

Given target line N:

1. Binary-search the `line_index` for the greatest checkpoint where
   `checkpoint.line_number <= N`
2. Seek the file to `checkpoint.byte_offset`
3. Read forward, counting `\n` bytes, until line N is reached

Step 3 scans at most `index_step_bytes` bytes. Total wall-clock
time is dominated by the JSON parse in step 1 (a few milliseconds)
plus the forward scan (tens of microseconds per MB).

This is the core enabler of the bounded-read contract for `samples`:
a request for `--lines=5000000-5001000` on a 1.3 GB file reads only
the few KB between the nearest checkpoint and line 5 001 000, instead
of the full file. See [Performance — Bounded read contract](../performance.md#bounded-read-contract).

### Chunk-boundary line lookup (during trace)

When a chunked trace reports a match at byte offset B, `rx` computes
the absolute line number:

1. Binary-search the index for the largest checkpoint with
   `byte_offset <= B`
2. Seek to that checkpoint
3. Count `\n` bytes until reaching offset B
4. Return `checkpoint.line_number + newlines_seen`

Without an index, this computation becomes a scan from the start of
the file. The chunked trace path handles absolute line numbers
correctly either way, but the indexed path is much faster.

### Negative line numbers

`rx samples --lines=-5` means "5 lines from the end". This requires
knowing the file's total line count:

- If the index has `line_count`, use it directly
- Otherwise, compute the count by scanning the whole file once (slow)

Always build an index before using negative line addresses on large
files.

## Building an index

```bash
# Default: index files >= 50 MB.
rx index /var/log/audit-2026-03.log
```

See [`rx index`](../cli/line-index.md) for all the flags.

The builder:

1. Opens the file and reads through it
2. Emits a checkpoint at byte 0 (line 1) and whenever the byte
   position has advanced `index_step_bytes` beyond the last checkpoint
3. Counts lines along the way
4. Optionally (when `--analyze` is set) gathers line-length statistics
5. Writes the JSON file atomically (temp file + rename) to
   `~/.cache/rx/indexes/<safe-name>_<hash16>.json`

### Cost

- **Cold build**: single-threaded, roughly real-time-per-GB on fast
  NVMe, slower on HDD or network mounts
- **Warm read**: ~10 ms regardless of source file size; JSON parse
  dominates
- **Memory**: the builder holds the full line-offset slice in memory
  during construction. On a 260 MB source, peak memory is around
  256 MB RSS.

### When to build

Build an index when:

- You'll run `rx samples --lines=...` on the file more than once
- You'll run `rx trace` on the file with parallel chunking — absolute
  line numbers become nearly free
- You want to know the line count (`rx index --info` reports it)

Don't bother when:

- The file is small (< 50 MB by default — below-threshold builds are
  silently skipped)
- You'll only read the file once
- You only need byte offsets (they don't need an index at all)

## Cache invalidation

An index is valid as long as the source file hasn't changed. `rx`
checks:

1. The source file's current mtime matches the cached
   `source_modified_at`
2. The source file's current size matches `source_size_bytes`

Either mismatch → the cache is treated as stale and rebuilt on the
next index/trace/samples call.

There is **no TTL**. A cache entry written last year is still valid if
the source file hasn't been touched. See [caching](caching.md) for the
full cache lifecycle.

### Manual invalidation

```bash
# Remove the cache file.
rx index /var/log/audit-2026-03.log --delete

# Or force a rebuild on the next call.
rx index /var/log/audit-2026-03.log --force

# Or disable the cache entirely for one run.
rx trace "error" /var/log/audit-2026-03.log --no-index
```

## Implications

### Latency on the first query

The first query against a large file pays the cold-build cost
(seconds to minutes). Every subsequent query is near-free. Script this
in deployment pipelines:

```bash
# At deploy time, pre-warm the cache.
find /var/log/ -name "*.log" -size +50M -exec rx index {} \;
```

### Index files are small

A typical index file for a multi-GB source is in the low-hundreds-of-KB
range. Caching 1000 large log files costs roughly 100 MB of disk.

### Compressed-file indexes are different

For compressed files, the `line_index` entries point into the
**decompressed** content, but seeking in a compressed stream requires
decompressing up to the byte offset. For `.gz`, `.bz2`, `.xz`, and
plain `.zst`, this ends up being a linear scan anyway — the index
mostly helps with line-number counting rather than random access.

For **seekable zstd** (produced by `rx compress`), the index can also
encode frame boundaries, making random-access decompression
effectively O(1) to the nearest frame. See [compression](compression.md).

## Implementation notes

- Line numbers in the index are **1-based** (line 1 is the first line
  of the file)
- Byte offsets are **inclusive start** positions — `byte_offset = 0`
  means the file start
- The last line of a file may or may not end with `\n`; `rx` handles
  both cases

## Related concepts

- [Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md)
- [Caching](caching.md)
- [Chunking](chunking.md)
- [Compression](compression.md)
