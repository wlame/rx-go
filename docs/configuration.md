# Configuration

Reference for every environment variable and global flag `rx`
recognizes. Environment variables are read at every call (not
memoized), so `t.Setenv(...)` in tests and `env X=Y rx ...` in shell
both work without restarting anything.

## Variable precedence

Configuration is resolved in this order, later items overriding
earlier ones:

1. **Compiled defaults** — the constants in source
2. **Environment variables** — read lazily on each call site
3. **Command-line flags** — supplied to the invocation

For the cache directory:

1. `$RX_CACHE_DIR/rx`
2. `$XDG_CACHE_HOME/rx`
3. `$HOME/.cache/rx`

## Cache directory

| Variable | Default | Description |
|---|---|---|
| `RX_CACHE_DIR` | — | Base directory for `rx` caches. Appended with `rx`, so `RX_CACHE_DIR=/tmp` yields `/tmp/rx`. A relative value is taken against the directory `rx` starts in. |
| `XDG_CACHE_HOME` | — | Standard XDG variable; used when `RX_CACHE_DIR` is unset. A relative value is taken against the directory `rx` starts in. |

The resolved cache base holds:

- `indexes/` — line-offset indexes
- `trace_cache/` — trace result caches
- `frontend/` — `rx-viewer` SPA

See [concepts/caching](concepts/caching.md).

## Chunking and workers

| Variable | Default | Description |
|---|---|---|
| `RX_WORKERS` | smaller of `NumCPU` and `RX_MAX_SUBPROCESSES` | Number of ripgrep processes a trace runs at once. Takes precedence when set, from 1 to 256; a larger value is used as 256. |
| `RX_MAX_SUBPROCESSES` | `20` | Most chunks one file is split into, and the worker count when `RX_WORKERS` is unset. From 1 to 256. |
| `RX_MIN_CHUNK_SIZE_MB` | `20` | Smallest chunk the parallel scanner carves: a file is split into `size / RX_MIN_CHUNK_SIZE_MB` chunks, so a file below twice this size is scanned as one. |
| `RX_HIDDEN` | `false` | Include files and directories whose name starts with a dot. Off by default, as in `ripgrep`. The `--hidden` flag overrides it. |

See [concepts/chunking](concepts/chunking.md).

## Long lines

| Variable | Default | Description |
|---|---|---|
| `RX_MAX_LINE_TEXT_BYTES` | `1048576` (1 MiB) | Most bytes of one line's text a trace answer holds, for a match and for a context line. A longer line is cut at the start of a character and marked `line_text_truncated`; its offset and line number stay exact. |
| `RX_MAX_SUBMATCHES_PER_LINE` | `10000` | Most submatches a trace answer lists for one line; the rest are left out and the match is marked `submatches_truncated`. |

