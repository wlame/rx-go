# Line indexes

A **line index** is a JSON file stored under `~/.cache/rx/indexes/`
that maps line numbers to byte offsets via sparse checkpoints. Once
built, an index lets `rx` answer "where does line N start?" by reading
at most one checkpoint step (1 MB by default) from the nearest
checkpoint, whatever the file size.

## The file format

An index is stored as a JSON document with the full `UnifiedFileIndex`
schema, format version 8. The critical field is `line_index`. The
start of the index `rx samples` built for a 465 MB log (most of the
other members trimmed; `rx index --info --json` prints all of them):

```json
{
  "version": 8,
  "source_path": "/var/log/middleware.log-2025121008",
  "source_modified_at": "2025-12-27T17:30:57.775888",
  "source_size_bytes": 487561499,
  "created_at": "2026-10-03T04:04:39.896939Z",
  "build_time_seconds": 0.123523083,
  "source_inode": 121515888,
  "source_changed_at": "2026-04-18T20:43:30.904417",
  "source_fingerprint": "8bace9b40fccf36b77b65e88908e85458cf74ea44324c4aaff8b374adb8ddfe8",
  "source_mtime_ns": 1766853057775888000,
  "source_ctime_ns": 1776534210904417000,
  "source_device": 16777232,
  "file_type": "text",
  "index_step_bytes": 1048576,
  "analysis_performed": false,
  "line_count": 1436842,
  "line_index": [[1, 0], [4713, 1048616], [7382, 2097336], [10118, 3157429]],
  "time_index": {
    "format": "iso",
    "anchored": true,
    "day_first": null,
    "has_zone": false,
    "year_from_mtime": false,
    "timestamped_lines": 1387928,
    "first": {"ms": 1765350004574, "line": 1, "offset": 0},
    "last": {"ms": 1765353604390, "line": 1436842, "offset": 487561376},
    "first_zone_offset_minutes": null,
    "backward_steps": 0,
    "max_backward_ms": 0,
    "max_before": [null, 1765350019707, 1765350027397, 1765350030944]
  }
}
```

The real `line_index` has 429 entries. Each checkpoint is a
`[line_number, byte_offset]` pair; a seekable zstd index adds the frame
as a third element, `[line_number, byte_offset, frame_index]`. Between
checkpoints, no data is recorded — to find line 6000 you seek to the
checkpoint for line 4713 and scan forward.

Every checkpoint names a line the file has: the first is `[1, 0]`, and
each one is at the byte where its line starts (in a seekable zstd
index, a frame's checkpoint is at the frame's first byte, which can be
inside its line). An empty file has no lines, so its `line_index` is
empty.

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
above has 429 checkpoints in a 15.8 KB file, its time section
included, and `rx index --info` on it
takes 11 ms, process start included.

## The time section

Every build also reads the timestamp at the start of each line and
records what it found as `time_index`. It costs no extra pass: the same
walk that places the checkpoints reads each line's timestamp, for a
plain, a compressed and a seekable zstd file alike, with or without
`--analyze`. `time_index` is `null` when no timestamp format is
recognized in the first mebibyte of the file's text; the key is always
there.

The format is decided once per file, from the first mebibyte of its
text (decompressed for a compressed file): ISO 8601 and its everyday
relatives (`2025-12-10 07:00:04.574`, `2025-2-15 18:16:22:397`,
`2025-12-10 16:18:53,741`, `2026-10-06T12:34:56+02:00`), the Apache
access-log form, the C `ctime` form, syslog (`Dec 10 07:49:50.123`),
dates with slashes or dots, and epoch seconds or milliseconds. Each
line's own timestamp is read with that one format; a line without one
(a traceback, a continuation line) has none.

Log lines often carry text that someone outside writes (a request path,
a user name, a message with a newline in it), so the choice is made in a
way a few such lines cannot steer:

- A timestamp at the start of a line (`anchored`) is strong evidence:
  three such lines make a log, whatever their share. They outrank a
  timestamp further into the line, as in JSON lines, only when they are
  at least 1% of the non-blank lines of the first mebibyte; below that
  they decide the format only when no reading further in matches half
  of the lines.
- A slash date such as `10/06/2026` is read month first unless at least
  three lines read only day first (`13/01/2026`) and they outnumber the
  lines that read only month first (`01/13/2026`).
- In a file whose timestamp is further into the line, the first
  timestamp in the line's first 120 bytes counts, so text written before
  the log's own time field can set it.

Every value is milliseconds since the Unix epoch, in the file's frame.
In a file whose timestamps carry a zone (`Z`, `+02:00`, `UTC`, `GMT`),
a value is the UTC instant, and a line without a zone is read as UTC.
In a file whose timestamps carry none, a value is the wall-clock time
the line shows, as if it were UTC, and a line that does carry a zone
keeps the wall clock it shows too: the 465 MB log above is written on a
host at UTC-7, and the JVM's `[2025-12-10T07:00:07.953-0700]` lines
between its own zone-less lines read as 07:00:07.953, beside them, not
seven hours later. Zone words that name no single offset (`MST`,
`CEST`) are not converted. The index never applies a time zone, so its
values do not depend on the environment of the process that built it.

