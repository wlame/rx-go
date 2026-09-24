# `GET /v1/logs/chains`

The log chains of one directory: the files of each rotated log, found
from their names alone.

## Purpose

Rotation tools split one log into many files: `syslog`, `syslog.1`,
`syslog.2.gz`, or `syslog-20261001-1790812801.gz`. A **log chain** is
those files, read as one text. This route finds the chains of a
directory so a client can show one entry per chain in place of its
files. [`rx logs list`](../../cli/logs.md) gives the same answer from a
terminal.

## Request

```text
GET /v1/logs/chains?path=<dir>
```

### Query parameters

| Parameter | Type | Required | Description |
|---|---|:-:|---|
| `path` | string | yes | The directory whose chains to list |

## Which files form a chain

A file is a part of a chain when its name has one of these shapes; the
chain's name is the name its active file has (or would have):

| Shape | Examples | Chain name |
|---|---|---|
| `{name}.{N}`, N of 1 to 5 digits | `syslog.1`, `dpkg.log.11.gz`, `dmesg.0` | `{name}` |
| `{name}{sep}{DATE}`, optionally `{sep}{digits}` | `syslog-20261001-1790812801.gz`, `app.log.2026-10-01_12`, `0.log.20261001-120000`, `access_log.1790726400` | `{name}` |
| `{stem}{sep}{DATE}`, optionally `{sep}{N}`, then `.{ext}` | `app-2026-10-01.3.log.gz`, `app-2026-10-01T12-00-00.000.log.gz`, `postgresql-2026-10-01_000000.log` | `{stem}.{ext}` |
| `{stem}{sep}{N}.{ext}` | `app.1.log.gz`, `app-2.log` | `{stem}.{ext}`, only when that file exists |

`sep` is `.`, `-` or `_`; `ext` is a letter and up to seven letters or
digits. `DATE` is `yyyymmdd`, `yyyymmddhh` (or a 10-digit epoch),
`yyyymmdd` followed by `-`, `T` or `_` and `HHMMSS`, or `yyyy-mm-dd`
optionally followed by `_` or `T` and `HH`, `HH-MM`, `HHMM`,
`HH-MM-SS`, `HHMMSS` or `HH-MM-SS.mmm`. A compression suffix (`.gz`,
`.bz2`, `.xz`, `.zst`) may end every shape. A name is matched as the
text it is: spaces, brackets and other characters in it are only
characters.

A number of four digits from 1970 to 2100 where `N` goes (`report.2023`,
`app.2024.log`) is a year, so such a file is a yearly part ordered by its
year, not the 2023rd rotation.

The file named exactly like the chain is its **active** part, the one a
program still writes. A chain needs two parts; the active file counts
when it exists.

Left out of every chain: directories, hidden entries (unless the server
runs with `--hidden`), names ending in `.tmp`, and files that are not
text by the rule every route applies (a NUL byte in the first 8 KiB of
the text, so `wtmp` and `wtmp.1` are no chain). An empty file is text,
so a chain of empty files is a chain. A file whose name makes it a part
but which cannot be opened (its permissions, an I/O error) stays a part
and is named in `unreadable`: the chain cannot be read as one text until
it can, and [`GET /v1/logs/chain`](logs-chain.md) says so.

One generation in several encodings (`app.log-2025121008`, its `.gz` and
its `.zst`) is one part. The part is the encoding found first in this
order, by the bytes of each file, not by its name: plain, seekable zstd,
zstd, gzip, bzip2, xz. The others are its duplicates and are not
listed in `parts`.

## Response — 200 OK

```json
{
  "path": "/var/log",
  "chains": [
    {
      "path": "/var/log/dpkg.log",
      "name": "dpkg.log",
      "parts": ["dpkg.log.4.gz", "dpkg.log.2.gz", "dpkg.log.1", "dpkg.log"],
      "has_active": true,
      "missing": ["dpkg.log.3"],
      "missing_count": 1,
      "size": 20147,
      "compression_formats": ["gzip"],
      "is_indexed": false,
      "unreadable": [],
      "too_many_parts": false
    }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `path` | string | The listed directory, absolute |
| `chains` | array | One entry per chain, sorted by name, case-insensitive; never `null` |

### `chains[]` fields

| Field | Type | Description |
|---|---|---|
| `path` | string | The chain's **handle**: the directory joined with the chain's name, which is the active file's path whether that file exists or not |
| `name` | string | The chain's name |
| `parts` | string[] | The parts' file names in the provisional order, oldest first: by the number or date in each name, in its scheme's direction (a higher number is older, a lower date is older), then by modification time; the active file last. When one chain mixes numbered and dated names, the kind whose newest file is older comes first. The real order of a chain comes from its timestamps |
| `has_active` | bool | Whether the active file exists |
| `missing` | string[] | The name each absent number would have, from 0 when a `.0` part exists (else from 1) up to the highest number present, lowest first, without a compression suffix; at most 100 names, and `missing_count` says how many are missing in all. They are named only when no more numbers are missing than parts with a number are present: `x.1` beside `x.500` names none. Dated parts, years among them, are never missing |
| `missing_count` | int | How many numbered parts are missing, whether `missing` names them all or stops at 100; 0 when `missing` names none |
| `size` | int64 | The sum of the parts' file sizes in bytes, as stored |
| `compression_formats` | string[] | The distinct compression formats of the parts, sorted: `gzip`, `bz2`, `xz`, `zstd` (a seekable zstd file is `zstd`). A plain part adds none |
| `is_indexed` | bool | Whether every part but the active file has a current line index, built from the file the listing found: an index of another file that took a part's name since (a rotation renamed it there) does not count |
| `unreadable` | string[] | The parts that cannot be opened (their permissions, an I/O error), in the order of `parts`; empty when every part can. A part is named here, not dropped, so the chain does not look complete or name it as missing |
| `too_many_parts` | bool | Whether the chain has more than 10,000 parts |

A chain may have at most 10,000 parts. A larger one is still listed,
with `too_many_parts` true, and is read no further: `parts` and
`missing` are empty, `missing_count` is 0, `size` is 0 and `is_indexed`
is false. The routes that read a chain as one text refuse it.

## Status codes

The same as [`GET /v1/tree`](tree.md) for the same path:

| Code | When |
|---:|---|
| `200 OK` | Listing returned, possibly with no chains |
| `400 Bad Request` | The path is a file, not a directory |
| `403 Forbidden` | The path is outside `--search-root`, reaches a hidden entry, or cannot be read |
| `404 Not Found` | The directory does not exist |
| `422 Unprocessable Entity` | `path` is missing |

## What it reads

One listing of the directory, as `GET /v1/tree` makes it. For each name
that can belong to a chain (a group of two names or more), the text
check `GET /v1/tree` makes for `is_text`: at most the file's signature,
its seek table and 8 KiB of its text. An empty file is not opened. A
chain is checked only until it is known to have more than 10,000 parts,
so one listing makes at most 10,001 checks per chain. For each chain of
at most 10,000 parts, the stored line index of each frozen part,
stopping at the first without one. Nothing is written, and no index is
built.

## Examples

```bash
curl -sG 'http://127.0.0.1:7777/v1/logs/chains' \
    --data-urlencode 'path=/var/log' \
    | jq '.chains[] | {name, parts: (.parts | length), missing}'
```

A directory or a chain name with spaces or brackets goes into a query
URL-encoded, as `--data-urlencode` does; the answer gives every name as
it is.

## See also

- [`rx logs`](../../cli/logs.md) — the same from a terminal
- [`GET /v1/tree`](tree.md) — the directory's files
