# CLI reference

`rx` exposes ten subcommands. `trace` is the default — any invocation whose
first argument isn't a subcommand name gets `trace` prepended automatically.

```text
rx <subcommand> [flags] [args]

Subcommands:
  trace      Search files/directories for regex patterns (default)
  index      Build, inspect, or delete line-offset indexes
  scan       Scan files for signatures and keep their findings in the cache
  samples    Retrieve context lines by byte offset or line number
  time-range Show the format and first and last timestamp of files
  logs       Read rotated logs (syslog, syslog.1, syslog.2.gz, …) as one log chain
  compress   Encode files as seekable zstd
  signatures List, show and check the signatures rx can find in files
  serve      Start the HTTP API server
  completion Generate the autocompletion script for bash, zsh, fish or powershell
```

## Default command dispatch

The following pairs are equivalent:

```bash
rx "error" /var/log/app.log
rx trace "error" /var/log/app.log
```

If the first positional argument matches a known subcommand name
(`trace`, `index`, `scan`, `samples`, `time-range`, `logs`, `compress`, `signatures`,
`serve`, `help`, `completion`), it's routed to that subcommand. Otherwise, `trace` is
assumed. `version` is not a pattern either: `rx version` exits 2 with
`Unknown command "version"`; use `--version`.

## Global flags

These are persistent flags on the root command: every subcommand accepts
them, and they may be written before or after the subcommand name.

| Flag | Default | Behaviour |
|------|---------|-----------|
| `--hidden` | `RX_HIDDEN` or `false` | Include entries whose name starts with a dot, the way `rg --hidden` does |
| `--search-root` | `RX_SEARCH_ROOTS` or no sandbox | Restrict file access to this directory; repeatable |
| `--help`, `-h` | — | Print help for the current command and exit |
| `--version` | — | Print the version string (`rx version <tag>`: a release prints its tag, such as `rx version v0.8.0`; `just build` prints what `git describe --tags --dirty --always` gives; a plain `go build` prints `rx version dev`) and exit |

`--search-root` confines every path a command touches, including the
destination `rx compress` writes. A path outside all roots exits 4; a
root that does not exist is a usage error and exits 2. `rx serve`
declares its own `--search-root`, whose default is the roots in
`RX_SEARCH_ROOTS`, else the current directory, never "no sandbox", and
exports the roots it resolved in
`RX_SEARCH_ROOTS` so a child process inherits them. See
[Security](../concepts/security.md).

