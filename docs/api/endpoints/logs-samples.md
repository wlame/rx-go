# `GET /v1/logs/samples`

Lines of a log chain, by the chain's global line numbers, by a part and
its own line numbers, or by time: what [`GET /v1/samples`](samples.md)
gives for one file, for the parts of a rotated log read as one text.

## Purpose

A chain numbers its lines as one text: line 1 is the first line of its
oldest part, and a part's lines follow the lines of every part before
it ([`GET /v1/logs/chain`](logs-chain.md) gives each part's
`global_start`). This route answers a line, a range or a time in that
numbering, reading each part with the rules one file is read with, and
says which part every line comes from.
[`rx logs samples`](../../cli/logs.md#rx-logs-samples) gives the same
answer from a terminal.

## Request

```text
GET /v1/logs/samples?path=<handle>&lines=<spec>[&part=<name>][&context=<n>][&before_context=<n>][&after_context=<n>][&file_tz=<zone>][&fingerprint=<fp>]
GET /v1/logs/samples?path=<handle>&timestamps=<T>[&timestamps=<T>...][&context=<n>]...
```

### Query parameters

| Parameter | Type | Required | Description |
|---|---|:-:|---|
| `path` | string | yes | The chain's handle, as `GET /v1/logs/chains` gives it in `path` |
| `lines` | string | one of | Comma-separated line numbers and ranges, as `GET /v1/samples` takes them (`100,200-300,-1`): the chain's global numbers, or with `part` the part's own. `-N` counts back from the end of the chain (or of the part) |
| `part` | string | no | The bare name of a part, as `GET /v1/logs/chain` lists it (`syslog.3.gz`): `lines` then numbers that part as `GET /v1/samples` numbers it on its own. Only with `lines` |
| `timestamps` | string | one of | A time or time range (`T`, `T1..T2`, `..T2`, `T1..`), as `GET /v1/samples` takes it; repeat the parameter for several, at most 1,000 |
| `context` | int | no | Lines of context before and after each single line or time (default 3, at most 100) |
| `before_context`, `after_context` | int | no | Override `context` on one side |
| `file_tz` | string | no | Read every part's timestamps as the wall clock each line writes, in this zone, as `GET /v1/samples` reads one file |
| `fingerprint` | string | no | The fingerprint of a description the client holds (16 hex digits): `409` with the current description when the chain's files changed since |

Exactly one of `lines` and `timestamps` is given.

### Header

| Header | Description |
|---|---|
| `Prefer: respond-async` | Answer `202` with the task the request waits for, once `RX_SAMPLES_WAIT_SECONDS` has passed, instead of waiting for it |

## Three ways to address lines

- **Global lines** (`lines`): the chain's numbers. A window crosses part
  edges: line 1 of a part follows the last line of the part before, with
  its context. `-1` is the chain's last line, the active file's last;
  lines past the frozen parts are the active file's, however far it has
  grown since it was described.
- **A part and its own lines** (`part` and `lines`): the numbers
  `GET /v1/samples` gives the part on its own. In a ready chain the
  context of a single line crosses the part's edges and the key's
  target is its global line; a range stays in the part, and a line past
  the part's end names none (`-1`), its context the part's lines before
  it. This is the only way to read a chain that is not ready yet.
- **Time** (`timestamps`): the line at `T` is the first line, in the
  chain's order, whose own timestamp is at or after `T`; a range runs
  from the line at `T1` to the line before the first line later than
  `T2`, as for one file. The queries are read once for the whole chain,
  as one file holding it would read them from its head: the format of
  its first part with timestamps, and the zone offset of its first
  timestamp for a time without a zone in a log whose timestamps carry
  zones (unless `RX_QUERY_TZ` names a zone). A time of day without a
  date takes its date from the chain's first and last timestamps, which
  must fall on one day, as one file's must. `T` in a time gap between
  two parts gives the first line of the later part; before the chain,
  line 1; after it, `-1`. The part that holds the line is the first one
  whose highest timestamp reaches `T`: a part whose lines go back in
  time (its last timestamp earlier than its highest) is still found.
  Under `file_tz` a part that writes several zone offsets has only an
  upper bound of its highest time (`max_is_bound`): it is searched, and
  when it holds no line at or after `T` the search moves to the next
  part.

## Pieces

Each key's lines come as **pieces**, one per part its window touches, in
the chain's order. A window of 100 lines from 123456, where
`nginx.log.8.gz` ends at line 123475, gives a piece of 20 lines from
`nginx.log.8.gz` and one of 80 from `nginx.log.7.gz`. An empty part
gives none.

| Field | Type | Description |
|---|---|---|
| `part` | string | The part's name, as in `parts` |
| `first_local_line` | int64 | The part's own number of the piece's first line |
| `first_global_line` | int64 | Its global number: the part's `global_start` + `first_local_line` − 1; `-1` before the chain is ready |
| `lines` | string[] | The lines, without their line breaks |
| `line_timestamps` | (int64 \| null)[] \| null | Each line's effective timestamp, as `GET /v1/samples` gives `line_timestamps`; see below |
| `part_start` | bool | The piece begins at the part's first line |
| `part_end` | bool | The piece ends at the part's last line: its line count when known, or the end of its text, which the read reached before the window's end |
| `cli_command` | string | The `rx samples` command that gives exactly the piece's lines from the part: `rx samples PART --lines=A-B` |

### Timestamps across a part edge

A line without a timestamp of its own (a traceback, a continued
message) carries the timestamp of the nearest earlier line that has
one, when that line starts at most `RX_TIMESTAMP_LOOKBACK_KB` before it.
In a ready chain that look back continues into the parts before: lines
at the start of a part that continue a record the part before began get
the value the parts read as one file give them. It is answered from the
earlier part's line index (its last timestamped line, and the length of
its text) without reading that part; the part is read only when the
index cannot answer: its last byte when a line lies exactly at the look
back's distance and the part may end without a line break (the chain
counts one there), or its last timestamped line under `file_tz` when
the index records no zone offsets. Before the chain is ready a part is
read alone, and the look back stops at its first line.

