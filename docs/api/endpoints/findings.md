# `GET /v1/findings`, `POST /v1/findings`

The findings of one file: what the findings cache holds for it
(`GET`, which never scans), and a scan of the file for signatures as a
background task (`POST`). Since contract 2.0; `features` on
[`GET /health`](health.md) lists `signatures`.

The findings, their masking and their cache are those of
[`rx trace --signatures`](../../cli/trace.md#signatures): a scan here
and a scan there fill one cache and give the same findings
([Signatures](../../concepts/signatures.md),
[the findings cache](../../concepts/caching.md#findings)).

**Masking is the server's mode.** A server started with
`--mask-secrets` (or `RX_MASK_SECRETS=1`) masks secrets in every
excerpt and every line it gives; one started without it, the default,
gives the file's text as it is and never sets `masked`. No request
parameter changes the mode, and the start banner names it
([Masking](../../concepts/signatures.md#masking)). A scan by a server
in one mode is not current for a server in the other: its `GET` lists
every signature under `missing` until it scans the file itself.

## `GET /v1/findings`

### Purpose

- Show the findings of an open file, a page at a time, by line or by
  group
- Find the next or the previous finding from a line (`after_line`,
  `before_line` with `limit=1`)
- Mark the findings of the lines an editor shows (`lines=A-B`)
- Place the findings on a timeline (`ticks=W`)
- Learn which selected signatures the cache does not hold yet
  (`missing`), to scan them with `POST`

### Request

```text
GET /v1/findings?path=/var/log/app.log&group=severity&text=true&before=1&limit=2
```

| Parameter | Required | Description |
|---|---|---|
| `path` | yes | The file. Checked against the search roots like every path; a directory or a file that is not text answers `400` |
| `signatures` | no | The selection the summary describes, as [`--signatures=`](../../concepts/signatures.md#selection) takes it: ids, groups, tags, `all`, `default`, `-word`. Empty or absent: the default set. A word that names an unknown, refused or disabled signature answers `400` with the reason and the load error |
| `signature` | no | Only the findings of these signatures; repeatable, at most 256 |
| `group` | no | Only the findings of signatures of these groups; repeatable, at most 256 |
| `q` | no | Only the findings whose excerpt or signature name holds this text, compared without case; at most 1024 bytes. On a server that masks secrets the excerpt is masked already, so a masked value never matches |
| `lines` | no | `A-B` (or one line `N`): only the findings that overlap those lines |
| `after_line` | no | Only the findings whose first line is after `N` |
| `before_line` | no | Only the findings whose first line is before `N`, listed from the nearest down (the reverse order) |
| `offset` | no | How many filtered findings to pass over (default 0) |
| `limit` | no | The most findings in the page: 1 to 10,000, default 1,000 |
| `text` | no | `true` fills `lines` (and `before`) of the page's findings |
| `before` | no | With `text`: lines before each finding, 0 to 5 |
| `max_bytes` | no | With `text`: the most bytes of text the page holds; 0 or absent is `RX_FINDINGS_TEXT_MAX_BYTES`, which also bounds a larger value |
| `ticks` | no | `W`, 1 to 4,000: place the filtered findings on a timeline of `W` columns ([Ticks](#ticks)) |
| `file_tz` | no | Read the file's times in this zone, as [`GET /v1/samples`](samples.md) and [`GET /v1/time-range`](time-range.md) do ([Times](#times)) |

Every filter applies before the page is cut, so `offset` and `limit`
page through the filtered list, and `total` counts it. Filters of
different kinds combine (a group and `after_line`); repeated values of
one kind are alternatives.

The request reads the findings cache: the file's `meta.json`, the
entries of the selected signatures and the masked ranges, and the
file's identity through its pin (a stat and the two 64 KiB windows of
its fingerprint). It never scans and never waits for a scan. With
`text` it reads the lines of the page's findings from the file in one
pass; with `ticks`, the file's time range as `GET /v1/time-range` reads
it.

### Response — 200 OK

The invented `app.log` of the [example below](#example), after a scan
of five built-in signatures, with
`signatures=python-traceback,aws-access-key,private-key,error-line,blank-run`,
`group=severity&group=secret&text=true&before=1&ticks=10&limit=2` and
`file_tz=Europe/Berlin`, from a server started with `--mask-secrets`.
The state is `partial`: the five are current, the rest of the default
set is not.

```json
{
  "state": "partial",
  "checked": ["python-traceback", "aws-access-key", "private-key", "error-line", "blank-run"],
  "missing": [],
  "counts": {
    "aws-access-key": {"count": 1, "stored": 1},
    "blank-run": {"count": 0, "stored": 0},
    "error-line": {"count": 3, "stored": 3},
    "private-key": {"count": 1, "stored": 1},
    "python-traceback": {"count": 1, "stored": 1}
  },
  "series": {},
  "total": 5,
  "findings": [
    {
      "signature": "aws-access-key",
      "start_line": 2, "end_line": 2, "start_offset": 31, "end_offset": 101,
      "time_ms": 1768467601000,
      "excerpt": "2026-01-15 10:00:01 ERROR login with key=************ refused",
      "value_start": 41, "value_end": 61, "masked": true,
      "lines": ["2026-01-15 10:00:01 ERROR login with key=************ refused"],
      "before": ["2026-01-15 10:00:00 INFO start"]
    },
    {
      "signature": "error-line",
      "start_line": 2, "end_line": 2, "start_offset": 31, "end_offset": 101,
      "time_ms": 1768467601000,
      "excerpt": "2026-01-15 10:00:01 ERROR login with key=************ refused",
      "value_start": 20, "value_end": 25, "masked": true,
      "lines": ["2026-01-15 10:00:01 ERROR login with key=************ refused"],
      "before": ["2026-01-15 10:00:00 INFO start"]
    }
  ],
  "ticks": {
    "width": 10,
    "start_ms": 1768467600000,
    "end_ms": 1768469400000,
    "columns": [
      {"column": 0, "signatures": {
        "aws-access-key": {"count": 1, "first_line": 2},
        "error-line": {"count": 2, "first_line": 2},
        "private-key": {"count": 1, "first_line": 7}
      }},
      {"column": 9, "signatures": {"error-line": {"count": 1, "first_line": 11}}}
    ]
  },
  "cli_command": "rx trace /var/log/app/app.log --before=1 --signatures=python-traceback,aws-access-key,private-key,error-line,blank-run --mask-secrets"
}
```

| Field | Type | Description |
|---|---|---|
| `state` | string | `full`: every signature of the default set has a current cache entry; `partial`: at least one signature has one (an opt-in one counts), and not full; `none`: none has |
| `checked` | string[] | The signatures of the selection the cache answers, in catalogue order |
| `missing` | string[] | The signatures of the selection the cache does not answer: never scanned, or stale (the file, the signature or a setting changed). `POST` fills them |
| `counts` | object | Per checked signature: `count` (every finding, those past `RX_FINDINGS_MAX_PER_SIGNATURE` included) and `stored` (the ones kept, the first by line) |
| `series` | object | Per checked `density` signature: `{bucket_ms, start_ms, counts}`; `start_ms` read like `time_ms` ([Times](#times)) |
| `total` | int | The stored findings of the checked signatures that pass the filters |
| `findings` | array | The page, in the one order (first line, last line, signature id), or the reverse with `before_line`. The finding shape of [`GET /v1/trace`](trace.md) without `file` |
| `ticks` | object \| null | With `ticks=W`; `null` without |
| `cli_command` | string | The rx command that gives the same findings: `rx trace PATH --signatures=SELECTION`, with `--mask-secrets` on a server that masks |

### Text

With `text=true` every finding of the page gets `lines`, its first to
its last line, and `before`, the `before` lines before it, read from the
file and shown as every carrier of finding text shows them
([the text of a finding](../../cli/trace.md#the-text-of-a-finding)). On
a server that does not mask secrets each line is the file's line as it
is; on one that does:

- a value any loaded redacting signature matches is masked, whether or
  not it is selected (`key=************` above: `aws-access-key` and
  `high-entropy-token` both match there, and are masked as one span);
- every body line of a redacting block or run (the base64 lines of a
  private key) is `[masked, N bytes]`, in `lines` and in `before` alike;
- no line past the text the scan read is shown, so a log that grew
  since its scan shows no line no masked range covers;
- each line is valid UTF-8 and cut to `RX_MAX_LINE_TEXT_BYTES`.

[Masking](../../concepts/signatures.md#masking) states the whole rule.

The page's text together is held to `max_bytes` (or
`RX_FINDINGS_TEXT_MAX_BYTES`), taken in line order, each line costing
its bytes and one for its line break: the finding at which it runs out
and every later one have no `lines` and `before`, and
`lines_truncated: true`. Without `text`, no finding has `lines`.

### Times

`time_ms` is the time of a finding's first line, or of the nearest
earlier stamped line within `RX_TIMESTAMP_LOOKBACK_KB`, as a UTC instant
in milliseconds, read the way every answer reads the file's times: a
file whose timestamps carry no zone in `RX_LOG_TZ`. Under `file_tz`
every line's written wall clock is read in that zone instead, so a
finding's `time_ms` is the `line_timestamps` value
[`GET /v1/samples`](samples.md) gives its line under the same
`file_tz`. For a file whose timestamps carry zones that is the
wall clock its stamped line writes, read in the zone: the scan keeps,
beside each finding's time, the zone offset that line writes, so no
line index is needed and the answer is the same with one and without.
An epoch value is read as a wall clock in the zone, as samples reads
it. A `density` series' `start_ms` belongs to no line: it is read the
same way in the file's own frame, and under `file_tz` the series of a
file whose timestamps carry zones are left out.

The answer is the same with a line index and without one, `ticks`
aside: the index only spares the read of the head of the text for the
format, and of the end of the text for the time range of `ticks`. The
span of `ticks` is the time range `GET /v1/time-range` gives, and
without an index that has no first or last time for a gzip, bzip2, xz
or plain zstd file (`source: none`), nor a last time for a plain or
seekable zstd file whose last timestamp lies more than 16 MiB before
its end, or a seekable one whose frames are too large to read back.
The time it lacks is then `null` in `ticks`, and `columns` is empty,
until the file is indexed ([Ticks](#ticks)). `time_ms` and every other
field are the same either way. `rx serve` keeps the decoded findings
of the last 8 files read (at most 64 MiB, the
least recently read file out first), so paging, filters and ticks of
one file decode its cache files once; `meta.json` and the file's
identity are still read on every request, so a kept answer is never
stale.

### Ticks

`ticks=W` places the filtered findings with a time on a timeline of `W`
columns over the file's time range: `start_ms` and `end_ms` are the
`first_ms` and `last_ms` [`GET /v1/time-range`](time-range.md) gives for
the same `file_tz`, the span a timeline is drawn over. A finding whose
time is `t` falls in column

```text
floor((t - start_ms) * W / (end_ms - start_ms + 1))
```

so `start_ms` is in column 0 and `end_ms` in column `W - 1`. `columns`
lists each column that holds findings, in order, with, per signature,
`count` and `first_line` (the first line of the first of them). A
finding without a time, or with a time outside `start_ms` to `end_ms`,
is in no column. When the time range lacks a first or last timestamp
(a stream-compressed file without an index, `source: none`, lacks both;
see [Times](#times) for the other cases), what it lacks is `null` and
`columns` is empty; ask again once the file's index is built.

### Status codes

| Code | When |
|---:|---|
| 200 | The answer, `none` included |
| 400 | A directory, a file that is not text, a selection that cannot be resolved, a bad `lines` or `file_tz` |
| 403 | The path is outside every search root (`SandboxError`), hidden, or cannot be read |
| 404 | The file does not exist |
| 422 | `path` missing, a value out of its range (`limit`, `before`, `ticks`), more than 256 `signature` or `group` values, or a `signatures` or `q` value over its length |
| 500 | The cache or the file could not be read |

## `POST /v1/findings`

Scans the file, or every file under a directory, for the selected
signatures in a background task and caches what it finds.

### Request

```text
POST /v1/findings
Content-Type: application/json

{"path": "/var/log/app/app.log", "signatures": "default", "force": false}
```

| Field | Required | Description |
|---|---|---|
| `path` | yes | The file, checked as for `GET`, or a directory: every file under it is scanned ([A directory](#a-directory)) |
| `signatures` | no | The selection, as for `GET`; default `default` |
| `force` | no | Scan every selected signature again, those with a current cache entry too; default `false` |

Without `force` the task scans only the selected signatures the cache
does not hold, in one pass over the file; a selection the cache holds
whole completes without reading the file. At most
`RX_MAX_SIGNATURE_SCANS` scans read files at once (default 1); later
ones wait, `queued`, in the order they came. While a scan of the same
file runs, a second `POST` for it returns that task (`Joined …`): it
scans what it was started for, and a later `GET` shows what is still
missing. A scan of a file runs beside an index build or a compression of
it.

### Response — 202 Accepted

```json
{
  "task_id": "440cb10b-c583-42ca-9283-840f514c8bd4",
  "status": "queued",
  "message": "Scanning /var/log/app/app.log for 64 signatures; follow GET /v1/tasks/440cb10b-c583-42ca-9283-840f514c8bd4",
  "path": "/var/log/app/app.log",
  "started_at": "2026-10-11T08:49:44.199639Z",
  "cli_command": "rx scan /var/log/app/app.log --signatures=default"
}
```

`cli_command` is the [`rx scan`](../../cli/scan.md) command that does
what the task does, the same in every status of the task: the
selection spelled out, `--force` when the request forced, and
`--mask-secrets` when the server masks secrets. A request that joined
a running task gets that task's command. (The task's result, a
`FindingsResponse`, keeps the `cli_command` of `GET /v1/findings`, an
`rx trace --signatures=…` command that prints the findings.)

Follow it at [`GET /v1/tasks/{task_id}`](tasks.md): operation
`findings`, `queued` while it waits, `running` from its first read,
with `bytes_read` and `bytes_total` (the file's bytes as stored on
disk) and `progress`. Its result is the answer `GET /v1/findings` gives
for the selection without filters: the summary and the first page of
1,000 findings, without text. [`DELETE /v1/tasks/{task_id}`](tasks.md#delete-v1taskstask_id)
cancels it: the scan stops and caches nothing.

A scan that fails ends the task `failed` with the reason. A file that
changes while it is scanned is answered and not cached, or fails when it
got shorter.

### A directory

Given a directory, one task scans every file the file tree lists under
it, recursively, one at a time, in the walk's order, as
`rx scan DIR --recursive` does, which is its `cli_command`
(`rx scan /var/log/app --recursive --signatures=default`). A file that
is not text, and one whose findings are current for every selected
signature (unless `force`), is skipped; one whose scan fails is
recorded and the task goes on. Each file waits for its place in the
scan queue, so `RX_MAX_SIGNATURE_SCANS` still holds, and the task takes
one place among the 256 unfinished findings tasks. The status has
`files_done` and `files_total`, and `bytes_read` and `bytes_total` of
the file it reads; the result is a `FolderTaskResult` with the counts
and the findings of every file scanned or current. `DELETE` cancels it
between two files or inside a file's scan: the files scanned before the
cancel keep their findings. See
[Tasks over a directory](tasks.md#tasks-over-a-directory).

### Status codes

| Code | When |
|---:|---|
| 202 | The task, started or joined |
| 400 | A file that is not text, a selection that cannot be resolved, or a body that is not valid JSON |
| 403 | The path is outside every search root, hidden, or cannot be read |
| 404 | The file does not exist |
| 422 | `path` missing, a field of the wrong type, or a `signatures` value over 4096 bytes |
| 503 | 256 findings tasks are queued or running, as many as the server holds; no task starts (a `POST` for a file whose scan is one of them still joins it). `Retry-After: 5` |

## Example

An invented log, served with `rx serve --search-root=/var/log/app`,
scanned for five built-in signatures:

```text
2026-01-15 10:00:00 INFO start
2026-01-15 10:00:01 ERROR login with key=AKIAIOSFODNN7EXAMPLE refused
Traceback (most recent call last):
  File "app.py", line 3, in <module>
ValueError: bad input
2026-01-15 10:00:05 INFO between
-----BEGIN PRIVATE KEY-----
RkFLRUtFWUJPRFlGQUtFS0VZQk9EWQ==
-----END PRIVATE KEY-----
2026-01-15 10:00:09 ERROR request timeout after the key
2026-01-15 10:30:00 ERROR much later
```

```bash
sel=python-traceback,aws-access-key,private-key,error-line,blank-run
curl -s "localhost:7777/v1/findings?path=/var/log/app/app.log&signatures=$sel"
# {"state":"none","checked":[],"missing":["python-traceback","aws-access-key","private-key","error-line","blank-run"], …, "total":0,"findings":[], …}
curl -s -X POST -H 'Content-Type: application/json' -d "{\"path\":\"/var/log/app/app.log\",\"signatures\":\"$sel\"}" localhost:7777/v1/findings
# {"task_id":"440cb10b-…","status":"queued","message":"Scanning /var/log/app/app.log for 5 signatures; …", …}
curl -s localhost:7777/v1/tasks/440cb10b-c583-42ca-9283-840f514c8bd4
# {"status":"completed","operation":"findings","progress":1,"bytes_read":408,"bytes_total":408,"result":{"state":"partial", …,"total":6, …}}
```

## See also

- [`GET /v1/signatures`](signatures.md) — the catalogue the selections name
- [`GET /v1/tasks/{task_id}`, `DELETE /v1/tasks/{task_id}`](tasks.md) — the scan task
- [`GET /v1/tree`](tree.md) — each file's `findings` label
- [`GET /v1/trace`](trace.md) — `signatures` scans the files a search covers
- [`rx trace --signatures`](../../cli/trace.md#signatures) — the same scan from a terminal
