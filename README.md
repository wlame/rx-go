# rx — Regex Tracer

High-performance CLI and REST API for regex search, indexing, sampling,
and seekable compression of very large log files. Wraps
[ripgrep](https://github.com/BurntSushi/ripgrep) and adds parallel
chunked search, native decompression (gzip, xz, bz2, zstd), frame-level
parallel zstd, line-offset indexes, result caching, and an HTTP API
with an embedded SPA — all in a single static binary (~13 MB, no runtime
deps beyond `rg`).

Built for multi-GB log files; designed to scale to ~100 GB.

## Requirements

- Go **1.25+** (to build)
- `ripgrep` (`rg`) **13+** on `$PATH` (runtime)

## Install

### Download a release

Every `vX.Y.Z` tag publishes static binaries with sha256 sidecars for
linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64. No Go toolchain is
needed to run them; `ripgrep` 13+ must be on `PATH`.

```bash
version=v0.1.0
base="https://github.com/wlame/rx-go/releases/download/$version"
asset="rx-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"

curl -LO "$base/$asset"
curl -LO "$base/$asset.sha256"
sha256sum -c "$asset.sha256"
install -m 0755 "$asset" /usr/local/bin/rx
rx --version
```

### Build from source

```bash
just build            # → dist/rx, version stamped from git describe
just install          # → $GOBIN/rx, plus shell completions
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
rx samples  /var/log/app.log --lines=450000 -C 3 # context by line
rx index    /var/log/app.log                     # build line-offset index
rx index    /var/log/app.log --analyze           # + run anomaly detectors
rx compress /var/log/app.log                     # produce seekable .zst
rx serve    --port=7777 --search-root=/var/log   # HTTP API + SPA
```

`--analyze` runs the built-in anomaly detectors (tracebacks, secrets,
long lines, multi-line JSON blobs, coredumps, repeated runs) alongside
the index build. See [`docs/concepts/analyzers.md`](docs/concepts/analyzers.md)
for the full catalog and wire contract.

### Subcommands

| Command    | Purpose                                                          |
|------------|------------------------------------------------------------------|
| `trace`    | Parallel regex search with optional caching and sample context   |
| `samples`  | Extract context windows by byte offset or line number            |
| `index`    | Build (or inspect) a cached line-offset index                    |
| `compress` | Produce a seekable zstd file for frame-parallel decompression    |
| `serve`    | REST API (`chi` + `huma` v2) with Swagger UI, metrics, SPA       |

### HTTP API (via `rx serve`)

| Method | Path             | Purpose                             |
|--------|------------------|-------------------------------------|
| GET    | `/health`        | Server + `rg` availability          |
| GET    | `/v1/trace`      | Search files for patterns           |
| GET    | `/v1/samples`    | Context around offsets / lines      |
| GET    | `/v1/index`      | Retrieve cached file index          |
| POST   | `/v1/index`      | Start background indexing task      |
| GET    | `/v1/tasks/{id}` | Background task status              |
| GET    | `/v1/tree`       | Browse directory tree               |
| GET    | `/metrics`       | Prometheus metrics                  |
| GET    | `/docs`          | Swagger UI (from `/openapi.json`)   |

`--search-root` sandboxes all path-accepting endpoints. Pass it multiple
times to allow several roots.

## Configuration

Configured via environment variables. The most common ones:

| Variable              | Default         | Description                           |
|-----------------------|-----------------|---------------------------------------|
| `RX_CACHE_DIR`        | `~/.cache/rx`   | Indexes + cached trace results        |
| `RX_SEARCH_ROOT(S)`   | (none)          | Path sandbox for `serve`              |
| `RX_MAX_SUBPROCESSES` | `20`            | Concurrent `rg` workers               |
| `RX_LARGE_FILE_MB`    | `50`            | Threshold for large-file optimizations |
| `RX_NO_CACHE`         | `0`             | Disable trace caching                 |
| `RX_LOG_LEVEL`        | `info`          | `debug` \| `info` \| `warn` \| `error` |

Full list and tuning notes: [`docs/configuration.md`](docs/configuration.md).

## Development

```bash
make test     # go test -race -cover ./...
make lint     # golangci-lint run
make fmt      # gofmt -s -w .
make ci       # fmt-check + vet + lint + test
make cover    # coverage.out + coverage.html
make clean    # remove dist/ and coverage artifacts
```

## Layout

- `cmd/rx/` — CLI entry point.
- `internal/clicommand/` — one file per subcommand.
- `internal/trace/` — parallel regex scan engine.
- `internal/webapi/` — HTTP layer (chi + huma v2).
- `internal/seekable/` · `internal/compression/` — zstd + format detection.
- `internal/index/` · `internal/samples/` — line-offset index and resolver.
- `pkg/rxtypes/` — public wire types (shared JSON schemas).
- `docs/` — architecture, CLI, API, concepts (30+ pages).

## Documentation

- [Quickstart](docs/quickstart.md) — 10-minute walkthrough
- [Installation](docs/installation.md) — binaries, ripgrep, completion
- [Configuration](docs/configuration.md) — full env var reference
- [Performance](docs/performance.md) — tuning notes and benchmarks
- [CLI reference](docs/cli/) · [HTTP API reference](docs/api/)
- [Project history](docs/MIGRATION.md)

## License

See `LICENSE`.
