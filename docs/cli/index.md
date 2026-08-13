# CLI reference

`rx` exposes five subcommands. `trace` is the default — any invocation whose
first argument isn't a subcommand name gets `trace` prepended automatically.

```text
rx <subcommand> [flags] [args]

Subcommands:
  trace      Search files/directories for regex patterns (default)
  index      Build, inspect, or delete line-offset indexes
  samples    Retrieve context lines by byte offset or line number
  compress   Encode files as seekable zstd
  serve      Start the HTTP API server
```

## Default command dispatch

The following pairs are equivalent:

```bash
rx "error" /var/log/app.log
rx trace "error" /var/log/app.log
```

If the first positional argument matches a known subcommand name
(`trace`, `index`, `samples`, `compress`, `serve`, `help`, `completion`,
`version`), it's routed to that subcommand. Otherwise, `trace` is
assumed.

## Global flags

`rx` itself has no global flags other than cobra's built-ins:

| Flag | Behaviour |
|------|-----------|
| `--help`, `-h` | Print help for the current command and exit |
| `--version` | Print the version string (`rx version 2.2.1-go`) and exit |

Every subcommand has its own set of flags documented on its page.

## Exit codes

All subcommands share the same exit-code scheme:

| Code | Meaning |
|-----:|---------|
| 0 | Success |
| 1 | Generic error (subprocess failure, IO error, rebuild failed) |
| 2 | Usage error (bad flag combination, missing required argument, regex that does not compile) |
| 3 | File not found |
| 4 | Access denied (path outside `--search-root`) |
| 5 | Interrupted by signal (SIGINT or SIGTERM) |

A scan that completes and finds nothing exits **0**, not 1: `rx` reports
whether the search ran, not whether it matched. This differs from `grep`
deliberately — check the result count in `--json` output if you need to
branch on "found something".

A pattern ripgrep cannot compile is fatal for the whole request. `rx`
prints ripgrep's own message, which names the offending position, and
exits 2. It does not list the file under `skipped_files` and report
success; `skipped_files` is for files that could not be read.

`rx-python` uses the same table.

## Subcommands

<div class="grid cards" markdown>

- **[`rx trace`](trace.md)**  
  Regex search with parallel chunking, cache-aware, supports webhooks.

- **[`rx index`](line-index.md)** (file: `line-index.md`)  
  Build and inspect line-offset indexes for fast line-number lookups.

- **[`rx samples`](samples.md)**  
  Read content around byte offsets or line numbers, with context.

- **[`rx compress`](compress.md)**  
  Encode files as seekable zstd for random-access decompression.

- **[`rx serve`](serve.md)**  
  Start the HTTP API + Swagger UI + Prometheus metrics.

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
shapes, with one caveat:

- `rx trace "pattern"` with no path **and stdin is a pipe**: reserved for
  stdin input. Not yet implemented in this release; the CLI returns an
  error.
- `-` as an explicit path argument: same — reserved, currently errors.

Until stdin is supported, pipe your data into a temporary file first:

```bash
my-producer | tee /tmp/producer.log | rx "error" /tmp/producer.log
```

## Color output

By default, color is emitted when stdout is a TTY. Override with:

- `--color=always` — force ANSI codes regardless of output destination
- `--color=never` — never emit color codes
- `--no-color` — alias for `--color=never`
- `NO_COLOR=1` environment variable — respected at auto-detect time

Only `rx samples` currently emits color in non-JSON mode.

## JSON output

Every subcommand accepts `--json`. The resulting stream is one JSON
object per invocation (not a stream of objects). Pipe through `jq` for
post-processing:

```bash
rx index /var/log/app-2026-03.log --json | jq '.indexed[].line_count'
```

The JSON shapes are documented on each subcommand's page and are the
same ones the HTTP API returns. See the [HTTP API reference](../api/index.md)
for the full schemas.

## See also

- [Configuration](../configuration.md) — env vars and global knobs
- [HTTP API](../api/index.md) — the same feature surface over HTTP
- [Troubleshooting](../troubleshooting.md) — common CLI issues
