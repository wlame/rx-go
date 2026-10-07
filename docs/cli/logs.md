# `rx logs`

Read a rotated log — `syslog`, `syslog.1`, `syslog.2.gz`, … — as one
**log chain**: the files of one rotated log in one directory, found by
their names.

## Synopsis

```text
rx logs list [DIR...] [--json]
rx logs show CHAIN... [--json] [--file-tz=ZONE] [--fingerprint=FP]
rx logs time-range CHAIN... [--json] [--file-tz=ZONE]
```

`CHAIN` is a chain's **handle**: its directory joined with its name,
`/var/log/syslog`, as `rx logs list --json` gives it in `path`.

Each subcommand calls the same function as its `/v1/logs` route, and
its `--json` output is that route's body.

## `rx logs list`

The chains of each directory (default: the current one), from the
files' names alone. The answer is the one
[`GET /v1/logs/chains`](../api/endpoints/logs-chains.md) gives; that
page says which names form a chain.

```bash
rx logs list /var/log
```

```text
/var/log: 7 chains
NAME               PARTS  SIZE       IDX  MISSING
alternatives.log   7      5.81 KB    -    -
auth.log           8      70.55 KB   -    -
dmesg              4      114.28 KB  -    -
dpkg.log           12     26.71 KB   -    -
edge-agent.log  8      0.00 B     -    -
kern.log           4      1.84 KB    -    -
syslog             8      23.85 KB   -    -
```

On that host `wtmp` and `wtmp.1` are no chain (they are binary),
`lastlog` and `fontconfig.log` have no rotated parts, and the eight
empty `edge-agent.log` files are one chain.

One block per directory: a line with the directory and how many
chains it holds, then one row per chain, sorted by name:

| Column | Meaning |
|---|---|
| `NAME` | The chain's name: its active file's name |
| `PARTS` | How many files the chain has, the active file included; one generation in several encodings counts once |
| `SIZE` | The parts' total size on disk |
| `IDX` | `idx` when every part but the active file has a current line index, else `-` |
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
/var/log/syslog: ready, 8 parts, 1269 lines, fingerprint 3173803a68e459cd, times in UTC
#  NAME                           COMPRESSION  LINES  GLOBAL LINES  FIRST TIME               HIGHEST TIME             IDX
1  syslog-20260930-1790726400.gz  gzip         109    1-109         2026-09-29 00:00:00.000  2026-09-30 00:00:00.000  -
2  syslog-20261001-1790812801.gz  gzip         137    110-246       2026-09-30 00:00:01.000  2026-10-01 00:00:01.000  -
...
8  syslog                         -            64     1206-1269     2026-10-06 00:00:00.000  2026-10-06 22:06:44.000  -
```

A first line with the handle, the state (`ready`, `pending` or
`invalid`), the number of parts and of lines, the fingerprint and the
zone the times are shown in (`--file-tz`, else `RX_LOG_TZ`, `UTC` by
default). For an invalid chain a `reason CODE: …` line per failed check
follows. Then one row per part in the chain's order:

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

`rx logs show` never waits for background work: a part without a line
index is read in full and indexed in memory, so the answer is the one a
fully indexed chain gives (with `IDX` `-`). [`rx index`](line-index.md)
on the parts stores their indexes.

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
first part with timestamps, the first and last time (to the
millisecond), the zone they are shown in, and the chain's state. The
times are known once the chain is ready; `?` before.

```bash
rx logs time-range /var/log/syslog
```

```text
/var/log/syslog  iso  2026-09-29 00:00:00.000 .. 2026-10-06 22:06:44.000  UTC  ready
```

`--json` prints `{path, name, state, format, first_ms, last_ms,
display_zone, cli_command}` per chain (one object for one chain, an array
for several); `first_ms` and `last_ms` are UTC instants in milliseconds.
`--file-tz` works as for `rx logs show`. The exit codes are those of
`rx logs show`, without 7 from a fingerprint.

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
- [`rx time-range`](time-range.md) — the time range of single files