Every subcommand except `rx completion` has its flags documented on its
own page; `rx completion` is covered in
[Installation](../installation.md#shell-completion).

## Exit codes

All subcommands share the same exit-code scheme:

| Code | Meaning |
|-----:|---------|
| 0 | Success |
| 1 | Generic error (subprocess failure, IO error, rebuild failed) |
| 2 | Usage error (bad flag combination, missing required argument, regex that does not compile, a path that is a named pipe, a socket or a device) |
| 3 | File not found |
| 4 | Access denied: a path outside `--search-root`, or a file the user named that the process may not read (`permission denied: <path>`, the same in `trace`, `samples`, `index` and `scan`) |
| 5 | Interrupted by signal (SIGINT or SIGTERM) |
| 6 | The log chain an [`rx logs`](logs.md) command reads is invalid: its parts cannot be read as one text |
| 7 | The log chain's files changed since the fingerprint an `rx logs` command was given (`--fingerprint=`) |

A scan that completes and finds nothing exits **0**, not 1: `rx` reports
whether the search ran, not whether it matched. This differs from `grep`
deliberately — check the result count in `--json` output if you need to
branch on "found something".

A pattern ripgrep cannot compile is fatal for the whole request. `rx`
prints ripgrep's own message, which names the offending position, and
exits 2. It does not list the file under `skipped_files` and report
success; `skipped_files` is for files that could not be read.

`rx-python` uses the same table for 0 to 5; it has no log chains, so
no 6 or 7.

## Subcommands

<div class="grid cards" markdown>

- **[`rx trace`](trace.md)**  
  Regex search with parallel chunking, cache-aware, supports webhooks.

- **[`rx index`](line-index.md)** (file: `line-index.md`)  
  Build and inspect line-offset indexes for fast line-number lookups.

- **[`rx scan`](scan.md)**  
  Scan files and directories for signatures and keep the findings in
  the cache.

- **[`rx samples`](samples.md)**  
  Read content around byte offsets or line numbers, with context.

- **[`rx time-range`](time-range.md)**  
  The timestamp format and the first and last timestamp of each file.

- **[`rx logs`](logs.md)**  
  Rotated logs read as one log chain: `rx logs list` finds them,
  `rx logs show` describes one, `rx logs time-range` gives its times,
  `rx logs samples` gives its lines by global line, by part and line or
  by time, `rx logs trace` searches chains with each one's parts in its
  order, `rx logs index` stores the line index of each of its parts.

- **[`rx compress`](compress.md)**  
  Encode files as seekable zstd for random-access decompression.

- **[`rx signatures`](signatures.md)**  
  List the built-in and user signatures, show one, check signature
  files, print a template.

- **[`rx serve`](serve.md)**  
  Start the HTTP API + Swagger UI + Prometheus metrics.

- **[`rx completion`](../installation.md#shell-completion)**  
  Generate the autocompletion script for `bash`, `zsh`, `fish` or
  `powershell`.

</div>

## Reading the flag tables

Each subcommand page has a flags table with these columns:

- **Flag** — all long and short forms
- **Type** — `bool`, `int`, `string`, `string[]` (repeatable)
- **Default** — what `rx` uses when the flag is omitted
- **Description** — one-sentence purpose

Flag aliases (e.g. `--path` and `--file`) list both spellings together.

## Stdin handling

Most commands operate on filesystem paths. `rx trace` supports two stdin
shapes:

- `rx trace "pattern"` with no path **and stdin is not a terminal** (a
  pipe or a redirected file): that input is searched.
- `-` as an explicit path argument: the same, for input redirected or
  piped to stdin.

```bash
my-producer | rx trace "error"
rx trace "error" - < /var/log/app.log
```

The input is spooled to a temporary file and searched, and the output
names that file. The file is removed when the search ends, and no
trace-cache entry is written for it. See
[pattern and path resolution](trace.md#pattern-and-path-resolution).

## Color output

By default, color is emitted when stdout is a TTY. Override with:

- `--color=always` — force ANSI codes regardless of output destination
- `--color=never` — never emit color codes
- `--no-color` — alias for `--color=never`
- `NO_COLOR=1` environment variable — respected at auto-detect time
- `RX_NO_COLOR=1` environment variable — the same, respected at
  auto-detect time

`rx trace`, `rx logs trace` and `rx samples` emit color in non-JSON
mode.

## JSON output

Every subcommand accepts `--json`, except `rx serve`,
`rx signatures check`, `rx signatures template`, `rx completion`, and
`rx logs` without a subcommand, which prints help.
The resulting stream is one JSON
object per invocation (not a stream of objects). Pipe through `jq` for
post-processing:

```bash
rx index /var/log/app-2026-03.log --json | jq '.indexed[].line_count'
```

The JSON shapes are documented on each subcommand's page and are the
same ones the HTTP API returns, except for the commands that build:
`rx index --json` prints `{indexed, skipped, skip_reasons, errors,
total_time}`, `rx logs index --json` adds `path` and `chain` to those
members, `rx compress --json` prints `{files}`, and `rx scan --json`
prints `{signatures, files, skipped, errors, totals, total_time}`, while
`POST /v1/index`, `POST /v1/logs/index`, `POST /v1/compress` and
`POST /v1/findings` answer with a background task. See the
[HTTP API reference](../api/index.md) for the full schemas.

## See also

- [Configuration](../configuration.md) — env vars and global knobs
- [HTTP API](../api/index.md) — the same feature surface over HTTP
- [Troubleshooting](../troubleshooting.md) — common CLI issues
