# `GET /v1/logs/trace`

A search of rotated logs: what [`GET /v1/trace`](trace.md) does, with
the parts of each log chain searched in the chain's order and each of
their matches placed in its chain.

## Purpose

A directory such as `/var/log` holds rotated logs: `syslog`,
`syslog.1`, `syslog.2.gz`, … [`GET /v1/trace`](trace.md) searches them
as files with nothing to say they belong together, in the order of
their names (`syslog`, `syslog.1`, `syslog.10.gz`, `syslog.2.gz`). This
route groups them into [log chains](logs-chains.md), searches each
chain's parts oldest first, and gives each match its line in the chain
beside its line in its file, the numbers
[`GET /v1/logs/samples`](logs-samples.md) reads.
[`rx logs trace`](../../cli/logs.md#rx-logs-trace) gives the same
answer from a terminal.

## Request

```text
GET /v1/logs/trace?path=<dir|handle|file>[&path=...]&regexp=<pattern>[&regexp=...][&max_results=N]...
```

The parameters are those of [`GET /v1/trace`](trace.md#request), with
the same meaning and checks: `path` and `regexp` repeat, `max_results`,
`context`, `before_context`, `after_context`, the matching flags
(`ignore_case`, `word_regexp`, `line_regexp`, `fixed_strings`, `pcre2`),
`no_cache`, `no_index`, `no_recursive`, `request_id` and the
`hook_on_*` webhooks.

### How each `path` is read

In this order:

1. **A directory** is walked as `GET /v1/trace` walks it (recursively
   unless `no_recursive=true`). The files of each directory it lists are
   grouped into chains, by name, as
   [`GET /v1/logs/chains`](logs-chains.md) groups them.
2. **A chain's handle** (its directory joined with its name, as
   `GET /v1/logs/chains` gives it in `path`) is that chain. The handle
   need not exist as a file: a chain of dated parts only has no active
   file.
3. **Anything else is a file**, searched on its own. A part's own path
   (`/var/log/syslog.2.gz`) is a file: it gets no chain.

A path that is none of these is `404`; a file named on its own that
cannot be opened is `403`, as on `GET /v1/trace`.

Whatever the request reaches more than once is searched once: a `path`
given twice (however it is spelled), a directory and a handle or a file
in it, a link to a directory, a directory under another spelling of its
path (another case on a case-insensitive disk, a bind mount). Each chain
is described once and has one entry in `chains` (a chain is the same
when its directory, by the device and inode it had when it was listed,
and its name are; on a filesystem that gives every directory inode 0,
when its handle is, or when its parts' paths with every link resolved
are), each part is searched once under one file id (a part is the same
file under any spelling of its directory, by its device and inode), each
other file is searched once under one file id (a file is the same when
its path with every link resolved is), and so each match comes once. A
part's own path named beside its chain's handle or directory is a part
of that chain, whichever comes first. Another encoding of a part named
beside its chain (`syslog.3.gz` beside the handle `syslog`, whose chain
reads `syslog.3`) is skipped with `duplicate_part` and never searched,
whichever comes first and under whatever path it is named (by its device
and inode), so each of its lines comes once, from the part the chain
reads; it is named once in `skipped_files`, under the path its chain
lists it by. Two chains that still give one identity (an inode reused
while the request runs) are both searched: the first as its chain, the
second's parts that are not the first's files as files of their own,
with `chain` null. `path` in the answer lists the paths as the request
gave them.

A file of its own, and a part's own path, are known by their paths
alone: two hard links are two files, and so is one file named under two
case spellings of its directory, each searched.

## The order of the search

The parts of a chain are searched in the chain's order, as
[`GET /v1/logs/chain`](logs-chain.md) describes it: by time once the
chain is ready (or invalid), in the provisional order of their names
before. In a directory, a chain takes the place of the first of its
files the walk lists, and its parts follow there; every other file
keeps its place. File ids (`f1`, `f2`, …) are given in that order, so
the matches are listed in it (by file id, then offset, then pattern)
and `max_results` cuts in it. As for a trace, the cap stops the scan
early and makes no promise about which matches it finds; the ones found
are listed in part order.

Each part is searched as one file by the trace engine: its own line
numbers and byte offsets (in its decompressed text), its own context
window, which never crosses into the next part, its own trace cache
entry and line index.

Left out of the search, each named in `skipped_files` with its reason
in `skip_reasons`:

| Reason | Means |
|---|---|
| `duplicate_part: the same part of its log chain as NAME, which is searched` | another encoding of one generation (`syslog.3` beside `syslog.3.gz`): the chain reads one, in the order plain, seekable zstd, zstd, gzip, bzip2, xz |
| the reason a trace gives (`permission denied`, `cannot be read`, …) | a part the listing could not read; its chain is invalid with the reason `unreadable`, whose message gives the same words |

A chain of more than 10,000 parts is not read as one text: its files are
searched as files of their own, where the walk lists them, and its
entry in `chains` has no parts and the reason `too_many_parts`.

Each chain is described from its parts' stored line indexes and the
head and tail of its active file, as `GET /v1/logs/chain` describes it,
but no index build starts: a chain whose frozen parts are not all
indexed is `pending`, searched in the provisional order, and its
matches have `chain_line` `-1`. No part is read to describe its chain,
so a search capped by `max_results` reads what `GET /v1/trace` reads on
the same files. `POST /v1/logs/index` (or a `GET /v1/logs/chain`, or
`rx logs index`) builds the indexes; the next search gives every chain
line. `rx logs trace` describes chains the same way. `no_index` applies to the search, as on `GET /v1/trace`,
not to the description.

## Response

The body of [`GET /v1/trace`](trace.md#response-200-ok), field for field, with
two additions: `chains`, and `chain` and `chain_line` on each match.

```json
{
  "request_id": "…",
  "path": ["/var/log"],
  "patterns": {"p1": "Accepted"},
  "files": {"f1": "/var/log/auth.log.3.gz", "f2": "/var/log/auth.log.2.gz", "f3": "/var/log/auth.log.1",
            "f4": "/var/log/auth.log", "f5": "/var/log/notes.txt"},
  "matches": [
    {"pattern": "p1", "file": "f2", "offset": 81234, "relative_line_number": 500, "absolute_line_number": 500,
     "line_text": "…", "submatches": [ … ], "line_text_truncated": false, "submatches_truncated": false,
     "chain": "c1", "chain_line": 2911},
    {"pattern": "p1", "file": "f5", "offset": 12, "relative_line_number": 2, "absolute_line_number": 2,
     "line_text": "…", "submatches": [ … ], "line_text_truncated": false, "submatches_truncated": false,
     "chain": null, "chain_line": -1}
  ],
  "chains": {
    "c1": {"path": "/var/log/auth.log", "name": "auth.log", "parts": ["f1", "f2", "f3", "f4"],
           "fingerprint": "730320a18b6dd2fc", "state": "ready", "reasons": []}
  },
  "scanned_files": [ … ],
  "skipped_files": [],
  "skip_reasons": [],
  "max_results": null,
  "file_chunks": { … },
  "context_lines": {},
  "before_context": null,
  "after_context": null,
  "time": 0.042,
  "cli_command": "rx logs trace /var/log --regexp=Accepted"
}
```

| Field | Type | Description |
|---|---|---|
| `chains` | object | The chains found, by id: `c1`, `c2`, … in the order found. Empty when there is none |
| `chains.*.path`, `name` | string | The chain's handle and name, as `GET /v1/logs/chain` takes and gives them |
| `chains.*.parts` | string[] | The file ids of its parts searched, in its order; a part that cannot be read has none. Empty for a chain of more than 10,000 parts |
| `chains.*.fingerprint`, `state`, `reasons` | | As `GET /v1/logs/chain` gives them |
| `matches[].chain` | string \| null | The id of the chain whose part the match is in; `null` for a file searched on its own |
| `matches[].chain_line` | int64 | The match's line in its chain: the part's `global_start` + `absolute_line_number` − 1, the global line `GET /v1/logs/samples` takes. `-1` when not known: a file of its own, a chain that is pending or invalid, and a match whose `absolute_line_number` is `-1` |

Every other field means what it means on `GET /v1/trace`: `files` keep
the paths as the walk or the request spelled them, a match's
`absolute_line_number` and `offset` are its part's own, and
`scanned_files` lists every file searched when a `path` is a
directory. The webhooks fire as for a trace, with its payloads: no
chain field reaches them.

## Status codes

| Code | When |
|---:|---|
| `200 OK` | The answer, with an invalid chain's parts searched too |
| `400 Bad Request` | A pattern ripgrep cannot compile; a webhook URL refused; `hook_on_match` without `max_results` |
| `403 Forbidden` | A path outside `--search-root`, into a hidden entry, or a file named on its own that cannot be opened |
| `404 Not Found` | A path that is no directory, no chain's handle and no file |
| `409 Conflict` | A part of a chain was renamed or replaced between the listing that found it and the read that described it (a rotation ran meanwhile); send the request again |
| `422 Unprocessable Entity` | `path` or `regexp` missing, or a value out of range |
| `500 Internal Server Error` | The search failed |
| `503 Service Unavailable` | ripgrep is not installed |

## Examples

```bash
# every chain of /var/log, each oldest part first
curl -sG 'http://127.0.0.1:7777/v1/logs/trace' \
  --data-urlencode 'path=/var/log' --data-urlencode 'regexp=Accepted publickey'

# one chain by its handle, the first 100 matches
curl -sG 'http://127.0.0.1:7777/v1/logs/trace' \
  --data-urlencode 'path=/var/log/syslog' --data-urlencode 'regexp=error' \
  --data-urlencode 'max_results=100'
```

## See also

- [`GET /v1/trace`](trace.md) — the same search over files
- [`GET /v1/logs/samples`](logs-samples.md) — the lines around a `chain_line`
- [`rx logs trace`](../../cli/logs.md#rx-logs-trace) — the CLI equivalent
