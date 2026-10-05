# `GET /v1/time-range`

The timestamp format of one file and its first and last timestamp: the
span a timeline across several open files is drawn from. Since contract
1.5; `features` on [`GET /health`](health.md) lists `time_range`.

## Purpose

- Place each open file on one time axis (UTC instants)
- Show the file's times the way its lines write them (`display_zone`,
  `example`)
- Learn whether the file has timestamps at all before offering a jump
  by time ([`GET /v1/samples?timestamps=`](samples.md#by-time))

## Request

```text
GET /v1/time-range?path=/var/log/app.log
```

### Query parameters

| Parameter | Required | Description |
|---|---|---|
| `path` | yes | The file. Checked against the search roots like every path |
| `file_tz` | no | Read the file's timestamps as the wall clock each line writes, in this zone (`UTC`, an IANA name or `±HH:MM`), ignoring any zone a line writes; see [A file zone](#a-file-zone). Another value answers `400` |

### What a request reads

| File | Reads | `source` |
|---|---|---|
| Any file whose [line index](../../concepts/line-indexes.md#the-time-section) still describes it | The index only; nothing of the file | `index` |
| Plain or seekable zstd, no index | The first mebibyte of the text (format, first timestamp, `example`) and a read back from the end, a mebibyte at a time, at most 16 MiB of text (last timestamp); for a seekable file, at most 32 MiB of frames decoded | `scan` |
| gzip, bzip2, xz or plain zstd, no index | The first mebibyte of the text (format, `example`) | `none` |

A request never builds an index, never writes to the cache and never
decompresses a whole file. A seekable zstd file decodes a frame whole
to read any part of it, and a frame may hold up to 128 MiB of text, so
the read back from the end decodes the frames it needs only while their
text adds up to 32 MiB at most: a file whose last frames are larger
(`rx compress --frame-size=64M`) answers `last_ms: null` without an
index. The frames `rx compress` writes by default (4 MiB) and frames of
16 MiB fit. A stream-compressed file gets its index from
the background build a `GET /v1/samples` of it starts (or from
`POST /v1/index`); ask again when that build ends. `RX_NO_INDEX=true`
on the server makes every request read without an index.

### A file zone

`file_tz=ZONE` reads each line's timestamp as the wall clock it
writes, in ZONE ([A file zone](../../concepts/timestamps.md#a-file-zone)):
`display_zone` is ZONE, `first_ms` and `last_ms` are those wall clocks
read in ZONE, and `has_zone` and `example` stay the file's. The reads
are those of the table above: an indexed file whose timestamps carry
zones holds instants and the offset each line writes (`zone_offsets`),
so each stored timestamp plus its line's offset gives the wall clock,
with no read. Only when that file's offset changes too often for
1,024 entries does the index record no offsets: the first timestamp is then the
stored one plus the offset the index stores for it, and the last
timestamped line is read again at the byte offset the index stores (a
window of one line, `source` stays `index`); a gzip, bzip2, xz or plain
zstd file, which cannot be read at an offset, then answers
`source: none`.

```bash
curl -sG 'http://127.0.0.1:7777/v1/time-range' \
    --data-urlencode 'path=/var/log/app.log-2025121008' --data-urlencode 'file_tz=Europe/Berlin'
```

```json
{
  "path": "/var/log/app.log-2025121008",
  "format": "iso",
  "has_zone": false,
  "day_first": null,
  "display_zone": "Europe/Berlin",
  "example": "2025-12-10 07:00:04.574",
  "first_ms": 1765346404574,
  "last_ms": 1765350004390,
  "source": "scan",
  "cli_command": "rx time-range /var/log/app.log-2025121008 --file-tz=Europe/Berlin"
}
```

## Response — 200 OK

```json
{
  "path": "/var/log/app.log-2025121008",
  "format": "iso",
  "has_zone": false,
  "day_first": null,
  "display_zone": "UTC",
  "example": "2025-12-10 07:00:04.574",
  "first_ms": 1765350004574,
  "last_ms": 1765353604390,
  "source": "scan",
  "cli_command": "rx time-range /var/log/app.log-2025121008"
}
```

A file without a recognized format:

```json
{
  "path": "/var/log/notes.txt",
  "format": null,
  "has_zone": null,
  "day_first": null,
  "display_zone": null,
  "example": null,
  "first_ms": null,
  "last_ms": null,
  "source": "scan",
  "cli_command": "rx time-range /var/log/notes.txt"
}
```

### Response fields

| Field | Type | Description |
|---|---|---|
| `path` | string | The file, as validated |
| `format` | string \| null | `iso`, `clf`, `ctime`, `syslog`, `slash`, `dotted` or `epoch` ([formats](../../concepts/timestamps.md#formats)); `null` when none is recognized in the first mebibyte, and then every field below but `source` and `cli_command` is `null` |
| `has_zone` | bool \| null | Whether most timestamps carry a zone |
| `day_first` | bool \| null | For `slash`, whether the day comes before the month; `null` for every other format |
| `display_zone` | string \| null | The zone to show this file's times in so they read as its lines do: `RX_LOG_TZ` as set (`UTC` by default, an IANA name or `±HH:MM`) for a file whose timestamps carry no zone, the offset of its first timestamp (`±HH:MM`) for one whose timestamps do |
| `example` | string \| null | The first timestamp as its line writes it (`2025-12-10 07:00:04.574`, `Dec 10 07:00:12.156`, `2025-12-10 16:18:53,741`): printable ASCII, any other byte as `\xHH`, at most 64 bytes |
| `first_ms` | int \| null | The timestamp of the first line that has one, as a UTC instant in ms. A file whose timestamps carry no zone has its wall clock read in `RX_LOG_TZ`, as `line_timestamps` reads it. `null` with `source: none` |
| `last_ms` | int \| null | The same for the last line that has one. `null` with `source: none`, and from a scan when no line within the last 16 MiB of the text has a timestamp or, for a seekable file, when the frames that hold it decode to more than 32 MiB |
| `source` | string | `index`, `scan` or `none` (above) |
| `cli_command` | string | The command that gives the same answer |

`GET /v1/samples?timestamps=<first_ms as RFC 3339 with Z>` answers the
first timestamped line, and the same for `last_ms` and the last one
when the file is in order. With an index and without one the answer is
the same, except that `first_ms` and `last_ms` may be `null` without
one (`source: none`, a last timestamp more than 16 MiB from the
end, or a seekable file of frames too large to decode for it).

## Status codes

| Code | Meaning |
|---:|---|
| `200 OK` | The answer, with timestamps or without |
| `400 Bad Request` | The path is a directory, not a text file, or needs more than 128 MiB at once to decompress; or `file_tz` names no zone (`Invalid file_tz "Mars/Base": invalid time zone: not a zone name; give UTC, an IANA zone name or ±HH:MM`) |
| `403 Forbidden` | Outside the search roots, a hidden entry, or a file the server may not read |
| `404 Not Found` | The file does not exist |
| `422 Unprocessable Entity` | `path` missing |
| `500 Internal Server Error` | The file could not be read (a damaged stream within its head) |

## Example

```bash
curl -s 'http://127.0.0.1:7777/v1/time-range?path=/var/log/app.log' | jq '{format, first_ms, last_ms, source}'
```

## See also

- [`rx time-range`](../../cli/time-range.md) — the same from a terminal
- [Timestamps](../../concepts/timestamps.md) — formats, zones, the line at a time
- [`GET /v1/samples`](samples.md#by-time) — the lines at a time
