# Timestamps

Most log lines start with a timestamp. rx recognizes the format a file
writes its timestamps in, reads the timestamp of each line, and finds
lines by time: `rx samples --timestamps=…` and
`GET /v1/samples?timestamps=…`.

## Formats

A file has one format, chosen from these families:

| Family | Examples |
|---|---|
| `iso` | `2026-10-06T12:34:56.123+02:00`, `2026-10-06 12:34:56,123`, `2025-2-15 12:34:5:123`, `2026/10/06 12:34:56`, `2025-12-10 07:49:50 UTC` |
| `clf` | `[06/Oct/2026:12:34:56 +0000]` (Apache and nginx access logs) |
| `ctime` | `Tue Oct 06 12:34:56.123456 2026` |
| `syslog` | `Oct  6 12:34:56`, `Dec 10 12:34:56.123` (no year: it comes from the file's modification time) |
| `slash` | `10/06/2026 12:34:56 PM`, `06/10/2026 14:34:56` (day or month first, decided once for the file) |
| `dotted` | `06.10.2026 12:34:56` (day first) |
| `epoch` | `1696600000`, `1696600000.123`, `1696600000123` (at the start of the line or after `[`) |

Milliseconds after a `:` (`12:34:5:123`) are a whole number of 1 to 3
digits. Digits past the millisecond are dropped. A zone is `Z`, a
numeric offset, `UTC` or `GMT`; other zone words (`EST`, `CEST`) are
ambiguous and are read as no zone. A time with no date (`12:34:56 INFO`)
and a kernel uptime (`[ 1234.567]`) are not timestamps.

## Detection

The format is decided from the first mebibyte of the file's text (the
decompressed text of a compressed file), at most 5,000 lines. A
timestamp at the start of a line (or right after a `[` there) is strong
evidence: three such lines are enough when they are at least 1% of the
lines. A timestamp further into the line, within its first 120 bytes,
must be on at least half of the lines. With neither, the file has no
timestamps, and a time query on it is refused (exit 2, HTTP 400:
"no timestamp format recognized in the first 1 MiB").

Detection reads the same bytes whether or not the file has a line
index, so the two always agree. A line index stores the format; without
one, every `samples` answer reads the head to report it as
`time_format`, at most a mebibyte.

## The line at a time

**The line at T is the first line, in file order, whose own timestamp
is T or later.** A line's own timestamp is the one written on it; a
line without one (a traceback, a continuation line) is never the line
at T, but it is inside a range that spans it.

- In an ordered file this is the obvious line.
- When several writers share a file and disagree by a second or two, it
  is the first line that reached T. There is nothing to tune.
- When no line reaches T, there is no line at T: `-1`, and a `null`
  sample, as for a line number past the end of the file.

A single time returns its line with the context lines `--context`,
`--before` and `--after` ask for, exactly as `--lines=N` returns line N.

A **range** `T1..T2` runs from the line at T1 to the line before the
first line whose own timestamp is later than T2. Both ends are inclusive
to the millisecond: `..14:35:15` means up to `14:35:15.000`, so a line
at `14:35:15.001` is after it. Either end may be left open: `..T2`
starts at line 1, `T1..` runs to the last line. A range returns its
lines without context, as a `--lines` range does. It is empty (`-1`,
`null`) when no line is at T1, or when its end comes before its start,
which a file whose writers disagree can give.

## Queries

Each end of a query may be:

1. A date and time in ISO 8601 or RFC 3339, with or without seconds,
   fraction and zone: `2026-10-06T12:34`, `2026-10-06 12:34:56.5Z`.
2. A date alone: `2026-10-06` is 00:00:00.000 that day.
3. A time alone: `14:33:12`, `14:33`, `2:33:12 PM`. The date comes from
   the file: when its first and last timestamps fall on the same date,
   that date; otherwise the query is refused, naming both dates.
4. Epoch seconds or milliseconds: 10 or 13 digits, an optional
   fraction.
5. A timestamp as the file writes it, copied from a line:
   `Dec 10 07:49:50.123`, `[06/Oct/2026:12:34:56 +0000]`,
   `2025-12-10 12:34:56,123`. Spaces around it and one pair of `[ ]`
   are ignored.

A value is never split on commas, since `,` is part of Python's
timestamp format: give several queries by repeating the flag
(`-t A -t B`) or the query parameter (`?timestamps=A&timestamps=B`).
One request takes at most 1,000 queries.

## Zones

A timestamp that carries a zone is the instant it names. One that does
not is kept as the wall-clock time written on the line, the **file's
frame**; a line index never applies a zone, so it does not depend on
the environment of the process that built it. In a file whose
timestamps carry no zone, a line that does carry one keeps its wall
clock too, so the whole file is in one frame.

Two settings say how a query meets the file
([configuration](../configuration.md#timestamps)):

- `RX_LOG_TZ` (default `UTC`) is the zone a file's zone-less timestamps
  were written in. A query with a zone, or an epoch time, is turned
  into the file's wall-clock time with it.
- `RX_QUERY_TZ` is the zone a query without a zone is read in. Unset
  (the default), it is read in the file's own frame: as the file's wall
  clock, or, in a file whose timestamps carry zones, at the offset of
  its first timestamp. So a time copied from a line finds that line.
  `local` reads it in the zone of the rx process.

`time_format.assumed_zone` in a samples answer says which zone a
timestamp without one is read in: `RX_LOG_TZ` for a file whose
timestamps carry no zone, `UTC` for one whose timestamps do.

## How a line is found

Without a line index, rx reads the file from its first line, reading
each line's timestamp, until it reaches the line at T: the line's
number has to be counted from the start anyway. With an index, each
checkpoint stores the latest timestamp of every line before it
(`max_before`); a binary search finds the last checkpoint whose
`max_before` is below T, and rx reads from there, at most one index
step (1 MB by default). Both give the same line. The line is then read,
with its context, by the same code that answers `--lines`, so a time
query and `--lines=N` for the line it found give the same sample.

A time with no date needs the file's first and last timestamps. An
index stores them. Without one, rx finds the first from the start of
the text, and the last by reading back from the end of the text a
mebibyte at a time; a gzip, bzip2, xz or plain zstd file cannot be
entered at its end, so it is decompressed whole for that.

A gzip, bzip2, xz or plain zstd file is always decompressed from its
first byte: an index there saves reading the timestamps of the lines
before its checkpoint, not the decompression. A seekable zstd file with
an index starts at the frame that holds the checkpoint.
