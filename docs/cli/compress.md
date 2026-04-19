# `rx compress`

Encode a file as seekable zstd — a zstd variant with random-access
decompression.

## Synopsis

```text
rx compress PATH [PATH ...] [flags]
rx compress PATH -o OUTPUT.zst [flags]
rx compress PATH --output-dir=DIR [flags]
```

## Description

`rx compress` produces **seekable zstd** files: streams written as
multiple independent frames, with a seek table appended in a skippable
frame. Any compliant zstd decoder can read the file as plain zstd; a
seek-aware decoder (including `rx samples`) can jump directly to any
frame without decompressing everything before it.

The trade-off: seekable zstd is ~4-5% larger than a monolithic zstd
file at the same level, because smaller frames have less context for
the dictionary. For files that will be searched, sampled, or
line-indexed repeatedly, the random-access gain far outweighs the size
cost.

The input file can itself be compressed (`gzip`, `bzip2`, `xz`, or
plain `zstd`). `rx compress` decompresses on the fly and re-encodes to
seekable zstd.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-o`, `--output` | `string` | `<PATH>.zst` | Output file path (single-file only) |
| `--output-dir` | `string` | — | Output directory; uses `<basename>.zst` inside |
| `--frame-size` | `string` | `4M` | Target frame size: `B`, `K`/`KB`, `M`/`MB`, `G`/`GB` |
| `-l`, `--level` | `int` | `3` | zstd level: `1` (fast) .. `22` (slowest, smallest) |
| `-f`, `--force` | `bool` | `false` | Overwrite existing output |
| `--build-index` | `bool` | `true` | Reserved: register for post-compress line indexing (CLI not yet wired; run `rx index` afterward) |
| `--no-index` | `bool` | `false` | Reserved: paired with `--build-index` (see above) |
| `--workers` | `int` | `1` | Parallel encoder goroutines (1..N) |
| `--json` | `bool` | `false` | Emit machine-readable JSON |

`--output` and `--output-dir` are mutually exclusive. `--output` is
only valid with exactly one input path.

### Frame size parsing

The `--frame-size` value is parsed case-insensitively. These are all
equivalent to 4 MiB:

```text
4M      4MB     4m      4mb     4194304
```

A bare integer is interpreted as bytes. Fractional values are accepted:
`1.5M` = 1572864 bytes.

## Examples

### Default encode

```bash
rx compress /var/log/audit-2026-03.log
```

Produces `/var/log/audit-2026-03.log.zst` with 4 MiB frames at zstd
level 3. Stdout:

```text
wrote /var/log/audit-2026-03.log.zst (582137856 bytes → 40231680 bytes, 14.47x) in 139 frames
```

The 14.47x ratio reads as "source-size / compressed-size" — i.e., the
file shrank to about 7% of its original size.

### Tune the frame size

```bash
rx compress /var/log/audit-2026-03.log --frame-size=1M
```

1 MiB frames — four times as many frames, four times the seek-table
size, slightly worse compression ratio (~1-2% extra). Smaller frames
mean finer-grained random access: a line-index lookup decompresses
only the enclosing 1 MiB instead of 4 MiB.

Use smaller frames when:

- You'll be doing many small-range samples across the file
- Memory is tight and decompressing 4 MiB at a time is too much
- The file is written once and queried often

Use larger frames (`8M`, `16M`) when:

- Compression ratio matters more than seek granularity
- The file is mostly sequentially scanned rather than randomly accessed

### Higher compression level

```bash
rx compress /var/log/audit-2026-03.log --level=19
```

Levels 19+ are markedly slower (roughly 5-10× encoding time vs level 3)
and produce marginally smaller output (~5-15% smaller). Worth it for
archival data that won't be re-encoded.

### Custom output path

```bash
rx compress /var/log/audit-2026-03.log -o /backup/audit-2026-03.zst
```

### Output directory

```bash
rx compress /var/log/audit-*.log --output-dir=/backup/logs/
```

Each input file is written to `/backup/logs/<basename>.zst`. The
directory is auto-created with permissions `0750` if it doesn't exist.

### Parallel encoding

```bash
rx compress /var/log/audit-2026-03.log --workers=4
```

Spawns 4 encoder goroutines, each producing one frame at a time. Useful
on multi-core hardware — near-linear speedup up to the physical core
count for the encoding step. I/O is single-threaded at the output side,
so extreme worker counts provide diminishing returns.

### Force overwrite and re-encode

```bash
rx compress existing-output.zst --force --level=9
```

Overwrite a previously-compressed file with a higher-level encoding.
Without `--force`, `rx` errors out if the output exists.

### JSON wrapper

```bash
rx compress /var/log/audit-*.log --json > compress-report.json
```

Produces a `files[]` wrapper listing every encode outcome — one entry
per input, with `success`, `output`, `compressed_size`,
`decompressed_size`, `frame_count`, `compression_ratio`, and a
`cli_command` field reproducing the effective invocation.

## How it works

### Native zstd encoding

`rx` uses `github.com/klauspost/compress/zstd` for encoding. The file
is read in frame-size chunks; each chunk becomes one independent zstd
frame. After all data frames, a single skippable frame holds the seek
table — a list of `(compressed_size, decompressed_size)` pairs, one
per frame.

Any decoder that understands zstd can read the file without knowing
about the seek table. Decoders that support the seekable extension
(like `rx samples`) parse the trailing frame and use it to skip to
arbitrary frames.

### Parallel encoding

With `--workers > 1`, `rx` uses a producer/consumer pipeline:

- The producer reads the source file in frame-size chunks
- A pool of `--workers` encoder goroutines picks up chunks and encodes
  them in parallel
- A single writer goroutine serializes frame output in order

Frame-size granularity is preserved. Inter-frame ordering is preserved.
The seek table is computed from the final frame layout.

### Output layout

```text
[zstd frame #0]  — first 4 MB of source, independently decompressable
[zstd frame #1]  — bytes 4M..8M of source
...
[zstd frame #N]  — last chunk (may be smaller than 4M)
[skippable frame] — seek table: [(c0_sz, d0_sz), (c1_sz, d1_sz), ...]
```

The skippable frame's magic number is part of the zstd spec; non-seekable
decoders ignore it.

### Performance characteristics

- **Encode throughput**: roughly 100-300 MB/s per worker at level 3 on
  modern hardware. Faster levels are IO-bound; slower levels are
  CPU-bound.
- **Memory**: one frame buffer per worker. At `--frame-size=4M` and
  `--workers=8`, peak memory is roughly 100-200 MB.
- **Output size**: typically 4-5% larger than monolithic zstd at the
  same level, because each frame restarts the dictionary. Real-world
  log data often shows 10x-30x ratio regardless of seekable vs
  monolithic.
- **Parallelism**: scales near-linearly to the physical core count;
  beyond that the output serialization and disk bandwidth become the
  bottleneck.

## Tips and gotchas

!!! tip "Encode once, query forever"
    Seekable zstd makes sense when you'll search or sample the file
    repeatedly. For archive-and-forget storage, regular zstd at a
    higher level (`zstd -19`) saves more space.

!!! warning "Frame size affects seek granularity"
    Each frame is the smallest unit of random access. A 4 MiB frame
    means retrieving one line requires decompressing up to 4 MiB. For
    latency-sensitive query patterns, prefer smaller frames
    (`--frame-size=1M` or even `512K`).

!!! note "Compression level is linear-compression but quadratic-time"
    Going from level 3 → 9 typically saves ~5-10% size at ~3-4× encode
    time. Level 19+ saves another ~5-10% at ~10× more time. Level 22
    (ultra) is almost always not worth it unless you're archiving.

!!! warning "Output file permissions"
    `--output-dir` creates missing directories with permissions
    `0750` (owner + group read/execute). Other users on the system
    cannot enter the directory unless they share the group. Set
    permissions manually after compression if you need wider access.

!!! tip "Compressed input is decompressed on the fly"
    `rx compress file.gz` decompresses `file.gz` to memory-buffered
    frames and writes `file.gz.zst`. There is no intermediate
    uncompressed file on disk.

## See also

- [concepts/compression](../concepts/compression.md) — supported formats, seekable-zstd details
- [`rx samples`](samples.md) — random-access reads on compressed files
- [`rx index`](line-index.md) — build a line index after compression
- [api/endpoints/compress](../api/endpoints/compress.md) — compression over HTTP