## Before the chain is ready

A request addressed to a part (`part` and `lines`) is answered at once
from that part alone: its context and its look back stop at the part's
edges (`part_start`, `part_end` say so), the global numbers are `-1`,
and `index_build` names the chain's index task, which the request
starts or joins as `GET /v1/logs/chain` does.

A request by global line or by time waits for the chain's index task,
up to `RX_SAMPLES_WAIT_SECONDS` with `Prefer: respond-async` (then
`202` with the task), or as long as it runs without; then it describes
the chain again and answers. A chain whose last index task failed for
the same files answers `500` with the task's message
([`POST /v1/logs/index`](logs-index.md) starts it again).

## What it reads

The chain's description, as `GET /v1/logs/chain` reads it, then each
part a window touches, once per request for all its pieces, through
the path `GET /v1/samples` reads one file by: its current line index, an
answer from the head of a part that wants an index and has none (the
active file, usually; its index build starts in the background and is
named in `index_build`), or a wait for that build (`202` with its task
under `Prefer: respond-async`). Every read goes through the part's pin:
a part renamed or replaced after the description answers `409`.

`RX_SAMPLES_MAX_LINES` and `RX_SAMPLES_MAX_BYTES` bound the whole
answer, summed over its keys and pieces; each part's read is limited to
what the answer has room for still. Over them, `400`.

## Response

