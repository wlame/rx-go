# Compression

`rx` reads and writes compressed files. Understanding which formats
support which operations is essential to using `rx` effectively on
archived data.

## Supported read formats

| Format | Extension | Random access | Works with `--offsets` | Works with `--lines` | Parallel trace |
|---|---|:-:|:-:|:-:|:-:|
| gzip | `.gz` | no | no | yes (slow for high lines) | no |
| bzip2 | `.bz2` | no | no | yes (slow) | no |
| xz | `.xz` | no | no | yes (slow) | no |
| zstd (plain) | `.zst` | no | no | yes (slow) | no |
| **seekable zstd** | `.zst` | **yes** | **no** (see below) | **yes (fast)** | **partial** |

Format detection is automatic based on the file's magic bytes, not
its extension.

## Why compressed files lose random access

A compressed stream is a state machine. Decompressing byte N typically
requires all previous bytes. Seeking halfway into a 1 GB `.gz` file
requires decompressing the first 500 MB of output first.

This has three consequences for `rx`:

1. **Byte offsets don't have decompressed semantics.** A byte offset
   in a compressed file refers to the *compressed* position, which
   isn't useful for content retrieval. `rx samples --offsets` rejects
   compressed files for this reason.
2. **Line-number lookups require streaming.** `rx samples --lines=N`
   on a `.gz` file streams the decompressor from the start, counts
   newlines, and captures content at line N. Sub-linear time is
   possible only at the granularity of the compression block
   structure, which for gzip is the whole stream.
3. **Parallel trace can't split a compressed stream.** Workers would
   need independent starting points, which don't exist. `rx trace`
   on compressed files runs in single-worker stream mode.

## Seekable zstd

**Seekable zstd** is zstd's standard workaround for random access. A
seekable file is written as:

```text
[independent zstd frame #0]
[independent zstd frame #1]
[independent zstd frame #2]
...
[skippable frame containing the seek table]
```

Each frame decompresses independently. The seek table at the end maps
frame number to `(compressed_size, decompressed_size)`. To read
decompressed byte D:

1. Walk the seek table to find the frame containing D
2. Seek to that frame's compressed offset
3. Decompress the frame (at most a few MB)
4. Extract the byte at the right offset within the frame's
   decompressed output

This is O(1) in file size — seek cost is dominated by the size of one
frame, not the whole file.

### How `rx compress` writes it

```bash
rx compress /var/log/audit-2026-03.log --frame-size=4M
```

- Reads the source in 4 MiB chunks (decompressed size)
- Encodes each chunk as an independent zstd frame
- Appends the seek table as a skippable frame

The resulting `.zst` file is:

- **Readable by any zstd decoder** as a regular zstd stream (the
  skippable frame is ignored by non-seekable readers)
- **Seekable via `rx samples`** on a line-number basis
- **~4-5% larger** than a monolithic zstd file at the same level
  (each frame restarts the dictionary, reducing compression)

### Frame size trade-offs

| Frame size | Compression ratio | Seek granularity | Memory to decompress one frame |
|---|---|---|---|
| 1 MiB | worst (smallest dictionary context) | best (1 MiB per seek) | lowest |
| 4 MiB (default) | balanced | 4 MiB per seek | moderate |
| 16 MiB | best | coarse (16 MiB per seek) | highest |
| 64 MiB | near-monolithic ratio | very coarse | large |

For log files queried by line number, 1-4 MiB frames are usually
right. For archive storage queried sequentially, larger frames save
space.

### Compression level

zstd level 1-22. Default: 3.

- Level 1-3: very fast encoding (~200-300 MB/s per worker), smaller
  ratio wins
- Level 9: slow encoding, 5-10% better ratio
- Level 19: very slow encoding, another 5% better
- Level 22 (ultra): rarely worth the time

## Parallel encoding

`rx compress --workers=4` uses 4 encoder goroutines. Scales
near-linearly to the physical core count. Beyond the core count, disk
bandwidth and output serialization become the bottleneck.

```bash
time rx compress /var/log/audit-2026-03.log --workers=4 --frame-size=4M
```

On a 582 MB fixture at level 3, this finishes in seconds on modern
hardware.

## Trace on compressed files

`rx trace` can scan compressed files, but:

- Only single-worker (no parallelism)
- Reads happen through the decompressor, one byte at a time
- Performance: roughly the decompressor's throughput
  (often 200-500 MB/s of decompressed output for gzip/zstd)

For seekable zstd, partial parallelism is possible by assigning
frames to workers. This is implemented for content retrieval but not
for scanning in the current release — `rx trace` on a seekable `.zst`
uses the single-worker path.

## Samples on compressed files

`rx samples --lines=N --context=C file.gz`:

- Opens the decompressor
- Streams through decompressed output, counting `\n`
- Captures lines in the window `[N-C, N+C]`
- Stops after the last requested line

Performance depends on the target line:

- Line 100: microseconds-milliseconds
- Line 1,000,000 on a 1 GB source: seconds

For seekable zstd with a built index, the seek is much faster —
`rx` jumps to the relevant frame via the seek table, decompresses only
that frame, and walks the target lines.

## Implications

### Archive strategy

Three reasonable strategies for storing large logs:

1. **Plain gzip/xz** — smallest size, slow random access. Good for
   cold archive (touched rarely).
2. **Plain zstd at high level** — smaller than gzip, faster
   decompression. Still slow random access.
3. **Seekable zstd (via `rx compress`)** — slightly larger than (1) and
   (2), fast random access. Best for archives that will be queried by
   line number.

For any archive that `rx` will read repeatedly, seekable zstd is the
right choice.

### Chaining `rx` operations

```bash
# Encode a log as seekable zstd.
rx compress /var/log/huge-audit.log --level=9 --frame-size=2M

# Build an index against the compressed output.
rx index /var/log/huge-audit.log.zst

# Retrieve a line.
rx samples /var/log/huge-audit.log.zst --lines=5000000 --context=3
```

The index stores frame-boundary info alongside line-offset
checkpoints, so the `samples` call skips directly to the enclosing
frame.

## Related concepts

- [Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md) —
  why byte offsets don't work on compressed data
- [Line indexes](line-indexes.md) — how line lookups work with and
  without random access
- [Caching](caching.md) — compressed-file indexes still cache
