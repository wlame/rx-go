# RX -- Fast Parallel Regex Search

RX is a high-performance file tracing and search tool built on top of [ripgrep](https://github.com/BurntSushi/ripgrep). It adds parallel chunked search, native compression support (gzip, xz, bz2, zstd), seekable zstd with frame-level parallelism, file indexing, result caching, and a REST API -- all in a single static binary.

## Build

Requires Go 1.25+ and `rg` (ripgrep) on PATH.

## Layout

- `cmd/rx/` — binary entry point.
- `internal/` — private packages.
- `pkg/rxtypes/` — public wire types (JSON schemas shared with clients).
- `docs/` — architecture, configuration, migration notes.
- `testdata/` — shared test fixtures.

```bash
make build          # Build for current platform
make test           # Run tests with race detector
make lint           # Run go vet
make cover          # Generate coverage report
make build-all      # Cross-compile for linux/darwin amd64/arm64
make release        # Build all targets into dist/ with checksums
```

The build produces a static binary (`CGO_ENABLED=0`) with version injected from git tags. Override with `VERSION=v1.2.3 make build`.

## Usage

### Trace (search)

```bash
# Search a file for a pattern (trace is the default command)
rx "error" /var/log/app.log

# Multiple patterns, case-insensitive via rg passthrough
rx "error|warning" /var/log/ -- -i

# Limit results and output JSON
rx "error" /var/log/app.log --max-results=100 --json

# Show context lines around each match
rx "error" /var/log/app.log --samples --context=5

# Search compressed files (auto-detected)
rx "error" /var/log/app.log.gz
rx "error" /var/log/app.log.zst
```

### Compress

Create seekable zstd files that enable parallel decompression during search:

```bash
rx compress /var/log/large.log
rx compress /var/log/large.log --frame-size=8388608 --level=6
```

### Index

Build line-offset indexes for faster repeated searches:

```bash
rx index /var/log/large.log
rx index /var/log/large.log --analyze    # Include line length statistics
```

### Samples

Extract context around specific byte offsets or line numbers:

```bash
rx samples /var/log/app.log -b 1234 -b 5678
rx samples /var/log/app.log -l 100 -B 5 -A 10
```

### Serve

Start the REST API server:

```bash
rx serve
rx serve --port=8080 --search-root=/var/log
```

Endpoints:

| Method | Path             | Description                        |
|--------|------------------|------------------------------------|
| GET    | /health          | Server health and configuration    |
| GET    | /v1/trace        | Search files for patterns          |
| GET    | /v1/samples      | Extract context around offsets     |
| GET    | /v1/index        | Retrieve cached file index         |
| POST   | /v1/index        | Start background indexing task     |
| GET    | /v1/tasks/{id}   | Check background task status       |
| GET    | /v1/complexity   | Regex complexity analysis (stub)   |
| GET    | /v1/detectors    | List anomaly detectors (stub)      |
| GET    | /v1/tree         | Browse directory tree              |
| GET    | /metrics         | Prometheus metrics                 |

### Check (stub)

Regex complexity analysis is not yet implemented:

```bash
rx check "(a+)+"          # Prints "not yet available"
rx check "(a+)+" --json   # Returns stub JSON response
```

## Configuration

All settings are controlled via environment variables with sensible defaults:

| Variable                  | Default            | Description                                  |
|---------------------------|--------------------|----------------------------------------------|
| `RX_CACHE_DIR`            | `~/.cache/rx`      | Cache directory for indexes and trace results |
| `RX_SEARCH_ROOT`          | (none)             | Restrict searches to this root directory      |
| `RX_SEARCH_ROOTS`         | (none)             | Multiple search roots (path-separated)        |
| `RX_MAX_SUBPROCESSES`     | `20`               | Maximum concurrent rg processes               |
| `RX_MIN_CHUNK_SIZE_MB`    | `20`               | Minimum chunk size before parallel splitting  |
| `RX_MAX_FILES`            | `1000`             | Maximum files to scan per directory           |
| `RX_MAX_LINE_SIZE_KB`     | `8`                | Maximum line size for rg JSON parsing         |
| `RX_LARGE_FILE_MB`        | `50`               | Threshold for large-file optimizations        |
| `RX_FRAME_BATCH_SIZE_MB`  | `32`               | Target decompressed size per zstd frame batch |
| `RX_NO_CACHE`             | `0`                | Disable trace result caching                  |
| `RX_NO_INDEX`             | `0`                | Disable file index usage                      |
| `RX_DEBUG`                | `0`                | Enable debug output                           |
| `RX_LOG_LEVEL`            | `info`             | Log level (debug, info, warn, error)          |
| `RX_FRONTEND_VERSION`     | (latest)           | rx-viewer frontend version to download        |
| `RX_DISABLE_CUSTOM_HOOKS` | `0`                | Block webhook URLs from request params        |
| `NEWLINE_SYMBOL`          | `\n`               | Symbol used to display newlines in output     |
| `NO_COLOR`                | (unset)            | Disable ANSI color output (per no-color.org)  |

## Differences from the Python Version

This is a ground-up Go rewrite of the [rx Python tool](https://github.com/wlame/rx). Key differences:

- **Native I/O**: uses `io.SectionReader` and `io.ReadAt` for chunked file access instead of spawning `dd` subprocesses.
- **Native compression**: all decompression (gzip, xz, bz2, zstd) runs in-process via pure Go libraries -- no subprocess decompressors.
- **Goroutine concurrency**: parallel chunk search and seekable zstd frame-batch processing use `errgroup` with bounded concurrency instead of Python's `ProcessPoolExecutor`.
- **Static binary**: single `CGO_ENABLED=0` binary with no runtime dependencies (besides `rg`).
- **Structured logging**: uses Go's `log/slog` for structured, level-aware logging.
- **Stubs**: regex complexity analysis and anomaly detectors are interface-only stubs (endpoints exist, return "not implemented").

## License

See LICENSE file.