| Field | Meaning |
|---|---|
| `format`, `anchored`, `day_first`, `has_zone` | The detected format: its family, whether the timestamp starts each line (or sits after a `[` at its start) rather than further in, the day/month order of a slash date (`null` for any other family), and whether most timestamps carry a zone |
| `year_from_mtime` | `true` for a format without a year (syslog): each timestamp takes the year of the file's modification time, `source_mtime_ns`, or the latest earlier year that puts it no more than a day after the mtime (up to 8 years back, for a February 29). A December line in a file last written in January is read in the previous year, and a rebuild of the same file reads the same years |
| `timestamped_lines` | How many lines carry a timestamp |
| `first`, `last` | The first and the last timestamped line: `{ms, line, offset}`, its value, its line number and the byte where it starts; `null` when no line has one |
| `first_zone_offset_minutes` | The zone offset, in minutes east of UTC, of the first timestamp of a file whose timestamps carry zones; `null` otherwise |
| `backward_steps`, `max_backward_ms` | How many lines carry a timestamp more than one second earlier than the latest timestamp before them, and the largest such step. Several programs writing one file disagree by a second or so; a large count says the file is mixed |
| `max_before` | One entry per `line_index` checkpoint: the latest timestamp of every line numbered below the checkpoint's line, or `null` when none of them has one. It never decreases, so a lookup by time can skip every checkpoint whose earlier lines all come before the time it looks for |

A seekable zstd index has checkpoints of its own (each frame, and every
10,000 lines inside a frame), and its `max_before` follows them; every
other member of the time section is the same for the plain, gzip and
seekable copies of one file.

`rx index --info` prints the format, the first and last timestamps, the
count and the backward steps; `--json` and `--info --json` give the
whole section (see [`rx index`](../cli/line-index.md)).

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
3. Counts lines along the way, and reads each line's timestamp for the
   [time section](#the-time-section)
4. Optionally (when `--analyze` is set) gathers line-length statistics
5. Writes the JSON file atomically (temp file + rename) to
   `~/.cache/rx/indexes/<safe-name>_<hash16>.json`, with the name cut
   to fit in 255 bytes (see [caching](caching.md#indexes))

### Cost

- **Cold build**: one sequential pass; 120 ms for the 465 MB log in the
  page cache, slower when the file must come from disk
- **Time section**: reading each line's timestamp costs about 35 ns a
  line. The walk reads each line where its read buffer holds it rather
  than copying it, which saves about as much on lines of a few hundred
  bytes: the 465 MB log (1.4 million lines) built in 133 ms before the
  time section was added and in 120 ms with it; the 6.3 GB log (42.5
  million lines of about 150 bytes) in 2.3 s and 2.7 s
- **Warm read**: 11 ms for `rx index --info` on that log, process start
  included
- **Memory**: the builder keeps only the sparse checkpoints, not one
  entry per line; peak RSS was 16 MB for the 465 MB log and 17 MB for a
  6.3 GB log (42.5 million lines, built in 2.7 s)

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
  why: `file size … bytes is below threshold 52428800 bytes`)
- You'll only read the file once
- You only need byte offsets (they don't need an index at all)

## Cache invalidation

An index is valid as long as its format version is the current one
(7) and the source file hasn't changed. `rx` checks:

1. `version` equals the current format version; an index of another
   version is treated as absent
2. The size matches `source_size_bytes`
3. The mtime matches `source_mtime_ns` (nanoseconds since the Unix
   epoch, so the time zone does not matter)
4. The inode, its device and the ctime match `source_inode`,
   `source_device` and `source_ctime_ns`
5. A digest of the size plus the first and last 64 KiB matches
   `source_fingerprint`

Any mismatch → the index is treated as absent and rebuilt by the next
call that wants one. So is an index file that cannot be read or parsed
(a truncated write, a permission error), which is also logged at Warn
level as `index_unreadable`; a `samples` lookup rebuilds such a file
even for a file below the large-file size. When the cache cannot store
an index at all, a lookup builds none and logs `index_not_stored` once
(see [caching](caching.md#when-the-cache-cannot-be-written)).

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

For **seekable zstd** (produced by `rx compress`, or by another
seekable encoder), each checkpoint also names its frame, so a lookup
decompresses only the frames that hold the wanted lines. The index's
`frames` table gives each frame's `first_line`, the line that holds the
frame's first byte, and the lines it ends. `rx compress` ends every
frame at a line break; another encoder may cut a frame mid-line, and a
line longer than a frame spans frames that end no line. Such a frame
has `line_count` 0 and `last_line` one less than `first_line`, and the
next frame starts on the same line, so every line number stays the one
the plain text gives. See [compression](compression.md).

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
