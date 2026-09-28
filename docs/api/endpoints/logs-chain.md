# `GET /v1/logs/chain`

The description of one log chain: its parts in time order, with their
line counts and times, the checks that make the chain one text, its
state and a fingerprint of its files.

## Purpose

A log chain is the files of one rotated log, found by
[`GET /v1/logs/chains`](logs-chains.md). Before a client reads it as
one text it needs to know the order of the parts, where each one starts
in the chain's numbering, and whether the parts can be read as one text
at all. This route says all of that, and how far the chain is from
being ready. [`rx logs show`](../../cli/logs.md#rx-logs-show) gives the
same answer from a terminal.

## Request

```text
GET /v1/logs/chain?path=<handle>[&file_tz=<zone>][&fingerprint=<fp>]
```

### Query parameters

| Parameter | Type | Required | Description |
|---|---|:-:|---|
| `path` | string | yes | The chain's handle: its directory joined with its name (`/var/log/syslog`), as `GET /v1/logs/chains` gives it in `path` |
| `file_tz` | string | no | Read every part's timestamps as the wall clock each line writes, in this zone (`UTC`, an IANA name or `±HH:MM`), as [`GET /v1/time-range`](time-range.md) reads one file; another value answers `400` |
| `fingerprint` | string | no | The fingerprint of a description the client holds (16 hex digits). When the chain's files changed since, the answer is `409` with the current description |

## What it reads

Each frozen part's stored line index, and the head and the tail of the
active file (at most the first mebibyte and 16 MiB back from its end),
or the active file's index when it is current. It never reads a whole
part and never builds an index inside the request: a frozen part without
a current index is only opened, to learn that it can be read, and the
chain is `pending` until its index is built. An empty part is not
opened.

## The index task of a pending chain

A request that finds the chain `pending` starts the chain's **index
task** in the background, or joins the one already running for it, and
names it in `index_build`; it does not wait for it. The task (operation
`chain_index`, shown at [`GET /v1/tasks/{id}`](tasks.md) with the
handle as its `path`) builds and stores the line index of each part the
chain waits for: every frozen part with lines and without a current
index, and a gzip, bzip2, xz or plain zstd active file, whose first
timestamp only its index gives. Its progress is the share of those
parts done. Once it completes, the next request finds the chain `ready`
(or `invalid`, when a check fails on what the indexes say).
[`POST /v1/logs/index`](logs-index.md) builds every part's index, the
active file's too, and has the details: one task per chain, the builds
it runs at once, and why a part fails it.

At most 128 chains' index tasks run or wait at once. A pending chain
past that gets no task: its description is `pending` with `index_build`
null, and the next request after one of those tasks has ended starts
its task.

A pending chain whose last index task failed for the same files (the
same fingerprint) does not start it again: its description names the
failed task, and the `message` of `index_build` says which part failed
and why (as the task's `error` does). The server keeps how the chain's
last task ended for `RX_TASK_TTL_MINUTES` after its end, as long as the
chain's files keep that fingerprint, even when the task table has
dropped the task to make room for others; `GET /v1/tasks/{id}` may then
answer `404` while the description still names it. `POST
/v1/logs/index` starts a new one.

The frozen parts' data of a chain whose frozen parts are all indexed
(or empty) is kept in memory: at most 64 chains and 40,000 parts in
all, the least recently used dropped. It is keyed by the handle, a
SHA-256 of every part's name and stat (a frozen part's size,
modification time and ctime included) and the zones, so a change to any
frozen part is a new key. The next request reads no index of a frozen
part: it stats each one's index file, and drops the kept data when an
index was removed, rebuilt or no longer describes its part. A request
whose listing could not open a frozen part (an I/O error, too many open
files, or a name that led to another file by then) does not use the
kept data, and keeps it: that request answers as if nothing were kept
(`unreadable`, or 409), and the next one that opens every part uses it
again. The active file is read on every request.

## The checks and the states

A chain is valid when:

1. every part with lines has timestamps rx recognizes (`no_timestamps`);
2. sorted by their first timestamp, no part's highest timestamp is after
   the next part's first by more than `RX_CHAIN_OVERLAP_SECONDS`
   (default 60; `overlap`, with the overlap in `overlap_ms`);
3. the active file, when it has lines, comes last (`active_not_last`);
4. every part can be opened and read (`unreadable`, whose `message`
   gives the failure in the fixed words a search gives it in
   `skip_reasons`: `permission denied`, `cannot be read`, …; a part the
   listing could not open is named without another read);
5. it has at most 10,000 parts (`too_many_parts`; no part is read, and
   `parts` is empty: the reason gives how many parts the files' names
   give).

Empty parts add no lines and take no part in the checks.

| State | When |
|---|---|
| `pending` | Some frozen part has no current line index, or the active file's first timestamp is not known yet (a compressed active file without an index). Each part can be read on its own; the global line numbers and the chain's times wait |
| `ready` | Every part is known and every check passed: the chain reads as one text |
| `invalid` | A check failed; `reasons` says which |

## Order and numbering

The parts are sorted by their first timestamp, ties keeping the
provisional order of their names; an empty part keeps its place. Before
the chain is ready (and for a part not read yet) the order is the
provisional one, by the number or date in the names.

A part's `global_start` is the global line number of its first line: 1
plus the lines of the parts before it. Line `L` of a part is global line
`global_start + L - 1`; each part's last line is its own line, even
without a final newline. The active file may grow: its new lines take
the numbers after it and change no earlier number.

## Times

Every time is a UTC instant in milliseconds, read the way
`GET /v1/time-range` reads one file: a part whose timestamps carry no
zone is read in `RX_LOG_TZ` (or `file_tz`), and a part whose timestamps
carry zones is read as instants (or, under `file_tz`, as the wall clock
it writes, in that zone). A year-less timestamp (`Oct  7 00:15:01`)
takes its year from its own part's modification time.

`max_ms` is a part's highest timestamp, from its line index. Under
`file_tz`, in a part whose lines write several zone offsets (a change of
daylight saving time inside it), the line with the latest wall clock is
not known from the index, and `max_ms` is an upper bound: the highest
instant plus the largest offset the part writes, `max_is_bound` true.
The overlap check uses it.

Time gaps: in a ready chain of four parts with lines or more, D is the
median distance between the first timestamps of neighboring parts, and
a gap is reported where the time from a part's highest timestamp to the
next part's first is more than 1.5 × D. A gap or a missing part does not
make a chain invalid.

## The fingerprint

The first 16 hex digits of a SHA-256 over the parts in the provisional
order: each frozen part's name, device, inode, size and modification
time (ns), and the active file's name, device and inode. A rotation that
renames, compresses or deletes a part, a new part, a write to a frozen
part and a new active file change it; the active file growing does not.
A request that sees a part replaced between the listing and the read
lists the chain again and answers `409` with the current description.

## Response — 200 OK

```json
{
  "path": "/var/log/syslog",
  "name": "syslog",
  "state": "ready",
  "reasons": [],
  "fingerprint": "3173803a68e459cd",
  "parts": [
    {
      "name": "syslog-20260930-1790726400.gz",
      "path": "/var/log/syslog-20260930-1790726400.gz",
      "is_active": false,
      "key": "20260930-1790726400",
      "compression_format": "gzip",
      "size": 1930,
      "modified_at": "2026-09-30T00:00:00.000000Z",
      "is_indexed": true,
      "line_count": 109,
      "first_ms": 1790640000000,
      "last_ms": 1790726400000,
      "max_ms": 1790726400000,
      "max_is_bound": false,
      "global_start": 1,
      "time_format": {"format": "iso", "has_zone": false, "assumed_zone": "UTC"},
      "day_first": null,
      "example": "2026-09-29 00:00:00.000",
      "duplicates": []
    }
  ],
  "missing": [],
  "missing_count": 0,
  "gaps": [],
  "first_ms": 1790640000000,
  "last_ms": 1791324404000,
  "frozen_line_count": 1205,
  "line_count": null,
  "index_build": null,
  "cli_command": "rx logs show /var/log/syslog"
}
```

(One part shown.)

| Field | Type | Description |
|---|---|---|
| `path`, `name` | string | The chain's handle and name |
| `state` | string | `pending`, `ready` or `invalid` |
| `reasons` | array | `{code, parts, message, overlap_ms}` per failed check; empty unless invalid |
| `fingerprint` | string | 16 hex digits; see above |
| `parts` | array | The parts in the chain's order; fields below |
| `missing` | string[] | The names absent numbered parts would have, as `GET /v1/logs/chains` gives them: at most 100, lowest first |
| `missing_count` | int | How many numbered parts are missing, as `GET /v1/logs/chains` counts them |
| `gaps` | array | `{after, before, from_ms, to_ms}` per time gap |
| `first_ms`, `last_ms` | int64 \| null | The chain's first and last timestamp; null unless ready (and `last_ms` when the active file's last timestamped line is more than 16 MiB from its end) |
| `frozen_line_count` | int64 \| null | The lines of every part but the active file; null unless ready |
| `line_count` | int64 \| null | All lines, when the active file's count is known (its current index); null unless ready |
| `index_build` | object \| null | `{task_id, status, message, path, started_at}` of the chain's index task: for a pending chain the task this request started or joined (see above); otherwise the chain's last index task, running or ended (an ended one for `RX_TASK_TTL_MINUTES` after its end, while the chain's files keep the fingerprint it ended for; a failed one's `message` names the part and the error); null when there is none, and for a pending chain whose task cannot start because 128 chains' index tasks run or wait already. The description is the same with a task and without |
| `cli_command` | string | The equivalent `rx logs show` command |

