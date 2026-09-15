# Line indexes

A **line index** is a JSON file stored under `~/.cache/rx/indexes/`
that maps line numbers to byte offsets via sparse checkpoints. Once
built, an index lets `rx` answer "where does line N start?" by reading
at most one checkpoint step (1 MB by default) from the nearest
checkpoint, whatever the file size.

## The file format

An index is stored as a JSON document with the full `UnifiedFileIndex`
schema, format version 5. The critical field is `line_index`. The
start of the index `rx samples` built for a 465 MB log (most of the
other members trimmed; `rx index --info --json` prints all of them):

```json
{
  "version": 5,
  "source_path": "/var/log/app.log-2025121008",
  "source_modified_at": "2025-12-27T17:30:57.775888",
  "source_size_bytes": 487561499,
  "created_at": "2026-10-03T04:04:39.896939Z",
  "build_time_seconds": 0.123523083,
  "source_inode": 121515888,
  "source_changed_at": "2026-04-18T20:43:30.904417",
  "source_fingerprint": "8bace9b40fccf36b77b65e88908e85458cf74ea44324c4aaff8b374adb8ddfe8",
  "file_type": "text",
  "index_step_bytes": 1048576,
  "analysis_performed": false,
  "line_count": 1436842,
  "line_index": [[1, 0], [4713, 1048616], [7382, 2097336], [10118, 3157429]]
}
```

The real `line_index` has 429 entries. Each checkpoint is a
`[line_number, byte_offset]` pair; a seekable zstd index adds the frame
as a third element, `[line_number, byte_offset, frame_index]`. Between
checkpoints, no data is recorded — to find line 6000 you seek to the
checkpoint for line 4713 and scan forward.

### Sparse checkpoints

The index is **sparse**, not dense. Dense would mean one entry per
line: for a 3.5-million-line file, that's 3.5 million entries and the
index file would be huge. Sparse means one entry every
`index_step_bytes`: a fiftieth of `RX_LARGE_FILE_MB`, so 1 MB by
default.

Trade-off:

- Smaller step → more entries, larger index file, shorter forward
  scan from checkpoint to target
- Larger step → smaller index, longer forward scan

The default balances disk size against seek latency. The 465 MB log
above has 429 checkpoints in a 9.3 KB file, and `rx index --info` on it
takes 11 ms, process start included.

## How `rx` uses the index

### Line-number-to-byte seek

Given target line N:

1. Binary-search the `line_index` for the greatest checkpoint where
   `checkpoint.line_number <= N`
2. Seek the file to `checkpoint.byte_offset`
3. Read forward, counting `\n` bytes, until line N is reached

Step 3 scans at most about `index_step_bytes` bytes. On the 465 MB
log, `rx samples --lines=700000 --context=3` took 13 ms with the index
and 90 ms without it.

This is the core enabler of the bounded-read contract for `samples`:
a request reads from the nearest checkpoint to the last wanted line
instead of from byte 0. See [Performance — Bounded read contract](../performance.md#bounded-read-contract).

### Chunk-boundary line lookup (during trace)

A chunked trace numbers its matches itself: every chunk counts the
newlines it reads. Only a scan cut short by `--max-results` leaves a
match without a line number. For such a match at byte offset B, `rx`
uses the index:

1. Binary-search the index for the largest checkpoint with
   `byte_offset <= B`
2. Seek to that checkpoint
3. Count `\n` bytes until reaching offset B
4. Return `checkpoint.line_number + newlines_seen`

Without an index the match keeps `absolute_line_number: -1`; with
`rx trace --no-index` the lines are counted from the start of the file
instead. See [line number resolution](../cli/trace.md#line-number-resolution).

### Negative line numbers

`rx samples --lines=-5` means "5 lines from the end". On a plain file
this is cheap with or without an index (`--lines=-50--1` on the 465 MB
log: 11 ms either way). On a gzip, bzip2, xz or plain zstd file, `rx`
streams the file once to count its lines first.

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

- **Cold build**: one sequential pass; 142 ms for the 465 MB log in the
  page cache, slower when the file must come from disk
- **Warm read**: 11 ms for `rx index --info` on that log, process start
  included
- **Memory**: the builder keeps only the sparse checkpoints, not one
  entry per line; peak RSS was 22 MB for the 465 MB log and 23 MB for a
  6.3 GB log (42.5 million lines, built in 2.2 s)

### `rx samples` builds one for you

A lookup in a large or compressed file builds an index if none is cached,
so the *second* lookup is fast — which is the case an index exists for.
The build runs without `--analyze`: anomaly detection is a separate
feature, nothing on the lookup path reads its output, and a full pass to
answer one line is work nobody asked for.

`--no-index` (or `RX_NO_INDEX=1`) turns it off for a caller that wants
the read to leave nothing behind; the lookup then streams, which is
slower and gives the same answer. `GET /v1/samples` behaves the same way
and reads the same variable — there is no query parameter for it, because
the decision belongs to whoever runs the server rather than to a caller.

A plain file is only indexed once it reaches the large-file threshold. A
compressed one is indexed whatever its size, because without an index
every lookup in it decompresses from the start.

rx-python does the same on both surfaces.

### When to build one yourself

Build an index when:

- You'll run `rx samples --lines=...` on the file more than once
- You'll run capped `rx trace` searches on the file — the matches a
  `--max-results` cap leaves unnumbered get their line numbers cheaply
- You want to know the line count (`rx index --info` reports it)

Don't bother when:

- The file is small (< 50 MB by default — `rx index` skips it and says
  `Skipped 1 files (below threshold or not text)`)
- You'll only read the file once
- You only need byte offsets (they don't need an index at all)

## Cache invalidation

An index is valid as long as its format version is the current one
(5) and the source file hasn't changed. `rx` checks:

1. `version` equals the current format version; an index of another
   version is treated as absent
2. The size matches `source_size_bytes`
3. The mtime matches `source_modified_at`
4. The inode and the ctime match `source_inode` and
   `source_changed_at`
5. A digest of the size plus the first and last 64 KiB matches
   `source_fingerprint`

Any mismatch → the index is treated as absent and rebuilt by the next
call that wants one.

There is **no TTL**. A cache entry written last year is still valid if
the source file hasn't been touched. See [caching](caching.md) for the
full cache lifecycle.

### Manual invalidation

```bash
# Remove the cache file.
rx index /var/log/audit-2026-03.log --delete

# Or force a rebuild on the next call.
rx index /var/log/audit-2026-03.log --force

# Or run a trace that neither reads nor writes an index.
rx trace "error" /var/log/audit-2026-03.log --no-index
```

## Implications

### Latency on the first query

The first line lookup in a large file pays the build cost — one full
read of the file. Every subsequent query starts at a checkpoint. Script
this in deployment pipelines:

```bash
# At deploy time, pre-warm the cache.
find /var/log/ -name "*.log" -size +50M -exec rx index {} \;
```

### Index files are small

An index has about one checkpoint per MB of source: 9.3 KB for the
465 MB log above. An analysis adds its anomalies to the file.

### Compressed-file indexes are different

For compressed files, the `line_index` entries point into the
**decompressed** content, but seeking in a compressed stream requires
decompressing up to the byte offset. For `.gz`, `.bz2`, `.xz`, and
plain `.zst`, `rx samples --lines` streams the whole file for every
lookup and does not use the index. `rx samples --offsets` decompresses
up to the last offset and uses the index only to start counting lines
at the nearest checkpoint.

For **seekable zstd** (produced by `rx compress`), each checkpoint also
names its frame, so a lookup decompresses only the frames that hold the
wanted lines. See [compression](compression.md).

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
