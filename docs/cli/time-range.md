# `rx time-range`

The timestamp format of each file and the first and last timestamp in
it: the time range a timeline needs, and a quick look at what period a
log covers.

## Synopsis

```text
rx time-range PATH... [--json]
```

## Description

For each file, `rx time-range` gives the [timestamp format](../concepts/timestamps.md)
of its lines, the first and the last line's own timestamp, the zone its
lines show times in, and where the range came from:

| Source | How the range was found |
|---|---|
| `index` | The file's [line index](../concepts/line-indexes.md#the-time-section) stores it. Nothing of the file is read |
| `scan` | A plain or seekable zstd file without an index: the first mebibyte of the text gives the format and the first timestamp, and a read back from the end, a mebibyte at a time and at most 16 MiB, the last |
| `none` | A gzip, bzip2, xz or plain zstd file without an index (under `RX_NO_INDEX`, or when its index could not be built or stored): its last timestamp needs the whole text decompressed, so the range is not given |

A gzip, bzip2, xz or plain zstd file without an index gets one built
first, as [`rx samples`](samples.md#the-index-it-may-build) builds one,
so its range comes from the index; `RX_NO_INDEX=true` turns that off. A
plain or seekable zstd file is never indexed by this command: its head
and tail are enough, however large it is.

The first and the last timestamp are those of the first and the last
line, in file order, that carry a timestamp of their own. A file whose
last timestamped line starts more than 16 MiB before its end (a log
that ends in a long dump) has no last timestamp from a scan; `rx index`
gives it one.

Times are UTC instants in `--json`. A file whose timestamps carry no
zone has its wall clock read in `RX_LOG_TZ` (default `UTC`), as
`line_timestamps` reads it, so `rx samples --timestamps=<first_ms>`
finds its first timestamped line.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | One JSON object for one path, an array of objects (in the order of the paths) for several |

The [global flags](index.md#global-flags) apply; `--search-root`
refuses a path outside the roots.

## Output

One line per file: the path, the format, the first and the last
timestamp written the way the file writes them and in its zone, the
zone, and the source. A timestamp the answer does not know is `?`.

```bash
rx time-range app.log-2025121008 worker.log-2025121008 db.log-2025121008.gz notes.txt
```

```text
app.log-2025121008  iso  2025-12-10 07:00:04.574 .. 2025-12-10 08:00:04.390  UTC  scan
worker.log-2025121008  syslog  Dec 10 07:00:12.156 .. Dec 10 08:00:13.175  UTC  scan
db.log-2025121008.gz  iso  2025-12-10 07:00:30 .. 2025-12-10 08:00:11  UTC  index
notes.txt  no timestamps  scan
```

The layout follows the file's first timestamp (`example`) in the
separators, the fraction and its digits, a 12-hour clock and the form
of a zone; numbers are always zero-padded, a two-digit year is written
with four digits and milliseconds after a `:` with three.

### `--json`

```bash
rx time-range app.log-2025121008 --json
```

```json
{
  "path": "app.log-2025121008",
  "format": "iso",
  "has_zone": false,
  "day_first": null,
  "display_zone": "UTC",
  "example": "2025-12-10 07:00:04.574",
  "first_ms": 1765350004574,
  "last_ms": 1765353604390,
  "source": "scan",
  "cli_command": "rx time-range app.log-2025121008"
}
```

The members are those of [`GET /v1/time-range`](../api/endpoints/time-range.md#response-fields).

## Exit codes

| Code | When |
|---:|---|
| 0 | Every file was answered, with timestamps or without |
| 1 | A file could not be read, or several files failed with different codes |
| 2 | A path is a directory or not a text file, or no path is given |
| 3 | A file does not exist |
| 4 | A path is outside `--search-root`, or the process may not read it |

With several paths, each file that can be answered is, and each one
that cannot is reported on stderr; the exit code is then the code the
failures share, or 1 when they differ.

## See also

- [`GET /v1/time-range`](../api/endpoints/time-range.md) — the same
  answer over HTTP
- [Timestamps](../concepts/timestamps.md) — formats, zones and the
  line at a time
- [`rx samples --timestamps`](samples.md#by-time) — the lines at a time
