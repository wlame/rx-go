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
| **seekable zstd** | `.zst` | **yes** | **no** (see below) | **yes (fast)** | **yes, by frame** |

## How rx decides what a file is

Every command and every HTTP route — `trace`, `samples`, `index`,
`compress`, `GET /v1/tree` — decides what a file is by one rule, read
from the file's own bytes:

- **Format: the magic bytes, never the name.** A gzip (`1f 8b`), bzip2
  (`BZh`), xz (`fd 37 7a 58 5a 00`) or zstd (`28 b5 2f fd`, or a zstd
  skippable frame, which pzstd and an empty seekable file start with)
  signature names the format. A file with none of them is plain text,
  whatever its extension. So a text file named `app.log.gz` is searched
  as text, and a gzip file named `app.log` is decompressed. When the
  extension and the bytes disagree, the bytes win.
- **Seekable zstd: the seek table.** A zstd file is seekable when it ends
  with a seek table that describes it, whatever its name (`.zst`,
  `.zstd` or none).
- **Text: the first 8 KiB of the text.** A NUL byte in the first 8 KiB
  of the file's text means the file is not text. For a compressed file
  the rule reads the decompressed text, so a plain file and its
  compressed copies are classified alike. This is what refuses a
  `.tar.gz` (every tar header is padded with NUL bytes), a binary file,
  and UTF-16 text, which writes every ASCII character next to a NUL
  (named "UTF-16" in the reason when the text starts with a UTF-16
  byte-order mark). A NUL byte after the first 8 KiB does not make a
  file binary: it stays part of its line.

A file that is not text is refused everywhere with a reason that
starts with "not a text file", for example
`not a text file: a NUL byte in the first 8 KiB of its decompressed text`.
`rx trace` and `rx index` skip it and say why; `rx samples` exits 2
(`GET /v1/samples` answers 400); `rx compress` refuses it
(`POST /v1/compress` answers 400); `POST /v1/index` answers 400. No line
index is ever built for it. `GET /v1/tree` reports `is_text: false`.

A compressed file whose first bytes of text cannot be decompressed is
not refused by the rule: the command that reads it reports the damage
where it meets it.

The rule decompresses a zstd file, seekable or not, a stream at a time
and holds one frame's window, at most 16 MiB, whatever size the frame
declares: classifying a file, also every file of a `GET /v1/tree`
listing, costs a few mebibytes at most. A zstd file whose first frame
declares a larger window (`zstd --long` writes 128 MiB) is taken for
text without being probed; the command that reads it decompresses it
with the window it needs. The first mebibyte an index build reads to
detect the timestamp format is decompressed the same way with at most a
128 MiB window, the most `zstd -d` accepts without `--long=N`; a file
whose frames need more cannot be indexed, and the build says why.

An xz file is held to the same limits. Each xz block header names the
dictionary its decoder reserves before decoding the block (`xz -6`
writes 8 MiB, `xz -9` 64 MiB), and a header can name up to 4 GiB
whatever the file's size, so rx reads the xz container itself and
checks every block header before the dictionary is reserved. Deciding
what a file is decodes blocks that name at most 16 MiB; a file whose
first block names more is taken for text without being probed. Every
command reads an xz file with at most a 128 MiB dictionary and refuses
a block that names more.

## Why compressed files lose random access

A compressed stream is a state machine. Decompressing byte N typically
requires all previous bytes. Seeking halfway into a 1 GB `.gz` file
requires decompressing the first 500 MB of output first.

This has three consequences for `rx`:

1. **Byte offsets are positions in the decompressed text, and
   reaching one means decompressing up to it.** Every offset `rx` reports for a
   compressed file — a trace match, the `lines` map of `samples` — is a
   position in the decompressed text, the same number the plain copy of
   the file gives. A `NullPointerException` match in a 465 MB log is at
   offset 403366791, line 1191541, in the plain file, its `.gz` copy and
   a seekable `.zst` made from it alike. `rx samples --offsets` takes
   the same positions and answers with the same lines; on a gzip,
   bzip2, xz or plain zstd file it decompresses from the first byte up
   to the line holding the last offset.
