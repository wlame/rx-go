# Log chains

Rotation tools split one log into many files: `syslog`, `syslog.1`,
`syslog.2.gz`, or `auth.log-20261001.gz`. A person reading the log wants
one thing to scroll, search and jump in by line or by time. A **log
chain** is that thing: the files of one rotated log in one directory,
ordered by time and numbered as one text, read and searched through the
usual rx code for each file.

```bash
rx logs list /var/log
rx logs show /var/log/syslog
rx logs samples /var/log/syslog --timestamps=2026-10-02T23:59:50..2026-10-03T00:00:10
rx logs trace 'Accepted publickey' /var/log
```

The commands are [`rx logs`](../cli/logs.md); the routes are under
[`/v1/logs`](../api/index.md#endpoint-summary). Both call the same code
and give the same answers.

**The rule every chain answer keeps:** a chain answers as the
concatenation of its parts. Decompress the parts in the chain's order
into one file, adding a line break after a part that does not end with
one, and every line number, line, time and match rx gives for the chain
is the one it gives for that file, apart from saying which part each
line comes from. Line `G` of the chain is line `G − global_start + 1`
of its part, where a part's `global_start` is 1 plus the line counts of
the parts before it. And an answer is the same before and after the
parts are indexed, apart from a number that is `-1` ("not known") in
one of them.

## Words

| Word | Meaning |
|---|---|
| chain | The files of one rotated log in one directory, ordered by time |
| part | One file of a chain |
| active part | The part whose name has no number or date (`syslog`): the file a program still writes. It may be absent, and it is the only part that may grow |
| frozen part | Every part but the active one |
| chain name | The active part's file name, whether that file exists or not: `syslog`, `app.log` |
| handle | The chain's directory joined with its name, written as a path: `/var/log/syslog`. Commands and routes name a chain by its handle |
| key | The number or date in a part's name (`3`, `20261001`). It gives the order before the times are known, and finds missing numbers |
| global line | A line's number in the chain: the line counts of the parts before it plus its line in its part |
| local line | A line's number in its part, as [`rx samples`](../cli/samples.md) numbers the part on its own |
| fingerprint | 16 hex digits that change when the chain's files change (see [Live logs](#live-logs)) |

## Which files form a chain

Membership comes from the names in one directory listing alone; no
file's text decides it, beyond the check that a file is text.

### Name templates

A file is a rotated part when its name has the shape of one of these
templates. They are tried in this order, and the first that matches the
name wins.

| Template | Shape | Chain name | Key | Newest part | Needs the active file |
|---|---|---|---|---|---|
| `numbered` | `{name}.{N}`, N of 1 to 5 digits | `{name}` | number | lowest number | no |
| `dated` | `{name}{sep}{DATE}`, optionally followed by `{sep}{digits}` | `{name}` | date | latest date | no |
| `dated-ext` | `{stem}{sep}{DATE}`, optionally followed by `{sep}{N}`, then `.{ext}` | `{stem}.{ext}` | date | latest date | no |
| `numbered-ext` | `{stem}{sep}{N}.{ext}`, N of 1 to 5 digits | `{stem}.{ext}` | number | lowest number | yes |

- `sep` is `.`, `-` or `_`.
- `ext` is a letter followed by up to seven letters or digits.
- `DATE` is `yyyymmdd`; `yyyymmddhh` (a 10-digit number that is no valid
  date and hour is read as epoch seconds); `yyyymmdd` followed by `-`,
  `T` or `_` and `HHMMSS`; or `yyyy-mm-dd`, optionally followed by `_`
  or `T` and `HH`, `HH-MM`, `HHMM`, `HH-MM-SS`, `HHMMSS` or
  `HH-MM-SS.mmm`.
- A compression suffix (`.gz`, `.bz2`, `.xz`, `.zst`) may end every
  shape. It is not part of the generation: `syslog.2` and `syslog.2.gz`
  are one generation in two encodings.
- A name is matched as the text it is. Spaces, brackets, unicode and
  regular-expression characters in it are only characters.
- `dated-ext` is tried before `numbered-ext`: `app-2026-10-01.3.log` has
  both shapes, and its date names it.
- `numbered-ext` parts count only when the active file `{stem}.{ext}`
  exists, so `node-1.log` and `node-2.log` alone are no chain.

Some names and what the templates make of them:

| Name | Template | Chain name | Key | Key kind |
|---|---|---|---|---|
| `syslog.1` | `numbered` | `syslog` | `1` | number |
| `dpkg.log.11.gz` | `numbered` | `dpkg.log` | `11` | number |
| `dmesg.0` | `numbered` | `dmesg` | `0` | number |
| `my app (1).log.2.gz` | `numbered` | `my app (1).log` | `2` | number |
| `report.2023` | `numbered` | `report` | `2023` | date (a year) |
| `auth.log-20261001.gz` | `dated` | `auth.log` | `20261001` | date |
| `syslog-20261001-1790812801.gz` | `dated` | `syslog` | `20261001-1790812801` | date |
| `app.log.2026-10-01_12` | `dated` | `app.log` | `2026-10-01_12` | date |
| `access_log.1790726400` | `dated` | `access_log` | `1790726400` | date |
| `app-2026-10-01.3.log.gz` | `dated-ext` | `app.log` | `2026-10-01.3` | date |
| `postgresql-2026-10-01_000000.log` | `dated-ext` | `postgresql.log` | `2026-10-01_000000` | date |
| `app.1.log.gz` | `numbered-ext` | `app.log` | `1` | number |
| `app-2.log` | `numbered-ext` | `app.log` | `2` | number |
| `syslog.123456` | none | | | |

A number of four digits from 1970 to 2100 where `N` goes
(`report.2023`, `app.2024.log`) is a year: such a file is a yearly part,
ordered by its year, and never the 2023rd rotation with 2,022 missing
parts before it.

### Grouping

1. Files are grouped by directory and chain name. Parts of one chain
   may match different templates (`syslog.1` and
   `syslog-20261001.gz`).
2. The file named exactly like the chain is its active part.
3. A chain needs two parts; the active file counts when it exists. One
   file alone is no chain.
4. One generation in several encodings (`app.log-2025121008`, its `.gz`
   and its `.zst`) is one part. rx reads the encoding that comes first
   in this order, by the bytes of each file, not by its name: plain,
   seekable zstd, zstd, gzip, bzip2, xz. The others are the part's
   `duplicates`; a search skips them with the reason `duplicate_part`.
   So `worker.log-2025121008` beside `worker.log-2025121008.gz` is one part,
   and no chain.
5. Left out of every chain: directories, hidden entries (unless
   `--hidden`), names that end in `.tmp`, and files that are not text by
   the rule every command applies (a NUL byte in the first 8 KiB of the
   text), so `wtmp` and `wtmp.1` are no chain. An empty file is text: a
   chain of empty files is a chain, and it is ready with 0 lines.
6. A file whose name makes it a part but which cannot be opened (its
   permissions, an I/O error) stays a part, named in `unreadable`; the
   chain is invalid until it can be read.
7. A chain may have at most 10,000 parts. A larger one is listed with
   `too_many_parts` and read no further.

### Missing parts

In a chain with numbered parts, every number from the lowest expected
one to the highest present must exist. The lowest expected number is 0
when a `.0` part exists, else 1. Each absent number is a **missing
part**, named as it would be without a compression suffix (`dpkg.log.3`,
since rx cannot know whether it was compressed). Missing parts are
named only when no more numbers are missing than numbered parts are
present (`x.1` beside `x.500` names none), at most 100 of them;
`missing_count` says how many are missing in all. Dated parts are never
missing: rotation skips quiet days by design. A missing part does not
make a chain invalid; it is shown where it would be.

## Order by time

A chain's **provisional order** comes from the names: by key, in the
template's direction (a higher number is older, a lower date is older),
then by modification time, the active part last. When one chain mixes
numbered and dated names, the kind whose newest file is older comes
first. The provisional order is only a guess: log4j numbers the other
way round, and a 10-digit key may be a date or an epoch.

The real order comes from the timestamps: the parts with lines are
sorted by their first timestamp, ties keeping the provisional order. An
empty part keeps its provisional place. Before the chain is ready, the
provisional order is used.

## Checks, states and `idx`

A chain is valid when:

| Check | Reason when it fails |
|---|---|
| Every part with lines has timestamps rx recognizes (see [Timestamps](timestamps.md)); parts may write different formats | `no_timestamps` |
| Sorted by time, no part's highest timestamp is later than the next part's first timestamp by more than `RX_CHAIN_OVERLAP_SECONDS` (default 60): this covers a program that writes to `.1` for a short time after a rotation | `overlap`, with the overlap in `overlap_ms` |
| The active file, when it has lines, comes last | `active_not_last` |
| Every part can be opened and read | `unreadable` |
| It has at most 10,000 parts | `too_many_parts` |

Empty parts add no lines and take no part in the checks. All times are
compared as UTC instants, each part read the way one file is read (see
[Times of a chain](#times-of-a-chain)).

To check these and to number the lines, rx needs each frozen part's line
count, first timestamp and highest timestamp. A frozen part's
[line index](line-indexes.md) gives all three without reading the part.
The active part needs no index: nothing is numbered after it, and its
first timestamp comes from a read of its head (a gzip, bzip2, xz or
plain zstd active file gives it only through its index).

| State | When | What works |
|---|---|---|
| `pending` | A frozen part with lines has no current line index, or a gzip, bzip2, xz or plain zstd active file has none | Each part can be read on its own; no global line numbers, no time queries |
| `ready` | Every frozen part is known and every check passed | Everything |
| `invalid` | A check failed; `reasons` say which, naming the parts | `rx logs show` prints it and exits 6; `rx logs samples` and `GET /v1/logs/samples` refuse it (exit 6, `422`); a search still searches its parts, as files of the chain without global numbers |

`rx logs show`, `rx logs time-range` and `rx logs samples` never leave
a chain pending: they read a frozen part without a stored index in full
and index it in memory, store nothing, and answer as a fully indexed
chain would. The HTTP routes never read a whole part inside a request:
a pending chain starts its [index task](#the-chains-index-task) and is
ready once the task ends. `rx logs trace` describes chains from stored
indexes only, like the routes (see [Searching](#searching)).

`idx` in a listing (`is_indexed` in `GET /v1/logs/chains`, the `IDX`
column of `rx logs list`) means that every frozen part has a current
line index of the file the listing found; an empty frozen part needs
none and counts as indexed. In a description each part's own
`is_indexed` says whether an index of it is stored, so an empty part
shows `false` there. The active part's index does not count: on a live
log it is almost never current.

## One numbering

`global_start` of a part is the global number of its first line: 1 plus
the lines of the parts before it. Line `L` of a part is global line
`global_start + L − 1`. A part's last line is its own line even when the
part does not end with a line break, so a part never joins its last line
to the next part's first.

Global numbers exist only when the chain is ready. Before that, every
global number is `-1` in JSON and `?` in human output, as everywhere in
rx: `-1` means "not computed", and a number rx fills in is never wrong.
A search match that its part's scan left unnumbered (a scan cut short
by `--max-results`) has `chain_line` `-1` too:
`rx samples PART --offsets=OFFSET` gives its line in the part, and
`global_start − 1` plus that line is its global line.

The active part may grow: its new lines take the numbers after its last
line and change no earlier number.

## Reading lines

[`rx logs samples`](../cli/logs.md#rx-logs-samples) and
[`GET /v1/logs/samples`](../api/endpoints/logs-samples.md) take a
position three ways:

| Given as | Example | When |
|---|---|---|
| A global line or range | `--lines=8001`, `--lines=100-200`, `--lines=-50` | Ready |
| A part and its local line | `--part=syslog.3.gz --lines=500` | Pending or ready |
| A time or time range | `--timestamps=2026-10-03T14:00..2026-10-03T15:00` | Ready |

Each position's lines come back as **pieces**, one per part its window
touches, each saying which part it is from, its first local and global
line, and the `rx samples PART --lines=A-B` command that gives exactly
those lines from the part. Context crosses part edges in a ready chain.
Over HTTP, `RX_SAMPLES_MAX_LINES` and `RX_SAMPLES_MAX_BYTES` bound the
whole answer, summed over every position and piece.

A chain is pending over HTTP only: `rx logs samples` indexes in memory
what it needs, and stores no index (a part without a current index is
read from its text). Before the chain is ready, a part is read alone: a
position addressed to a part is answered at once, its context stops at
the part's edges, and the timestamp read-back (below) stops at its
first line. A position by global line or by time waits for the chain's
index task.

### Times of a chain

Each part's timestamps are read the way
[`rx time-range`](../cli/time-range.md) reads one file: a part whose
timestamps carry no zone is read in `RX_LOG_TZ` (or `--file-tz=`), one
whose timestamps carry zones as instants, and a year-less timestamp
(`Oct  7 00:15:01`) takes its year from its own part's modification
time. Parts may write different formats and zones.

The line at a time `T` is the first line, in the chain's order, whose
own timestamp is at or after `T`, which is the answer a scan of the
whole chain gives:

- The part that holds it is the first whose highest timestamp reaches
  `T` (a part whose lines go back in time is still found), and the
  per-file search finds the line in it.
- `T` in a time gap between two parts gives the first line of the later
  part; `T` before the chain gives line 1; `T` after it gives no line
  (`-1`).
- The query is read once for the whole chain, as one file holding the
  chain would read it from its head: in the format of its first part
  with timestamps.
- A time of day without a date (`--timestamps=14:00`) takes its date
  from the chain's first and last timestamps, which must fall on one
  day, as one file's must; on a chain that spans several days it is
  refused (exit 2, `400`).
- Under `--file-tz=`, a part whose lines write several zone offsets (a
  change of daylight saving time inside it) has only an upper bound of
  its highest time (`max_is_bound`): it is searched, and when it holds
  no line at or after `T` the search moves on to the next part.

A line without a timestamp of its own (a traceback, a continued
message) carries the timestamp of the nearest earlier line that has
one, within `RX_TIMESTAMP_LOOKBACK_KB`. In a ready chain that look back
continues into the parts before, so the lines at the start of a part
that continue a record the part before began get the timestamp the
concatenation gives them. It is answered from the earlier part's index.
Two rare cases answer `null` where the concatenation has a value, and
never a wrong value: under `file_tz` an earlier part whose zone offset
changes more often than its index records, and over HTTP a line at
exactly the look back's distance when deciding it would decode more of
the earlier part than is left of `RX_SAMPLES_MAX_BYTES`.

## Searching

[`rx logs trace`](../cli/logs.md#rx-logs-trace) and
[`GET /v1/logs/trace`](../api/endpoints/logs-trace.md) search as
`rx trace` does, with each path read as a directory (its files grouped
into chains), a chain's handle, or a file (a part's own path is a
file). Each chain's parts are searched in the chain's order, each as one
file by the trace engine with its own line numbers, byte offsets,
context window, trace cache entry and line index. A match in a part
also gets `chain_line`, its global line. Context never crosses a part's
edge. Another encoding of a part is skipped as `duplicate_part`, and
what the paths reach more than once is searched once.

A search never reads a part to describe its chain: each chain is
described from its parts' stored line indexes and the head and tail of
its active file, and no index build starts. A capped search
(`--max-results=`) therefore reads what `rx trace` reads on the same
files, however large the parts. A chain with a frozen part not indexed
yet is `pending`: its parts are searched in the provisional order and
its matches have `chain_line` `-1`; when it has a match,
`rx logs trace` names `rx logs index` on stderr for it. An invalid
chain is still searched, its matches without a chain line.

## The chain's index task

Under `rx serve` a chain's parts are indexed by one background task
per chain (operation `chain_index`, followed at
[`GET /v1/tasks/{id}`](../api/endpoints/tasks.md)):

- `GET /v1/logs/chain` and `GET /v1/logs/samples` start it for a pending
  chain, for the parts the chain waits for: the frozen parts with lines
  and no current index, and a gzip, bzip2, xz or plain zstd active file
  without one.
  `POST /v1/logs/index` and `rx logs index` build every part, the active
  file too, whose index speeds a jump inside it until it grows.
- Every part is indexed whatever its size: `RX_LARGE_FILE_MB` does not
  apply.
- One task per chain: it is known by its directory's device and inode
  and its name, so two paths to one directory share it. A second start
  joins the running task.
- Each part build is a task of its own (operation `index`), the same
  build a samples lookup in that part starts. The part builds share
  `RX_MAX_INDEX_BUILDS` with every other background build: the chain
  task submits at most that many parts at a time and the next as one
  ends, and the part builds of all chains together take at most half of
  the queue of 256 builds, so a lookup in another file always finds
  room, however many chains are pending.
- At most 128 chains' index tasks run or wait at once, as many as that
  half of the queue has places. Past that a pending chain gets no task:
  its description names none (`index_build` null), and
  `POST /v1/logs/index` and a samples request that needs the chain
  ready answer `503`, until one of those tasks has ended. A chain
  waiting for room waits in line, and each build's end lets one go on.
- Finished part builds are kept apart from other tasks, at most 256 of
  them, the oldest dropped first. A chain of thousands of parts cycles
  through that share and never drops a task another client follows; a
  part build it started may be gone from `GET /v1/tasks/{id}` once 256
  later part builds have ended.
- The first part whose build fails fails the task. How the task ended
  is kept with the chain for `RX_TASK_TTL_MINUTES`, so a description
  names the failure, and describing the chain again does not restart
  it; `POST /v1/logs/index` does.

## Live logs

rx keeps no state about a chain. It finds the chain again from the disk
on every request, and memory caches only make answers faster: a
description of a chain whose frozen parts are all indexed is kept for
the next request (at most 64 chains and 40,000 parts), and a change to
any frozen part's stat or index file makes it unused.

- **Growth.** The active file may grow by appending. Its new lines
  take the numbers after it; no earlier number changes, and the
  fingerprint stays the same.
- **Rotation.** A rename, a compression, a deletion, a new part or a
  write to a frozen part changes the **fingerprint**: the first 16 hex
  digits of a SHA-256 over the parts in the provisional order, each
  frozen part by its name, device, inode, size and modification time,
  the active part by its name, device and inode only. A client keeps the
  fingerprint of the description it read and sends it back
  (`--fingerprint=`, `fingerprint=`). When the files changed since, the
  answer is `409` with the current description, or exit 7. A part
  renamed or replaced between the listing and its read is refused the
  same way. The client finds its place again by time: a rotation never
  changes a line's timestamp.
- **Re-indexing.** A line index is found by its file's path. A numbered
  rotation renames every part (`.1` becomes `.2`, …), so every frozen
  part is indexed again and the chain is pending meanwhile; a dated
  rotation adds one part to index. An index of a path that no longer
  exists stays in the cache.
- **Truncation.** A truncation of the active file in place, with no new
  part, does not change the fingerprint; the active file is then read
  as a truncated single file is. With `copytruncate` a new `.1`
  appears, so the fingerprint changes.

## Limits

| Limit | Value |
|---|---|
| Parts of one chain | 10,000; more is `too_many_parts` |
| Missing parts named | 100; `missing_count` counts them all |
| Overlap between neighboring parts | `RX_CHAIN_OVERLAP_SECONDS`, 60 by default, 0 to 86400 |
| Time gaps | Reported in a ready chain of four parts with lines or more, where the time from a part's highest timestamp to the next part's first is more than 1.5 times the median distance between neighboring first timestamps; a gap does not make a chain invalid |
| Kept descriptions | 64 chains and 40,000 parts in all |
| Time values in one samples request | 1,000 |
| Lines and bytes of one samples answer | `RX_SAMPLES_MAX_LINES`, `RX_SAMPLES_MAX_BYTES` (HTTP), over the whole answer |

Every part goes through the same checks as a file named on its own: the
search roots, the hidden-entry rule and the pin that refuses a file
replaced between its check and its read. `--part=` and `part=` take a
bare name that must be one of the chain's parts.

## Not supported

- Byte offsets in a chain (`--offsets=`): a search gives each part's
  own offsets, and `rx samples PART --offsets=` reads them.
- Parts in another directory (logrotate's `olddir`), and the
  daemontools and s6 log directories.
- Names with two-digit years or `MM-dd-yyyy` dates.
- Templates of one's own: the table is fixed.
- Finding a part's index by device and inode: a numbered rotation
  indexes every frozen part again.
- Removing the indexes of parts that no longer exist.
- Context that crosses a part's edge in a search.
- Anomaly analysis of a chain, and `rx compress` of a chain.
- `--fingerprint=` on `rx logs trace`: one search can find several
  chains, and each one's entry in `chains` carries its fingerprint.
- `chain_line` in webhook payloads: a chain search fires the webhooks
  of a trace, with each part's own line.
- An active file that starts with a block of NUL bytes (a writer that
  does not append) is not text, so it drops out of its chain.
- rx-python (the `rx-tool` package) has no log chains.

## See also

- [`rx logs`](../cli/logs.md) — the commands
- [`GET /v1/logs/chains`](../api/endpoints/logs-chains.md),
  [`GET /v1/logs/chain`](../api/endpoints/logs-chain.md),
  [`POST /v1/logs/index`](../api/endpoints/logs-index.md),
  [`GET /v1/logs/samples`](../api/endpoints/logs-samples.md),
  [`GET /v1/logs/trace`](../api/endpoints/logs-trace.md) — the routes
- [Line indexes](line-indexes.md) — what a part's index holds
- [Timestamps](timestamps.md) — the formats rx recognizes
- [Configuration](../configuration.md#log-chains) — `RX_CHAIN_OVERLAP_SECONDS`
