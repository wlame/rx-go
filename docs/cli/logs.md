# `rx logs`

Read a rotated log — `syslog`, `syslog.1`, `syslog.2.gz`, … — as one
**log chain**: the files of one rotated log in one directory, found by
their names.

## Synopsis

```text
rx logs list [DIR...] [--json]
```

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
| `MISSING` | The names of the absent numbered parts (the first three, then how many more), or `-` |

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

## Exit codes of the chain commands

Beyond the codes every command uses ([exit codes](index.md#exit-codes)),
the commands that read one chain use two more:

| Code | When |
|---:|---|
| 6 | The chain is invalid: its parts cannot be read as one text |
| 7 | The chain's files changed since the fingerprint the command was given |

## See also

- [`GET /v1/logs/chains`](../api/endpoints/logs-chains.md) — the same over HTTP
- [`rx time-range`](time-range.md) — the time range of single files