Together they bound the memory one line costs while ripgrep's output is
read, whatever the line holds: without them a 100 MB line on which the
pattern matches every character costs about 5 GB, and one request could
exhaust a server. Both are bounded too (see
[integer settings](#integer-settings)): at most 256 MiB of text and
1,000,000 submatches per line. A trace answer that cuts a line is not written to the trace
cache. See [long lines](api/endpoints/trace.md#long-lines).

## Indexing thresholds

| Variable | Default | Description |
|---|---|---|
| `RX_LARGE_FILE_MB` | `50` | The large-file size in MB. `rx index` skips a smaller plain file unless `--analyze` is set or `--threshold=N` is supplied (`--threshold=0` indexes every file; omitting the flag uses this variable). `rx samples` and `GET /v1/samples` build an index before a lookup in a plain file of this size or more. A trace answer for a plain file of this size or more is cached. The index checkpoint step is a fiftieth of it (1 MB by default). |
| `RX_ANALYZE_WINDOW_LINES` | `128` | Sliding-window size, in lines, of the anomaly detectors that `--analyze` runs. `--analyze-window-lines` and the `analyze_window_lines` request field win over it, and their value is clamped to 1–2048. The variable follows the [integer settings](#integer-settings) rule: from 1 to 2048. See [analyzers](concepts/analyzers.md). |

## Cache control

The trace cache is turned off per invocation; there is no variable for
it:

- `--no-cache` on `rx trace`, `no_cache=true` on `GET /v1/trace` —
  don't consult or write the trace cache
- `--no-index` on `rx trace`, `no_index=true` on `GET /v1/trace` —
  neither read nor write a line index; lines a capped scan left
  unnumbered are counted from the start of the file instead (same
  answer)

| Variable | Default | Description |
|---|---|---|
| `RX_NO_INDEX` | `false` | `rx samples` and `GET /v1/samples` neither build nor read a line index; the lookup streams the file (same answer). The CLI flag is `rx samples --no-index`; there is no query parameter. |

`NO_COLOR` / `RX_NO_COLOR` control color output:

| Variable | Default | Description |
|---|---|---|
| `NO_COLOR` | unset | When set to any value, disables ANSI color output (standard convention) |
| `RX_NO_COLOR` | unset | When set to any value, disables ANSI color output. Takes precedence over `NO_COLOR`. |

Both are overridden by explicit `--color=always`.

## Timestamps

`rx samples --timestamps` and `GET /v1/samples?timestamps=` find lines
by time ([timestamps](concepts/timestamps.md)). Two variables say which
zone a time without one is in:

| Variable | Default | Description |
|---|---|---|
| `RX_LOG_TZ` | `UTC` | The zone a file's timestamps were written in when they carry none. A query with a zone is turned into the file's wall-clock time with it, and `time_format.assumed_zone` reports it. |
| `RX_QUERY_TZ` | unset | The zone a time query is read in when it carries none. Unset reads such a query in the file's own frame: as the file's wall clock, or at the offset of the first timestamp of a file whose timestamps carry zones, so a time copied from a line finds that line. `local` reads it in the zone of the rx process. |

## Search-root sandbox

| Variable | Default | Description |
|---|---|---|
| `RX_SEARCH_ROOTS` | — | Path-separator-delimited (`:`) list of directories. Read by every subcommand as the environment form of `--search-root`, which overrides it. With neither set, `rx serve` uses the current directory and the other subcommands have no sandbox. Exported by `rx serve` from the roots it resolved, so a child process inherits the same sandbox. |

See [concepts/security](concepts/security.md).

## Frontend cache

| Variable | Default | Description |
|---|---|---|
| `RX_FRONTEND_VERSION` | unset | Pin the `rx-viewer` SPA to a specific release tag (e.g. `v0.2.0`). When unset, `rx serve` fetches the latest release. |
| `RX_FRONTEND_URL` | unset | Direct URL to a `dist.tar.gz` of the SPA. When set, `rx serve` downloads from this URL on every start. |
| `RX_FRONTEND_PATH` | — | Alternate directory for the SPA extraction. Defaults to `{cache}/frontend`. Tilde expansion supported. |

## Webhooks

| Variable | Default | Description |
|---|---|---|
| `RX_HOOK_ON_FILE_URL` | — | URL fired per file after scan |
| `RX_HOOK_ON_MATCH_URL` | — | URL fired per match (requires `max_results`) |
| `RX_HOOK_ON_COMPLETE_URL` | — | URL fired once per trace |
| `RX_DISABLE_CUSTOM_HOOKS` | `false` | When truthy, per-request hook URLs (query params / flags) are silently ignored; only env-configured URLs fire. |
| `RX_ALLOW_INTERNAL_HOOKS` | `false` | When truthy, bypass all SSRF checks. Loopback / private / link-local URLs become valid. |
| `RX_HOOK_STRICT_IP_ONLY` | `false` | When truthy, reject every hostname; only IP-literal URLs are allowed. Strongest defense against DNS rebinding. |

See [concepts/security](concepts/security.md) and
[api/webhooks](api/webhooks.md).

## Tasks

| Variable | Default | Description |
|---|---|---|
| `RX_TASK_TTL_MINUTES` | `60` | How long finished (completed/failed) tasks stay in memory before the sweeper removes them, from 1 minute to one week (10080). At most 256 tasks are kept; past that, the oldest finished ones go first. |
| `RX_SAMPLES_WAIT_SECONDS` | `5` | How long a `GET /v1/samples` request that sends `Prefer: respond-async` waits for the line index it needs to be built (a background `index` task, one per file) before it answers `202` with the task instead of the lines; a request without the header waits for the build. From 0 to 3600; `0` answers `202` at once whenever a build is needed. `rx samples` ignores it and waits for the build. See [`GET /v1/samples`](api/endpoints/samples.md#response-202-accepted). |

## Logging

| Variable | Default | Description |
|---|---|---|
| `RX_LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` (alias `WARNING`), or `ERROR`, in any case. Sets slog level on `rx serve` startup. |

## HTTP server

The listening address and the search roots are flags on `rx serve`; see
[`rx serve`](cli/serve.md). One setting is an environment variable
because it is a secret, and secrets stay out of flags and process
listings:

| Variable | Default | Description |
|---|---|---|
| `RX_API_TOKEN` | unset | When set, every `/v1` request must send `Authorization: Bearer <token>`; others get `401`. `/health`, `/metrics` and the docs stay open, and `/health` reports the value as `<redacted>`. See [security](concepts/security.md#opt-in-api-token). |

## Variables rx does not read

`/health` echoes every `RX_*`, `UVICORN_*` and `PROMETHEUS_*` variable
under `environment`, as it is set, whether rx reads it or not. Several
names rx-python reads do nothing in rx: `RX_MAX_FILES`,
`RX_MAX_LINE_SIZE_KB`, `RX_DEBUG`, `RX_DEBUG_DIR`, `NEWLINE_SYMBOL` and
`RX_NO_CACHE`. Setting them changes nothing, and `/health` does not list
them under `constants`.

## Global flags

Two flags are declared on the root command, so every subcommand accepts
them, before or after the subcommand name:

| Flag | Scope | Effect |
|---|---|---|
| `--hidden` | all | Include entries whose name starts with a dot; default `RX_HIDDEN` or `false` |
| `--search-root=DIR` | all | Restrict file access to `DIR` (repeatable); default `RX_SEARCH_ROOTS`, else no sandbox — except `rx serve`, which defaults to the current directory |
| `--help`, `-h` | all | Print help for the current command |
| `--version` | root | Print `rx version <version>` |

Per-subcommand flags are documented on each CLI page:

- [`rx trace`](cli/trace.md)
- [`rx index`](cli/line-index.md)
- [`rx samples`](cli/samples.md)
- [`rx compress`](cli/compress.md)
- [`rx serve`](cli/serve.md)

## Inspecting effective configuration

With a running `rx serve`:

```bash
# Every RX_* env var visible to the server; secrets show as <redacted>.
curl -s http://127.0.0.1:7777/health | jq '.environment'

# Current effective constants.
curl -s http://127.0.0.1:7777/health | jq '.constants'

# Effective hook configuration.
curl -s http://127.0.0.1:7777/health | jq '.hooks'

# Configured search roots.
curl -s http://127.0.0.1:7777/health | jq '.search_roots'
```

## Boolean parsing

The boolean variables (`RX_HIDDEN`, `RX_NO_INDEX`,
`RX_ALLOW_INTERNAL_HOOKS`, `RX_HOOK_STRICT_IP_ONLY`,
`RX_DISABLE_CUSTOM_HOOKS`) recognize these truthy values:

```text
true, yes, 1, on
```

`RX_HIDDEN` and `RX_NO_INDEX` also recognize these falsy values, and
fall back to their default for anything else:

```text
false, no, 0, off
```

The three hook variables are on only for a truthy value; anything else
is off. Matching is case-insensitive.

`NO_COLOR` and `RX_NO_COLOR` are different: any non-empty value turns
colour off.

## Integer settings

Every integer variable accepts a whole decimal number in a fixed
range:

| Variable | Default | Minimum | Maximum | Unit |
|---|---|---|---|---|
| `RX_WORKERS` | unset | 1 | 256 | ripgrep processes |
| `RX_MAX_SUBPROCESSES` | 20 | 1 | 256 | chunks per file, processes |
| `RX_MIN_CHUNK_SIZE_MB` | 20 | 1 | 1048576 | MB (up to 1 TiB) |
| `RX_LARGE_FILE_MB` | 50 | 1 | 1048576 | MB (up to 1 TiB) |
| `RX_MAX_LINE_TEXT_BYTES` | 1048576 | 1 | 268435456 | bytes (1 MiB to 256 MiB) |
| `RX_MAX_SUBMATCHES_PER_LINE` | 10000 | 1 | 1000000 | submatches |
| `RX_ANALYZE_WINDOW_LINES` | 128 | 1 | 2048 | lines |
| `RX_TASK_TTL_MINUTES` | 60 | 1 | 10080 | minutes (up to one week) |
| `RX_SAMPLES_WAIT_SECONDS` | 5 | 0 | 3600 | seconds |

One rule applies to all of them:

- Unset or empty: the default.
- Not a whole decimal number (`abc`, `1.5`, ` 2`, `2MB`), or below the
  minimum (`0` and negative numbers for every variable except
  `RX_SAMPLES_WAIT_SECONDS`, which takes `0`): the default.
- Above the maximum: the maximum.

A value that is not used as it is logs one `invalid_setting` warning
per process, naming the variable, its value, the accepted range and
the value used instead:

```text
WARN invalid_setting name=RX_LARGE_FILE_MB value=0 problem="below the minimum" accepted="1 to 1048576" using=50
```

The minimum of `RX_LARGE_FILE_MB` matters most: the index checkpoint
step is a fiftieth of it, and at `0` every line became a checkpoint and
every plain file counted as large. The maxima bound the work and
memory one setting can ask for: 256 ripgrep processes at once, and
sizes that stay far from overflowing when turned into bytes or a
duration.

## Zone settings

Each zone variable accepts `UTC`, an IANA zone name (`Europe/Berlin`,
`America/New_York`) or a fixed offset `±HH:MM` up to 18 hours
(`+05:30`); `RX_QUERY_TZ` also accepts `local`. The zone names are
built into the binary, so they work on a host without
`/usr/share/zoneinfo`.

| Variable | Default | Also accepts |
|---|---|---|
| `RX_LOG_TZ` | `UTC` | — |
| `RX_QUERY_TZ` | unset | `local` |

Any other value keeps the default and logs one `invalid_setting`
warning per process, as the integer settings do:

```text
WARN invalid_setting name=RX_LOG_TZ value=Europe/Berln problem="not a zone name" accepted="UTC, an IANA zone name or ±HH:MM" using=UTC
```

A daylight-saving change is not special: a zone-less time is read in
the zone's offset at that moment, and a wall time that a change skips
or repeats takes one of its readings, as Go's `time.Date` documents.

## Example environment for production

```bash
# Large-file-friendly thresholds.
export RX_LARGE_FILE_MB=100
export RX_MIN_CHUNK_SIZE_MB=50

# Dedicated cache directory.
export RX_CACHE_DIR=/var/cache/rx

# Extended task retention.
export RX_TASK_TTL_MINUTES=240

# Webhook destinations (internal).
export RX_HOOK_ON_COMPLETE_URL=https://internal.example.com/rx-log
export RX_ALLOW_INTERNAL_HOOKS=true

# Lock down per-request overrides.
export RX_DISABLE_CUSTOM_HOOKS=true

# INFO logging.
export RX_LOG_LEVEL=INFO

rx serve --host=0.0.0.0 --port=7777 \
    --search-root=/var/log \
    --search-root=/srv/data/logs
```

## See also

- [Performance](performance.md) — tuning advice grounded in benchmarks
- [Troubleshooting](troubleshooting.md) — what to check when config
  seems to be ignored
- [concepts/caching](concepts/caching.md) — cache paths
- [concepts/security](concepts/security.md) — security-related variables