2. **Line-number lookups require streaming.** `rx samples --lines=N`
   on a `.gz` file streams the decompressor from the start, counts
   newlines, and captures content at line N. Sub-linear time is
   possible only at the granularity of the compression block
   structure, which for gzip is the whole stream.
3. **Parallel trace can't split a compressed stream.** Workers would
   need independent starting points, which don't exist. `rx trace`
   on a gzip, bzip2, xz or plain zstd file runs in single-worker stream
   mode. A seekable zstd file has those starting points — its frames —
   and is scanned frame-parallel.

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

### When rx trusts a seek table

A file is seekable for rx when it is named `.zst` and ends with a seek
table that describes it: the table's skippable frame runs to the end of
the file, the frames it lists, laid end to end, end where the table
starts, and each starts with a zstd frame header whose content size,
when the header records one, is the table's decompressed size for it.
Checking that reads the table and one frame header (at most 18 bytes)
per frame, never a frame's data.

rx reads the seek-table footer (its last 9 bytes) in two layouts:

| Layout | Fields, in order (bytes) | Checksum flag |
|---|---|---|
| zstd seekable format specification | frame count (4), descriptor (1), magic `0x8F92EAB1` (4) | descriptor bit 7 |
| legacy rx (rx-go up to v0.3.0, rx-python) | magic `0x8F92EAB1` (4), frame count (4), flags (1) | flags bit 0 |

All fields are little-endian. When the specification's magic is in
place, rx reads that layout first, so a file written by another tool
that follows the specification (t2sz, the zstd `contrib` library) is
seekable for rx. A footer whose checksum flag is set has 12-byte
entries (two sizes and a checksum); rx reads past the checksums and
does not check them. A specification footer with a reserved
descriptor bit (6 to 2) set is not trusted.

A table that does not add up is not used. Two seekable files joined
with `cat` end with the second file's table alone, and a damaged table
places the frames wrongly; either way the file is still a valid zstd
stream, so `rx trace`, `rx samples`, `rx index` and `rx compress` read it
as plain zstd and answer what its text holds (`file_chunks` 1), and
`rx trace` logs a `seek_table_mismatch` warning. A damaged frame is a
different case: the table still describes the file, and the frame
itself does not decompress. `rx trace` searches around it (see below),
and `rx samples` and `rx index` stop at it with an error that names the
frame (`seekable zstd frame is damaged: frame N`).

### How `rx compress` writes it

```bash
rx compress /var/log/audit-2026-03.log --frame-size=4M
```

- Reads the source in 4 MiB chunks (decompressed size)
- Encodes each chunk as an independent zstd frame
- Appends the seek table as a skippable frame, its footer in the
  layout of the zstd seekable format specification (frame count,
  descriptor, magic; no checksums), so tools that follow the
  specification can seek in it too

The resulting `.zst` file is:

- **Readable by any zstd decoder** as a regular zstd stream (the
  skippable frame is ignored by non-seekable readers)
- **Seekable via `rx samples`** on a line-number basis
- **Larger** than a monolithic zstd file, because each frame restarts
  the compression context: on a 465 MB application log, 4 MiB frames
  at the default level gave 47.8 MB where `zstd -3` gave 41.6 MB (15%
  more)

### Frame size trade-offs

Measured on a 465 MB application log at the default level:

| Frame size | Frames | Output | Ratio | Seek granularity |
|---|---:|---:|---:|---|
| 1 MiB | 429 | 95.3 MB | 5.11× | 1 MiB per seek |
| 4 MiB (default) | 114 | 47.8 MB | 10.19× | 4 MiB per seek |
| 16 MiB | 29 | 32.7 MB | 14.91× | 16 MiB per seek |
| 64 MiB | 8 | 29.7 MB | 16.43× | 64 MiB per seek |

The ratio depends on the content; this log has some very long lines,
which small frames cut into many pieces.

For log files queried by line number, 1-4 MiB frames are usually
right. For archive storage queried sequentially, larger frames save
space.

### Compression level

