# rx — Regex Tracer

High-performance CLI and REST API for regex search, indexing, sampling,
signature scans and seekable compression of very large log files. Wraps
[ripgrep](https://github.com/BurntSushi/ripgrep) and adds parallel
chunked search, native decompression (gzip, xz, bz2, zstd), systemd
journal files read as text, frame-level parallel zstd, line-offset
indexes, result caching, and an HTTP API
with a web viewer that `rx serve` downloads from the GitHub releases of
`wlame/rx-ui` — all in a single static binary (about 18 MB, no runtime
deps beyond `rg`).

Built for multi-GB log files; designed to scale to ~100 GB.

## Intended use

rx is built for **internal use on a trusted network. It is not intended to be
exposed to the internet.**

`rx serve` has **no identity system**. Unless `RX_API_TOKEN` is set, anyone
who can reach the port can read any file under `--search-root`, and
`--search-root` defaults to the current directory. There is no TLS and no
multi-tenancy.

This is a deliberate scope decision. Building the perimeter is the operator's
job, and any of these is enough:

- bind to loopback, which is the default, and keep it there;
- reach it over a VPN, or an SSH tunnel —
  `ssh -L 7777:127.0.0.1:7777 loghost`;
- or put an authenticating reverse proxy in front of it.

Inside that perimeter rx handles the hygiene that is cheap to get right: a path
outside `--search-root` is refused rather than served, webhook targets are
checked against internal address ranges before *and* at the moment of
connecting, and the viewer bundle is checked against the `.sha256` sidecar
its release publishes. Set `RX_API_TOKEN` and every `/v1` request must send
it as `Authorization: Bearer <token>`: one shared value, sent in clear text
over plain HTTP, so no substitute for the perimeter.

Identity providers, RBAC, session management and certificate handling are out of
scope — they belong to the perimeter, not to rx.

## Hidden files and directories
Entries whose name starts with a dot are skipped by default, the way
`ripgrep` skips them. This matters most for `rx serve`, whose
`--search-root` defaults to the current directory: started from a home
directory it would otherwise serve `~/.ssh` and `~/.aws`.

```bash
rx "token" ~/                 # skips ~/.ssh, ~/.aws, ~/.bashrc
rx --hidden "token" ~/        # includes them
RX_HIDDEN=true rx "token" ~/  # same, from the environment
```

`--hidden` is a global flag, so it works on every subcommand and may
appear before or after the subcommand name. Wherever a search root is
set — always in `rx serve`, and on the other commands with
`--search-root` or `RX_SEARCH_ROOTS` — the rule is enforced in the path
sandbox as well as in directory listings, so a hidden file is not
merely absent from `rx serve`'s tree: asking for it by name returns 403
(exit 4 on the CLI) with a message naming the flag. Without a search
root the CLI has no sandbox: a dot-file you name is read, and only
directory walks skip dot-entries.

A hidden component of a `--search-root` is exempt: pointing rx at
`~/.local/share/logs` is a deliberate choice and needs no flag.

## Requirements

- Go **1.25+** (to build)
- `ripgrep` (`rg`) **13+** on `$PATH` (runtime)

## Install

### Download a release

Every `vX.Y.Z` tag publishes static binaries with sha256 sidecars for
linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64. No Go toolchain is
needed to run them; `ripgrep` 13+ must be on `PATH`. The commands below
fetch the latest release; for another one, replace `latest/download` with
`download/vX.Y.Z`.

```bash
base="https://github.com/wlame/rx-go/releases/latest/download"
asset="rx-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"

curl -LO "$base/$asset"
curl -LO "$base/$asset.sha256"
sha256sum -c "$asset.sha256"     # on macOS: shasum -a 256 -c "$asset.sha256"
install -m 0755 "$asset" /usr/local/bin/rx
rx --version                     # prints the release tag, e.g. "rx version v0.8.0"
```

### Build from source

```bash
just build            # → dist/rx, version stamped from git describe
just install          # → ${GOBIN:-$HOME/go/bin}/rx, plus shell completions
just build-all        # cross-compile every release target with checksums
```

The version comes from the git tag; there is nothing to override by hand.
`just --list` shows the rest.

## Usage

`trace` is the default subcommand — bare `rx <pattern> <path>` routes to it.

```bash
rx "error"  /var/log/app.log                     # search
rx "error"  /var/log/app.log.zst                 # compressed (auto-detected)
rx "error"  /var/log/app.log --json --samples    # JSON + context lines
rx samples  /var/log/app.log --lines=450000 --context=3 # context by line
rx index    /var/log/app.log                     # build line-offset index
rx trace    --signatures /var/log/app.log        # tracebacks, secrets and more, found by signatures
rx scan     --recursive /var/log/app             # cache the findings of every file under a directory
rx compress /var/log/app.log                     # produce seekable .zst
rx logs show /var/log/syslog                     # syslog, syslog.1, syslog.2.gz, … as one log
rx serve    --port=7777 --search-root=/var/log   # HTTP API + SPA
```

`--signatures` scans the files for named signatures: the built-in ones
(tracebacks, crashes, secrets, SQL, long lines, runs of blank lines and
more) and your own, from YAML files. `rx signatures` lists them. See
[`docs/concepts/signatures.md`](docs/concepts/signatures.md) and the
catalogue in [`docs/reference/signatures.md`](docs/reference/signatures.md).

### Subcommands

| Command    | Purpose                                                          |
|------------|------------------------------------------------------------------|
| `trace`    | Parallel regex search with optional caching and sample context   |
| `samples`  | Extract context windows by byte offset or line number            |
| `index`    | Build (or inspect) a cached line-offset index                    |
| `scan`     | Scan files or a directory for signatures and cache the findings  |
| `time-range` | Show the format and the first and last timestamp of each file |
| `logs`     | Read a rotated log (`syslog`, `syslog.1`, `syslog.2.gz`, …) as one log chain: list, describe, index, sample by line or time, search |
| `compress` | Produce a seekable zstd file for frame-parallel decompression    |
| `signatures` | List, show and check the signatures `--signatures` scans for; print a template file |
| `serve`    | REST API (`chi` + `huma` v2) with Swagger UI, metrics, SPA       |

### HTTP API (via `rx serve`)

| Method | Path             | Purpose                             |
|--------|------------------|-------------------------------------|
| GET    | `/health`        | Server + `rg` availability          |
| GET    | `/v1/trace`      | Search files for patterns           |
| GET    | `/v1/samples`    | Context around offsets / lines      |
| GET    | `/v1/time-range` | First and last timestamp of a file  |
| GET    | `/v1/index`      | Retrieve cached file index          |
| POST   | `/v1/index`      | Start background indexing of a file or a directory |
| POST   | `/v1/compress`   | Start background compression task   |
| GET    | `/v1/tasks/{task_id}` | Background task status         |
| DELETE | `/v1/tasks/{task_id}` | Cancel a signature scan or a task over a directory |
| GET    | `/v1/tree`       | Browse directory tree               |
| GET, POST | `/v1/logs/*`  | Log chains: list, describe, index, samples, search |
| GET    | `/v1/signatures` | Signature catalogue                 |
| GET, POST | `/v1/findings` | A file's cached findings; scan a file or a directory for signatures |
| GET    | `/metrics`       | Prometheus metrics                  |
| GET    | `/docs`          | Swagger UI (from `/openapi.json`)   |

`--search-root` sandboxes all path-accepting endpoints. Pass it multiple
times to allow several roots.

## Configuration

Configured via environment variables. The most common ones:

| Variable              | Default         | Description                           |
|-----------------------|-----------------|---------------------------------------|
| `RX_CACHE_DIR`        | unset           | Cache base, with `rx` appended (`RX_CACHE_DIR=/tmp` gives `/tmp/rx`; unset: `$XDG_CACHE_HOME/rx`, else `~/.cache/rx`): indexes, cached trace results, findings and the viewer |
| `RX_SEARCH_ROOTS`     | `serve`: the current directory; other commands: no sandbox | Path sandbox, `:`-separated; `--search-root` wins |
| `RX_MAX_SUBPROCESSES` | `20`            | Most chunks per file, and most `rg` processes at once unless `RX_WORKERS` is set |
| `RX_LARGE_FILE_MB`    | `50`            | Size from which a plain file is indexed and its trace answers cached |
| `RX_NO_INDEX`         | `false`         | `samples`, `time-range` and the findings routes neither build nor read a line index; `rx trace` ignores it |
| `RX_HIDDEN`           | `false`         | Include dot-prefixed files and dirs   |
| `RX_API_TOKEN`        | unset           | When set, every `/v1` request must send `Authorization: Bearer <token>` |
| `RX_LOG_LEVEL`        | `INFO`          | The level every command logs at (`DEBUG`, `INFO`, `WARN`, `ERROR`); `GET /health` reports it |

There is no variable that turns the trace cache off; pass `--no-cache` to
`rx trace`.

Full list and tuning notes: [`docs/configuration.md`](docs/configuration.md).

## Development

`just` is the entrypoint; `just --list` shows every recipe.

```bash
just test         # unit tests; arguments pass through to go test
just test-race    # the tests under the race detector
just lint         # golangci-lint
just fmt          # gofmt every Go file
just ci           # exactly what GitHub CI runs: fmt-check vet lint tidy-check
                  # scaffolding-check spec-check test-race docs-build
just cover        # tests with the coverage floor
just clean        # remove build and coverage artifacts
```

## Layout

- `cmd/rx/` — CLI entry point.
- `internal/clicommand/` — one file per subcommand.
- `internal/trace/` — parallel regex scan engine.
- `internal/webapi/` — HTTP layer (chi + huma v2).
- `internal/seekable/` · `internal/compression/` — zstd + format detection.
- `internal/index/` · `internal/samples/` — line-offset index and resolver.
- `internal/logchain/` — log chains: the files of a rotated log read as one.
- `internal/signatures/` · `internal/sigscan/` · `internal/findings/` — the
  signature catalogue and its YAML loader, the scan engine, and the
  findings cache.
- `pkg/rxtypes/` — public wire types (shared JSON schemas).
- `docs/` — architecture, CLI, API, concepts and the built-in signatures
  (49 pages).

## Documentation

- [Quickstart](docs/quickstart.md) — 10-minute walkthrough
- [Installation](docs/installation.md) — binaries, ripgrep, completion
- [Configuration](docs/configuration.md) — full env var reference
- [Performance](docs/performance.md) — tuning notes and benchmarks
- [CLI reference](docs/cli/) · [HTTP API reference](docs/api/)
- [Project history](docs/MIGRATION.md)

## License

See `LICENSE`.