```json
{
  "path": "/var/log/syslog",
  "name": "syslog",
  "state": "ready",
  "fingerprint": "e8ed018e0ff41f34",
  "parts": [ … as GET /v1/logs/chain … ],
  "before_context": 1,
  "after_context": 1,
  "lines": {"15641": 15641},
  "timestamps": {},
  "samples": {
    "15641": [
      {"part": "syslog-20260930-1790726400.gz", "first_local_line": 15640, "first_global_line": 15640,
       "lines": ["…", "…"], "line_timestamps": [1790726399000, 1790726400000],
       "part_start": false, "part_end": true,
       "cli_command": "rx samples /var/log/syslog-20260930-1790726400.gz --lines=15640-15641"},
      {"part": "syslog-20261001-1790812801.gz", "first_local_line": 1, "first_global_line": 15642,
       "lines": ["…"], "line_timestamps": [1790726400000],
       "part_start": true, "part_end": false,
       "cli_command": "rx samples /var/log/syslog-20261001-1790812801.gz --lines=1-1"}
    ]
  },
  "index_build": null,
  "cli_command": "rx logs samples /var/log/syslog --lines=15641 --context=1"
}
```

| Field | Type | Description |
|---|---|---|
| `path`, `name`, `state`, `fingerprint`, `parts` | | As `GET /v1/logs/chain` gives them |
| `before_context`, `after_context` | int | The context of each single line or time |
| `lines` | object | Each key of a lines request (`-N` keyed by the line it names) to its global target: the line itself, a range's first line; `-1` when the chain has no such line, and before the chain is ready |
| `timestamps` | object | Each time query to the global line it found (a range's first); `-1` when none |
| `samples` | object | Each key to its pieces; `null` when the chain has no line of it, `[]` for a range that ends at 0 |
| `index_build` | object \| null | The background index build this answer started or joined: the chain's index task for a pending chain; for a ready chain the build of a part read from its head; `null` otherwise |
| `cli_command` | string | The equivalent `rx logs samples` command |

## Status codes

| Code | When |
|---:|---|
| `200 OK` | The answer |
| `202 Accepted` | Only with `Prefer: respond-async`: the chain's index task, or a part's index build, did not end within `RX_SAMPLES_WAIT_SECONDS`. The body is that task; poll [`GET /v1/tasks/{id}`](tasks.md), then ask again |
| `400 Bad Request` | Neither or both of `lines` and `timestamps`; `part` without `lines` or not the bare name of a part of the chain; a bad lines spec, time or `file_tz`; more than 1,000 times; an answer over `RX_SAMPLES_MAX_LINES` or `RX_SAMPLES_MAX_BYTES` |
| `403 Forbidden` | The handle's directory is outside `--search-root`, or hidden |
| `404 Not Found` | The handle names fewer than two parts, or its directory does not exist |
| `409 Conflict` | The body is the current description: `fingerprint` differs from it, or a part was renamed or replaced while the request read it |
| `422 Unprocessable Entity` | The chain is invalid (the detail lists the reasons, as `GET /v1/logs/chain` describes them); or `path` is missing, or `fingerprint` is not 16 hex digits |
| `500 Internal Server Error` | The chain's last index task failed for these files, or its files kept changing |

## Examples

```bash
# 100 lines from global line 123456
curl -sG 'http://127.0.0.1:7777/v1/logs/samples' \
  --data-urlencode 'path=/var/log/nginx.log' --data-urlencode 'lines=123456-123555'

# line 500 of one part, with context across its edges
curl -sG 'http://127.0.0.1:7777/v1/logs/samples' \
  --data-urlencode 'path=/var/log/syslog' --data-urlencode 'part=syslog.3.gz' \
  --data-urlencode 'lines=500' --data-urlencode 'context=10'

# the lines of one hour, which may span several parts
curl -sG 'http://127.0.0.1:7777/v1/logs/samples' \
  --data-urlencode 'path=/var/log/syslog' \
  --data-urlencode 'timestamps=2026-10-03T14:00..2026-10-03T15:00'
```

## See also

- [`GET /v1/logs/chain`](logs-chain.md) — the parts, their order and global starts
- [`GET /v1/samples`](samples.md) — the same for one file
- [`rx logs samples`](../../cli/logs.md#rx-logs-samples) — the CLI equivalent