`--level` accepts 1-22 (default 3), but the encoder `rx` uses
(`klauspost/compress`) has four settings, and each level maps to one
of them:

| Levels | Encoder setting | 465 MB log, 1 worker | Output | Ratio |
|---|---|---:|---:|---:|
| 1 | fastest | 0.47 s | 82.9 MB | 5.88× |
| 2-5 | default | 0.71 s | 47.8 MB | 10.19× |
| 6-9 | better compression | 0.97 s | 44.4 MB | 10.99× |
| 10-22 | best compression | 4.84 s | 43.2 MB | 11.27× |

Two levels in the same row write byte-identical files: `--level=19`
is `--level=10`, and `--level=5` is the default. For the smallest
archive, `zstd -19` (13.5 MB on the same log) is far smaller, but it
writes one frame and gives up random access.

## Parallel encoding

`rx compress --workers=4` uses 4 encoder goroutines. On a 16-core Mac,
4 workers took the 465 MB log from 0.71 s to 0.29 s at the default
level, and from 4.77 s to 1.71 s at level 19. Beyond the core count,
disk bandwidth and output serialization become the bottleneck.

```bash
time rx compress /var/log/audit-2026-03.log --workers=4 --frame-size=4M
```

The output is the same whatever the worker count.

## Trace on compressed files

`rx trace` can scan compressed files:

- A gzip, bzip2, xz or plain zstd file is one stream, decompressed and
  piped to a single `ripgrep`; `file_chunks` reports 1
- A seekable zstd file is scanned frame-parallel: batches of frames go
  to separate workers, and `file_chunks` reports the frame count (114
  for the log above, `Parallel chunks: 114` in human output). Frames
  need not end at line breaks: where another encoder cut a frame
  mid-line, or a line is longer than a frame, a worker skips the end
  of the line its batch begins inside and reads on into the next
  frames to finish its own last line, so every line is matched whole,
  by one worker
- Either way, offsets and line numbers are those of the decompressed
  text
- A damaged seekable zstd file is searched around the damage. Frames
  decompress independently, so a frame whose bytes no longer decompress
  (a bad sector, a partial copy) costs only the lines that touch it:
  the lines inside it, the line running into it and the line running
  out of it. Every other line is searched, in every batch. The file is
  listed in `skipped_files`, a warning names the damaged frames
  (`seekable_damaged_frames`), the matches after the first damaged
  frame have `absolute_line_number` -1 (its line count is lost), and
  the answer is never written to the trace cache. A gzip, bzip2, xz or
  plain zstd stream that breaks keeps the matches found before the
  break and is listed in `skipped_files` the same way

## Samples on compressed files

`rx samples --lines=N --context=C file.gz`:

- Opens the decompressor
- Streams through decompressed output, counting `\n`
- Captures lines in the window `[N-C, N+C]`
- Reads on to the end of the stream

So the cost is one full decompression whatever the line: on the 113 MB
`.gz` of a 465 MB log, line 5 and line 1,191,541 both took 1.55 s.

`rx samples` builds and stores a line index for a compressed file the
first time it is asked about one (unless `--no-index` or `RX_NO_INDEX`
is set), so that call reads the whole file once. Only a seekable zstd
file uses it; a gzip, bzip2, xz or plain zstd lookup streams whether
an index exists or not. On the 113 MB `.gz` of a 465 MB log, line
1191541 took 3.0 s the first time (stream plus index build) and 1.6 s
after.

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
# Encode a log as seekable zstd; this also builds its index.
rx compress /var/log/huge-audit.log --level=9 --frame-size=2M

# Rebuild the index later, if it was removed.
rx index /var/log/huge-audit.log.zst

# Retrieve a line.
rx samples /var/log/huge-audit.log.zst --lines=5000000 --context=3
```

The index stores frame-boundary info alongside line-offset
checkpoints, so the `samples` call skips directly to the enclosing
frame.

## Related concepts

- [Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md) —
  what a byte offset means in compressed data
- [Line indexes](line-indexes.md) — how line lookups work with and
  without random access
- [Caching](caching.md) — compressed-file indexes still cache
