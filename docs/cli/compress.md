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

The trade-off: seekable zstd is larger than a monolithic zstd file,
because each frame restarts the compression context. On a 465 MB
application log, the default 4 MiB frames gave 47.8 MB where `zstd -3`
gave 41.6 MB. For files that will be searched, sampled, or
line-indexed repeatedly, the random-access gain outweighs the size
cost.

`rx compress` encodes the bytes of the input file as they are. A
`.gz`, `.bz2`, `.xz` or `.zst` input is **not** decompressed first: the
result is a seekable zstd of the compressed bytes, which `rx` cannot
search as text. Decompress first (`gunzip -k app.log.gz`), then
compress the plain file.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-o`, `--output` | `string` | `<PATH>.zst` | Output file path (single-file only) |
| `--output-dir` | `string` | — | Output directory; uses `<basename>.zst` inside |
| `--frame-size` | `string` | `4M` | Target frame size: `B`, `K`/`KB`, `M`/`MB`, `G`/`GB` |
| `-l`, `--level` | `int` | `3` | zstd level `1`-`22`; the encoder has four settings: `1`, `2`-`5`, `6`-`9`, `10`-`22` |
| `-f`, `--force` | `bool` | `false` | Overwrite existing output |
| `--build-index` | `bool` | `true` | Build the frame index for the `.zst` that was written |
| `--no-index` | `bool` | `false` | Turns `--build-index` off |
| `--workers` | `int` | `1` | Parallel encoder goroutines (1..N) |
| `--json` | `bool` | `false` | Emit machine-readable JSON |

`--output` and `--output-dir` are mutually exclusive. `--output` is
only valid with exactly one input path.

When search roots are configured, the destination is validated against
them exactly like the input path: `--output`, `--output-dir` and the
derived `<PATH>.zst` default must all resolve inside a root, or the file
is reported as failed and nothing is written. See
[the path sandbox](../concepts/security.md#path-sandbox-search-root).

### Frame size parsing

The `--frame-size` value is parsed case-insensitively. These are all
equivalent to 4 MiB:

```text
4M      4MB     4m      4mb     4194304
```

A bare integer is interpreted as bytes. Fractional values are accepted:
`1.5M` = 1572864 bytes.

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
not exist, 4 when every failure was a path outside the roots, and 1 when
the failures were of different kinds.

## Examples

### Default encode

```bash
rx compress /var/log/audit-2026-03.log
```

Produces `/var/log/audit-2026-03.log.zst` with 4 MiB frames at zstd
level 3, and its index. Stdout, for a 465 MB application log:

```text
wrote /var/log/audit-2026-03.log.zst (487561499 bytes → 47808474 bytes, 10.19x) in 114 frames
```

The 10.19x ratio reads as "source-size / compressed-size" — i.e., the
file shrank to about 10% of its original size.

### Tune the frame size

```bash
rx compress /var/log/audit-2026-03.log --frame-size=1M
```

1 MiB frames — about four times as many frames and a worse ratio: on
the 465 MB log, 429 frames and 95.3 MB (5.11x) against 114 frames and
47.8 MB at 4 MiB. Smaller frames mean finer-grained random access: a
line-index lookup decompresses only the enclosing 1 MiB instead of
4 MiB. See [frame size trade-offs](../concepts/compression.md#frame-size-trade-offs).

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

The encoder has four settings, and levels 10 to 22 all use the
strongest one: `--level=19` writes the same file as `--level=10`. On
the 465 MB log it took 4.84 s against 0.71 s for the default and wrote
43.2 MB against 47.8 MB. See
[compression levels](../concepts/compression.md#compression-level).

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

Spawns 4 encoder goroutines, each producing one frame at a time. On a
16-core Mac the 465 MB log took 0.29 s instead of 0.71 s at the default
level, and 1.71 s instead of 4.77 s at level 19. The output is the same
for any worker count. I/O is single-threaded at the output side, so
extreme worker counts provide diminishing returns.

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
per input, with `action`, `input`, `output`, `success`,
`compressed_size`, `decompressed_size`, `frame_count`,
`compression_ratio` and, when an index was built, `index`:

```json
{
  "files": [
    {
      "action": "compress",
      "compressed_size": 135,
      "compression_ratio": 3.48,
      "decompressed_size": 471,
      "frame_count": 1,
      "index": {
        "frame_count": 1,
        "line_count": 30
      },
      "input": "/var/log/small.log",
      "output": "/var/log/small.log.zst",
      "success": true
    }
  ]
}
```

The entries carry no `cli_command`; that member belongs to the HTTP
answers.

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

- **Encode throughput**: on a 16-core Mac with one worker, about
  1 GB/s at level 1, 690 MB/s at the default level, 500 MB/s at levels
  6-9 and 100 MB/s at levels 10-22.
- **Memory**: one frame buffer per worker. At `--frame-size=4M` and
  `--workers=8`, peak memory is roughly 100-200 MB.
- **Output size**: larger than monolithic zstd, because each frame
  restarts the compression context; how much depends on the frame size
  (see [frame size trade-offs](../concepts/compression.md#frame-size-trade-offs)).
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

!!! note "Four encoder settings, not 22 levels"
    `1`, `2`-`5`, `6`-`9` and `10`-`22` each map to one encoder
    setting. On the 465 MB log, `6`-`9` saved 7% of the size for 1.4×
    the time, and `10`-`22` another 3% for 5× the time of `6`-`9`.

!!! warning "Output file permissions"
    `--output-dir` creates missing directories with permissions
    `0750` (owner + group read/execute). Other users on the system
    cannot enter the directory unless they share the group. Set
    permissions manually after compression if you need wider access.

!!! warning "Compressed input is not decompressed"
    `rx compress file.gz` writes `file.gz.zst`, a seekable zstd of the
    gzip bytes; its index counts lines in those bytes, not in the text.
    Decompress the file first.

## See also

- [concepts/compression](../concepts/compression.md) — supported formats, seekable-zstd details
- [`rx samples`](samples.md) — random-access reads on compressed files
- [`rx index`](line-index.md) — build a line index after compression
- [api/endpoints/compress](../api/endpoints/compress.md) — compression over HTTP

## The index it builds

With `--build-index` (the default) the `.zst` is indexed as soon as it is
written, and the JSON reports what was built:

```json
"index": { "line_count": 50000, "frame_count": 38 }
```

The index is a frame table: which lines each zstd frame holds. That is
what lets `rx samples big.log.zst --lines=25000` decompress one frame
rather than the whole stream, and it is written to the shared cache under
`~/.cache/rx/indexes/`, so rx-python reads it and the other way round.

A failure to index is reported as `index_error` and does not fail the
command: the compressed file is correct and usable, and `rx index` can
build the index later.

`--no-index` writes no index and reports neither key.