### `parts[]` fields

| Field | Type | Description |
|---|---|---|
| `name`, `path` | string | The part's file name and path |
| `is_active` | bool | Whether it is the active file |
| `key` | string \| null | The number or date in its name as written; null for the active file |
| `compression_format` | string \| null | `gzip`, `bz2`, `xz` or `zstd`; null for a plain file |
| `size`, `modified_at` | | From the listing |
| `is_indexed` | bool | Whether a current line index of the part is stored |
| `line_count` | int64 \| null | Its lines; null when not known |
| `first_ms`, `last_ms`, `max_ms` | int64 \| null | Its first, last and highest timestamp; null when not known or when it has none |
| `max_is_bound` | bool | Whether `max_ms` is an upper bound |
| `global_start` | int64 \| null | The global number of its first line; null unless ready |
| `time_format` | object \| null | As in a samples answer |
| `day_first`, `example` | bool \| null, string \| null | As `GET /v1/time-range` gives them for the part: the day/month order of a slash date, and its first timestamp as its line writes it, from which a client shows the part's times in its own layout; null when not known |
| `duplicates` | string[] | Its other encodings, which rx does not read |

## Status codes

| Code | When |
|---:|---|
| `200 OK` | The description, whatever the state, invalid included |
| `400 Bad Request` | The handle ends in no name (`/var/log/`), or `file_tz` names no zone |
| `403 Forbidden` | The handle's directory is outside `--search-root`, or the directory or the name is hidden |
| `404 Not Found` | The handle names fewer than two parts, or its directory does not exist |
| `409 Conflict` | The body is the current description: `fingerprint` differs from it, or a part was replaced while the request read it |
| `422 Unprocessable Entity` | `path` is missing, or `fingerprint` is not 16 hex digits |
| `500 Internal Server Error` | The chain's files kept changing on every attempt to read them (three) |

## Examples

```bash
curl -sG 'http://127.0.0.1:7777/v1/logs/chain' \
    --data-urlencode 'path=/var/log/syslog' \
    | jq '{state, reasons, parts: [.parts[] | {name, line_count, global_start}]}'
```

## See also

- [`rx logs show`](../../cli/logs.md#rx-logs-show) — the same from a terminal
- [`GET /v1/logs/chains`](logs-chains.md) — the chains of a directory
- [`POST /v1/logs/index`](logs-index.md) — index every part of a chain
- [Configuration](../../configuration.md#log-chains) — `RX_CHAIN_OVERLAP_SECONDS`
