# `rx logs`

Read a rotated log — `syslog`, `syslog.1`, `syslog.2.gz`, … — as one
**log chain**: the files of one rotated log in one directory, found by
their names. [Log chains](../concepts/log-chains.md) explains which
names form a chain, how its parts are ordered and numbered, and how a
chain follows a live log.

## Synopsis

```text
rx logs list [DIR...] [--json]
rx logs show CHAIN... [--json] [--file-tz=ZONE] [--fingerprint=FP]
rx logs time-range CHAIN... [--json] [--file-tz=ZONE]
rx logs index CHAIN... [--json] [--force]
rx logs samples CHAIN (--lines=SPEC [--part=NAME] | --timestamps=T...) [--context=N] [--json] [--file-tz=ZONE] [--fingerprint=FP]
rx logs trace PATTERN [CHAIN|DIR|FILE ...] [the flags of rx trace]
```

`CHAIN` is a chain's **handle**: its directory joined with its name,
`/var/log/syslog`, as `rx logs list --json` gives it in `path`.

Each subcommand calls the same function as its `/v1/logs` route, and
its `--json` output is that route's body.

## `rx logs list`

The chains of each directory (default: the current one), from the
files' names alone. The answer is the one
[`GET /v1/logs/chains`](../api/endpoints/logs-chains.md) gives; the
[name templates](../concepts/log-chains.md#name-templates) say which
names form a chain.

```bash
rx logs list /var/log
```

```text
/var/log: 5 chains
NAME      PARTS  SIZE       IDX  MISSING
auth.log  4      43.04 KB   -    -
dmesg     2      165.00 B   -    -
dpkg.log  4      1.99 KB    -    dpkg.log.3
kern.log  3      4.03 KB    -    -
syslog    5      554.77 KB  -    -
```

On that host `wtmp` and `wtmp.1` are no chain (they are binary),
`lastlog` and `notes.txt` have no rotated parts, and `dpkg.log.3` is
missing between `dpkg.log.4.gz` and `dpkg.log.2.gz`. `dmesg` and
`dmesg.0` form a chain by their names; [`rx logs show`](#rx-logs-show)
finds it invalid, as its lines carry no timestamp.

One block per directory: a line with the directory and how many
chains it holds, then one row per chain, sorted by name:

| Column | Meaning |
|---|---|
| `NAME` | The chain's name: its active file's name |
| `PARTS` | How many files the chain has, the active file included; one generation in several encodings counts once; `(N unreadable)` after it when N parts cannot be opened. `>10000` for a chain of more parts than are read as one text |
| `SIZE` | The parts' total size on disk |
| `IDX` | `idx` when every part but the active file has a current line index (an empty part needs none), else `-` |
| `MISSING` | The names of the absent numbered parts: the first three, then how many more are missing in all (`missing_count`), or `-` |

A chain's handle is its directory joined with its name,
`/var/log/syslog`, whether that file exists or not; `--json` gives it
as each chain's `path`.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | One JSON object (`{path, chains}`) for one directory, an array of objects (in the order of the directories) for several |

The [global flags](index.md#global-flags) apply: `--search-root`
refuses a directory outside the roots, and `--hidden` lets hidden
files be parts.

```bash
rx logs list /var/log /srv/app/logs --json
```

### Exit codes

| Code | When |
|---:|---|
| 0 | Every directory was listed, with or without chains |
| 2 | A path is a file, not a directory |
| 3 | A directory does not exist |
| 4 | A directory is outside `--search-root`, reaches a hidden entry, or cannot be read |

With several directories, each one that cannot be listed is reported on
stderr and the others are still listed; the exit code is the failures'
code when they all share one, else 1.

## `rx logs show`

The description of each chain: its parts in time order, their line
counts and times, the checks that make the chain one text, its state and
its fingerprint. The answer is the one
[`GET /v1/logs/chain`](../api/endpoints/logs-chain.md) gives; that page
explains each field.

```bash
rx logs show /var/log/syslog
```

```text
/var/log/syslog: ready, 5 parts, 14405 lines, fingerprint ed4c86af991bdd8b, times in UTC
#  NAME         COMPRESSION  LINES  GLOBAL LINES  FIRST TIME       HIGHEST TIME     IDX
1  syslog.4.gz  gzip         3120   1-3120        Sep 29 00:00:05  Sep 29 23:23:38  -
2  syslog.3.gz  gzip         2988   3121-6108     Sep 30 00:00:05  Sep 30 23:14:01  -
3  syslog.2.gz  gzip         3305   6109-9413     Oct  1 00:00:05  Oct  1 23:51:49  -
4  syslog.1     -            3012   9414-12425    Oct  2 00:00:05  Oct  2 23:25:13  -
5  syslog       -            1980   12426-14405   Oct  3 00:00:05  Oct  3 16:29:35  -
```

A first line with the handle, the state (`ready`, `pending` or
`invalid`), the number of parts and of lines, the fingerprint and the
zone the times are shown in (`--file-tz`, else `RX_LOG_TZ`, `UTC` by
default). A chain of more than 10,000 parts lists none, and says
`too many parts (N)` with the number its files' names give. For an
invalid chain a `reason CODE: …` line per failed check follows. Then
one row per part in the chain's order:

| Column | Meaning |
|---|---|
| `#` | The part's place in the chain |
| `NAME` | The part's file name |
| `COMPRESSION` | `gzip`, `bz2`, `xz` or `zstd`, or `-` for a plain file |
| `LINES` | The part's line count, or `?` when not known |
| `GLOBAL LINES` | The chain's line numbers the part holds (`110-246`); `-` for an empty part, `?` before the chain is ready |
| `FIRST TIME` | The part's first timestamp, or `?` |
| `HIGHEST TIME` | The part's highest timestamp, `<=` before an upper bound (see `max_is_bound`), or `?` |
| `IDX` | `idx` when a current line index of the part is stored, else `-` |

Then a `gap:` line per stretch of time no part covers and a `missing:`
line naming absent numbered parts.

The times are written in the layout the parts write their timestamps
(`Sep 29 00:00:00`, `2025-12-10 07:00:30`, `2025-12-10 07:00:04.574`),
as [`rx time-range`](time-range.md) writes one file's, when every part
with lines writes them one way; when the parts write them in several
layouts, they are written to the millisecond
(`2026-09-29 00:00:00.000`).

`rx logs show` never waits for background work: a part without a line
index is read in full and indexed in memory, so the answer is the one a
fully indexed chain gives (with `IDX` `-`).
[`rx logs index`](#rx-logs-index) stores the parts' indexes.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | One JSON object (the `GET /v1/logs/chain` body) for one chain, an array for several |
| `--file-tz` | — | Read every part's timestamps as wall clock in this zone (`UTC`, an IANA name or `±HH:MM`), as [`rx time-range`](time-range.md) reads one file; another value exits 2 |
| `--fingerprint` | — | The fingerprint of an earlier description: exit 7 when the chain's files changed since. 16 hex digits, with one `CHAIN` only; otherwise exit 2 |

### Exit codes

| Code | When |
|---:|---|
| 0 | Every chain was described and is valid |
| 2 | A handle ends in no name, or `--fingerprint=` is malformed or given with several chains |
| 3 | A handle names no chain (fewer than two parts), or its directory does not exist |
| 4 | A handle's directory is outside `--search-root`, hidden or unreadable |
| 6 | A chain is invalid (printed first) |
| 7 | The chain's fingerprint differs from `--fingerprint=` (printed first), or its files kept changing while they were read |

With several chains each one that cannot be described is reported on
stderr and the others are still described; the exit code is the
failures' code when they all share one, else 1.

## `rx logs time-range`

The first and the last timestamp of each chain, in the layout of
[`rx time-range`](time-range.md): the handle, the timestamp format of its
first part with timestamps, the first and last time (written as
`rx logs show` writes them), the zone they are shown in, and the
chain's state. The times are known once the chain is ready; `?` before.

```bash
rx logs time-range /var/log/syslog
```

```text
/var/log/syslog  syslog  Sep 29 00:00:05 .. Oct  3 16:29:35  UTC  ready
```

`--json` prints `{path, name, state, format, first_ms, last_ms,
display_zone, cli_command}` per chain (one object for one chain, an array
for several); `first_ms` and `last_ms` are UTC instants in milliseconds.
`--file-tz` works as for `rx logs show`. The exit codes are those of
`rx logs show`, without 7 from a fingerprint.

## `rx logs samples`

Lines of a chain by its global line numbers, by a part and its own
line numbers, or by time: what [`rx samples`](samples.md) gives for one
file, for the parts of a rotated log read as one text. The answer is
the one [`GET /v1/logs/samples`](../api/endpoints/logs-samples.md)
gives; that page says how each kind of position is answered.

```bash
rx logs samples /var/log/syslog --lines=12000 --context=5
rx logs samples /var/log/syslog --part=syslog.3.gz --lines=500
rx logs samples /var/log/syslog --timestamps=2026-10-03T14:00..2026-10-03T15:00
```

Line 3121 is the first line of `syslog.3.gz`, so its context starts in
`syslog.4.gz`:

```bash
rx logs samples /var/log/syslog --lines=3121 --context=1
```

```text
Chain: /var/log/syslog  ready  5 parts  fingerprint ed4c86af991bdd8b
Times: Sep 29 00:00:05 .. Oct  3 16:29:35  UTC
Context: 1 before, 1 after

=== /var/log/syslog:3121 ===
-- syslog.4.gz --
3120  syslog.4.gz:3120  Sep 29 23:23:38 web01 systemd[1]: Started session-3280.scope - Session 3280 of User deploy.
-- syslog.3.gz --
3121  syslog.3.gz:1  Sep 30 00:00:05 web01 systemd[1]: logrotate.service: Deactivated successfully.
3122  syslog.3.gz:2  Sep 30 00:00:33 web01 nginx[7838]: connect() failed (111: Connection refused) while connecting to upstream, request 7840
```

A head with the handle, the state, the number of parts and the
fingerprint, the chain's first and last time (in the layout its parts
write them, as for `rx logs show`) with their zone, and the context;
then one block per position, in the order of the lines (a time by the
line it found), headed `=== HANDLE:KEY ===` (`=== HANDLE PART:KEY ===`
for a part's own numbers, `=== HANDLE:LINE @ TIME ===` for a time).
Each line is printed with its global number (`?` before the chain is
ready), the part it comes from and its number in that part; a
`-- NAME --` line marks where a part's lines start. A position the
chain has no line for is named on stderr, as `rx samples` names one.

`rx logs samples` never waits for background work: it describes the
chain as `rx logs show` does (a part without a line index is indexed in
memory), so the chain is ready unless it is invalid, and reads each part
with its stored index when one is current, else from its text. It
stores nothing; [`rx logs index`](#rx-logs-index) stores the indexes.
`--json` prints the `GET /v1/logs/samples` body, each piece with the
`rx samples` command that gives exactly its lines.

### Flags

| Flag | Default | Description |
|---|---|---|
| `-l`, `--lines` | — | Line numbers and ranges, comma-separated or repeated: the chain's global numbers, or with `--part` the part's own. `-N` counts back from the end |
| `--part` | — | The bare name of a part (`syslog.3.gz`): `--lines` then numbers that part as `rx samples` numbers it on its own. Context still crosses its edges |
| `-t`, `--timestamps` | — | A time or time range (`T`, `T1..T2`, `..T2`, `T1..`), as `rx samples` takes it; repeat the flag for several |
| `-c`, `--context` | `3` | Lines of context before and after each single line or time |
| `-B`, `--before`, `-A`, `--after` | — | Override `--context` on one side |
| `--file-tz` | — | Read every part's timestamps as wall clock in this zone, as for `rx logs show` |
| `--fingerprint` | — | The fingerprint of an earlier description: exit 7 when the chain's files changed since |
| `--json` | `false` | Print the `GET /v1/logs/samples` body |

Exactly one of `--lines` and `--timestamps` is given.

### Exit codes

| Code | When |
|---:|---|
| 0 | The lines were given (a position the chain has no line for is named on stderr) |
| 2 | Neither or both of `--lines` and `--timestamps`, `--part` without `--lines` or naming no part of the chain, a bad lines spec, time, zone or fingerprint, a handle that ends in no name |
| 3 | The handle names no chain, or its directory does not exist |
| 4 | The handle's directory is outside `--search-root`, hidden or unreadable |
| 6 | The chain is invalid |
| 7 | The chain's fingerprint differs from `--fingerprint=`, or a part changed while it was read |

## `rx logs index`

Builds and stores the line index of every part of each chain, the
active file too, in the foreground, one part after the other in the
chain's order. A part's index is what [`rx index`](line-index.md) stores
for the file, and each part is reported as `rx index` reports a file,
counted as parts (`Indexed 4 parts`, `No parts indexed.`); then the
chain is printed as `rx logs show` prints it, every part `idx`.

```bash
rx logs index /var/log/kern.log
```

```text
Indexed 3 parts in 0.0s
  /var/log/kern.log.2.gz: 12 lines, 211.00 B
  /var/log/kern.log.1: 31 lines, 3.12 KB
  /var/log/kern.log: 7 lines, 721.00 B
/var/log/kern.log: ready, 3 parts, 50 lines, fingerprint 32162d49a340e7f6, times in UTC
#  NAME           COMPRESSION  LINES  GLOBAL LINES  FIRST TIME       HIGHEST TIME     IDX
1  kern.log.2.gz  gzip         12     1-12          Sep 26 03:00:00  Sep 26 10:31:00  idx
2  kern.log.1     -            31     13-43         Sep 30 01:00:00  Sep 30 21:30:00  idx
3  kern.log       -            7      44-50         Oct  3 02:00:00  Oct  3 06:06:00  idx
```

Every part is indexed whatever its size: `RX_LARGE_FILE_MB`, below
which `rx index` skips a file, does not apply, because the chain needs
each part's line count and times. A part whose index is current is kept
(counted in the first line, not listed under it) unless `--force`. The
indexes are those `GET /v1/logs/chain` and the other routes read, so a
chain indexed this way is `ready` over HTTP at once.

[`POST /v1/logs/index`](../api/endpoints/logs-index.md) does the same
in the background, under `rx serve`.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Per chain `{path, indexed, skipped, skip_reasons, errors, total_time, chain}`: the members of [`rx index --json`](line-index.md) for its parts, and the chain's description after them as `rx logs show --json` prints it (`null` when it could not be described again). One object for one chain, an array for several |
| `--force` | `false` | Build the index of every part again, current ones too |

### Exit codes

The codes of [`rx index`](line-index.md): 0 when every part was indexed
(the chain may be invalid: it is printed with its reasons), 1 when a
part could not be indexed, 4 when a part could not be read, after the
other parts are indexed; and those of `rx logs show` for a handle that
cannot be described: 2 for a handle that ends in no name, 3 when it
names no chain, 4 outside `--search-root`, 7 when the chain's files
kept changing while they were read. With several chains the exit code
is the failures' code when they all share one, else 1.

## `rx logs trace`

A search of rotated logs: what [`rx trace`](trace.md) does, with the
parts of each chain searched in the chain's order and each of their
matches given its line in the chain. The answer is the one
[`GET /v1/logs/trace`](../api/endpoints/logs-trace.md) gives; that page
says how each path is read and in which order the parts are searched.

```bash
rx logs trace 'Accepted publickey' /var/log
rx logs trace refused /var/log/syslog --max-results=100
rx logs trace -e timeout -e refused /var/log/syslog /var/log/notes.txt
```

Once a chain's parts are indexed, each match gives its line in the
chain:

```bash
rx logs index /var/log/syslog
rx logs trace 'Out of memory' /var/log/syslog
```

```text
Request ID: 01a11a78-…
Path: /var/log/syslog
Pattern: Out of memory
Time: 0.036s
Matches: 3

Matches (chain:line (part:line), or file:line):
  /var/log/syslog:4620 (syslog.3.gz:1500): Sep 30 11:39:37 web01 kernel: Out of memory: Killed process 30211 (java) total-vm:8123456kB
  /var/log/syslog:11613 (syslog.1:2200): Oct  2 17:06:17 web01 kernel: Out of memory: Killed process 41877 (java) total-vm:8123456kB
  /var/log/syslog:13325 (syslog:900): Oct  3 07:29:35 web01 kernel: Out of memory: Killed process 52004 (java) total-vm:8123456kB
```

Each `PATH` is a directory (its files are grouped into chains, as
`rx logs list` groups them), a chain's handle, or a file (a part's own
path is a file); without one, the current directory. The header is the
one `rx trace` prints. A match in a part reads
`CHAIN:LINE (PART:LINE): TEXT`: its line in the chain first (`?` when
not known: a pending or invalid chain, or a line a capped scan left
unnumbered),
then the part's name and its own line; a match in a file of its own
reads `FILE:LINE: TEXT`. With several patterns each row names its
pattern in square brackets before the text. Context (`--samples`,
`--context=`) is shown per file, as `rx trace` shows it, and never
crosses a part's edge. Another encoding of a part (`syslog.3` beside
`syslog.3.gz`) is listed under `Files skipped:` as `duplicate_part`,
also when it is named on its own beside its chain, and is never
searched, so each of its lines is printed once, in the chain. An
invalid chain is still searched and is named on stderr. What the paths
reach more than once (a path given twice, a directory and a chain or a
part in it, a link to a directory) is searched once, each match printed
once and in its chain.

`rx logs trace` never waits for background work, and never reads a
part to describe its chain: each chain is described from its parts'
stored line indexes and the head and tail of its active file, as
`GET /v1/logs/trace` describes it. A search capped with
`--max-results=` therefore reads what `rx trace` reads on the same
files, however large the parts. A chain with a frozen part that has no
line index yet is `pending`: its parts are searched in the order of
their names, each of its matches prints `?` as its line in the chain
(`chain_line` `-1` in `--json`), and stderr says, for each pending
chain with a match:

```text
Hint: the log chain /var/log/syslog is pending (a part has no line index), so its matches have no line in the chain: run rx logs index -- /var/log/syslog for chain line numbers.
```

The `--` ends the options of `rx logs index`, so the command pastes as
it is, also for a handle that starts with a dash.
[`rx logs index`](#rx-logs-index) stores the indexes; the next search
gives every match its line in the chain, and the parts in time order.
(`rx logs show` and `rx logs samples` do read a part without an index,
as they need its line count to answer at all.) `--json` prints the
`GET /v1/logs/trace` body, with `cli_command` null as for `rx trace`.

### Flags

Every flag of [`rx trace`](trace.md#flags), with the same meaning:
`-e`/`--regexp`, `--path`, `--max-results`, `--samples`, `--context`,
`-B`/`--before`, `-A`/`--after`, the matching flags (`-i`, `-w`, `-x`,
`-F`, `-P`), `--json`, `--color`, `--no-cache`, `--no-index` (for the
search; describing a chain reads its indexes all the same),
`--no-recursive`, `--request-id` and the `--hook-on-*` webhooks, whose
payloads are those of `rx trace`. It does not read standard input.

### Exit codes

| Code | When |
|---:|---|
| 0 | The search ran (an invalid chain is named on stderr, and a pending chain with a match gets its hint there) |
| 2 | A pattern that does not compile, `-` as a path, and the usage errors of `rx trace` |
| 3 | A path that is no directory, no chain's handle and no file |
| 4 | A path outside `--search-root` or hidden, or a file named on its own that cannot be read |
| 7 | A part of a chain was renamed or replaced while its chain was described (a rotation ran meanwhile); run it again |

## Exit codes of the chain commands

Beyond the codes every command uses ([exit codes](index.md#exit-codes)),
the commands that read one chain use two more:

| Code | When |
|---:|---|
| 6 | The chain is invalid: its parts cannot be read as one text |
| 7 | The chain's files changed since the fingerprint the command was given |

## See also

- [`GET /v1/logs/chains`](../api/endpoints/logs-chains.md) — `rx logs list` over HTTP
- [`GET /v1/logs/chain`](../api/endpoints/logs-chain.md) — `rx logs show` over HTTP
- [`POST /v1/logs/index`](../api/endpoints/logs-index.md) — `rx logs index` as a background task
- [`GET /v1/logs/samples`](../api/endpoints/logs-samples.md) — `rx logs samples` over HTTP
- [`GET /v1/logs/trace`](../api/endpoints/logs-trace.md) — `rx logs trace` over HTTP
- [`rx time-range`](time-range.md) — the time range of single files
- [Log chains](../concepts/log-chains.md) — what a chain is, its states, its numbering and live logs
