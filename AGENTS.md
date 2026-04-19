# AGENTS.md — rx

Instructions for AI coding agents (Claude Code, Cursor, Aider, Copilot, etc.) working on this repository.

## What this is

`rx` is a high-performance CLI + REST API tool for parallel regex search, indexing, sampling, and compression of very large text files (built and benchmarked at 1.3 GB; designed to work on files up to 100 GB). It wraps `ripgrep` as the external regex engine, adds native-Go parallel chunking, line-offset indexes, seekable-zstd compression with frame parallelism, an HTTP API, and a static SPA.

Target binary: **single statically-linked executable** (`CGO_ENABLED=0`), ~13 MB, no runtime dependencies beyond `ripgrep` on `PATH`.

## Quick orientation

| Where | What |
|---|---|
| `cmd/rx/main.go` | CLI entry point; default-dispatch routes bare patterns to `trace` |
| `internal/clicommand/` | One file per subcommand: `trace`, `index`, `samples`, `compress`, `serve` |
| `internal/webapi/` | HTTP layer (chi + huma v2), 12 routes, embedded favicon, SPA fallback |
| `internal/trace/` | Parallel regex scan engine — the hottest code path |
| `internal/samples/` | Shared line/byte-offset resolver for CLI `samples` + HTTP `/v1/samples` |
| `internal/index/` | Line-offset index builder + on-disk cache format |
| `internal/seekable/` | Seekable-zstd encoder/decoder with frame-parallel decode |
| `internal/compression/` | Format detection + pooled zstd decoder + unified `ReadCloser` wrapping |
| `internal/hooks/` | Fire-and-forget webhook dispatcher with layered SSRF defense |
| `internal/analyzer/` | Pluggable `FileAnalyzer` registry (Freeze-barrier pattern, empty in v1) |
| `internal/paths/` | `--search-root` sandbox enforcement |
| `internal/prometheus/` | Metrics registration with an `atomic.Bool` enable-gate (off in CLI, on in serve) |
| `internal/testutil/counting/` | Byte-counting `io.Reader`/`ReadAt`/`File` wrappers for efficiency tests |
| `pkg/rxtypes/` | Wire types (request/response JSON shapes, OpenAPI source of truth) |
| `docs/` | 33 MkDocs-ready pages + `MIGRATION.md` |

## Build & run

```bash
# Build static binary (primary distribution artifact)
CGO_ENABLED=0 go build -ldflags='-s -w' -o /tmp/rx-static ./cmd/rx

# Run the full test suite with race detector
go test -race ./...

# Stress under multiple iterations (hunts flaky tests)
go test -race -count=10 ./...

# Lint (staticcheck + govet + revive + gosec via golangci-lint v2.x config)
golangci-lint run ./...

# Benchmarks (baseline numbers in BENCHMARKS.md)
go test -bench=. -benchmem ./internal/trace/
go test -bench=. -benchmem ./internal/seekable/

# Makefile shortcuts: build, test, lint, fmt, ci
make test
```

**Go toolchain:** 1.25+ required (huma v2 uses 1.25 features). Tested on 1.26.2.

**External runtime dep:** `ripgrep` 13+ on `PATH`. Verify with `rg --version`.

## Architecture at a glance

```
CLI (cobra)                HTTP (chi + huma v2)
    │                           │
    └──────────┬────────────────┘
               │
        internal/clicommand/  internal/webapi/
               │                   │
               └─────────┬─────────┘
                         │
                 internal/trace/   ← hot path: chunker + workers + rg subprocess pool
                 internal/samples/ ← line/byte resolver (shared CLI+HTTP)
                 internal/index/   ← line-offset index builder (constant-memory stats)
                 internal/seekable/ ← zstd frame-parallel decode
                         │
         internal/compression/  internal/paths/  internal/hooks/  internal/analyzer/
```

Data flow for `rx trace "pattern" largefile.log`:
1. `cmd/rx/main.go::preprocessArgs` rewrites argv → `rx trace "pattern" largefile.log`
2. `internal/clicommand/trace.go` resolves paths, patterns, flags
3. `internal/trace/engine.go` plans chunks (`chunker.go`), spawns worker goroutines (`errgroup.SetLimit`)
4. Each worker: `os.File.ReadAt` → pipe bytes to `rg --json` via `exec.Cmd.Stdin` → parse events
5. Results collected via channels, deduplicated by range-containment at chunk boundaries
6. Output via `internal/output/trace.go` (ANSI-colored) or JSON-serialized `rxtypes.TraceResponse`

## Design contracts agents MUST preserve

### 1. Bounded-Read Contract (Stage 9 R5)

The tool's core value is efficiency on extra-large files. **No code path may read more bytes than explicitly requested**, except:
- `rx index` building a new index (legitimate full scan)
- `rx trace` without `--max-results` (legitimate full scan)
- `rx compress` (legitimate full scan)

Enforced by byte-budget tests in `internal/testutil/counting/` + per-package `*_budget_test.go` / `*_maxresults_test.go`. **When adding a new file-reading code path, add a budget test alongside.** The test pattern:

```go
counter, restore := counting.InjectOpen(&openFileForFoo) // seam in prod code
t.Cleanup(restore)
// ...run the function under test...
require.Less(t, counter.Load(), int64(expectedBudget))
```

### 2. Python cache cross-compatibility

`~/.cache/rx/indexes/*.json` and `~/.cache/rx/trace/*.json` files must be byte-readable by BOTH this Go implementation and the original Python `rx-python`. Preserve:
- JSON field names (camelCase for external, snake_case for internal — see `pkg/rxtypes/`)
- Every existing `IndexAnalysis` field (`LineCount`, `LineLengthAvg`, `LineLengthP99`, etc.)
- mtime format: Python-compatible ISO string (Stage 8 R3 Blocker fix)

**Do not add `omitempty` to any schema-documented wire field.** Use explicit nulls; omitempty is reserved for Go-only extensions under `go_extras`.

### 3. Pluggable analyzer registry — Freeze-barrier

`internal/analyzer/registry.go` uses the Freeze-barrier pattern: `Register()` during package init / main setup, `Freeze()` before server starts, post-Freeze `Register()` panics. **Readers are lock-free; no `sync.RWMutex`**. Do NOT add a mutex — the whole point is that the post-Freeze phase is write-free.

v1 ships with zero real analyzers; `/v1/detectors` returns `{detectors: [], categories: [], severity_scale: [...]}`. Adding a new analyzer is a 1-file drop-in — see `docs/concepts/analyzers.md`.

### 4. Layered SSRF defense for webhooks

`internal/hooks/config.go::internalHostReason` rejects loopback, link-local, RFC 1918, RFC 6598 CGNAT (100.64/10), multicast, unspecified. DNS names are resolved at validation time with a 2 s timeout. `RX_HOOK_STRICT_IP_ONLY=true` additionally rejects hostnames entirely (opt-in paranoid mode). **Do not weaken these checks** — they close a documented SSRF vector and were verified with red-green TDD.

### 5. Prometheus metrics are OFF by default

`internal/prometheus/metrics.go` uses an `atomic.Bool` enable-gate. CLI mode leaves it false → zero-cost instrumentation (single atomic load per call site, often compiler-optimized away). `serve` mode calls `prometheus.Enable()` once during `webapi.Start`. When adding a new metric call site, wrap it behind the gate — never unconditionally.

### 6. Detached goroutines need `defer recover()`

Chi/huma's middleware `recover()` only protects the request goroutine. Any `go func()` spawned from a handler must wrap its body in a recover boundary. The canonical helper is `internal/webapi/run_detached.go::runDetached(name, fn)` — use it for background tasks (see `runIndexTask`, `runCompressTask`). **Do not spawn raw `go func()` in webapi without recover.**

### 7. Sandbox enforcement is non-negotiable

Every HTTP endpoint that accepts a `path` parameter MUST call `paths.ValidatePathWithinRoots()` before opening the file. No exceptions — not even for internal-looking endpoints. See `internal/paths/security.go` + `docs/concepts/security.md`.

## Engineering mandates

These are binding for any agent working in this repo:

### TDD (red-green-refactor)

**No production code without a failing test first.** The workflow:
1. Write the smallest test that captures the desired behavior
2. Run it — it MUST fail with the expected error (capture output)
3. Write the minimum code to pass
4. Run it — it MUST pass (capture output)
5. For bug fixes: revert the fix, confirm test fails again, restore the fix — proves regression guard works
6. Only THEN commit

Exception: pure documentation changes, test-only utilities, config file edits.

### Verification before claiming done

**Claims like "this should work" or "tests pass" are not acceptable without captured command output in the same message.** The canonical completion gate:

```bash
go build ./...                    # exit 0
go test -race -count=1 ./...      # all packages green
golangci-lint run ./...           # 0 issues
CGO_ENABLED=0 go build -o /tmp/rx-static ./cmd/rx   # binary still builds
```

For performance-sensitive changes, add a re-run of the relevant benchmark from `BENCHMARKS.md` or `.go-rewriter/comparison/pre-port-baseline.md`.

### Go idioms

- `context.Context` as first parameter on any I/O-doing function
- Errors wrapped with `%w` when context adds information
- Exported symbols have godoc (complete sentences, subject-first)
- Interfaces accepted as parameters, structs returned
- `t.TempDir()` over `os.MkdirTemp` in tests; `t.Setenv()` over `os.Setenv`
- `t.Parallel()` + `t.Setenv()` are **mutually exclusive in Go 1.25+** — test helper panics. Choose one.

### Comments — generous by project convention

This codebase has **more comments than typical Go code**. Maintainer is more familiar with Python than Go, so:
- Non-obvious Go idioms get a comment explaining WHY (e.g. "decoder close order: decoder first, then source — decoder may need source trailer during Close")
- Goroutine spawning points get a comment explaining lifecycle + cancellation
- Stateless vs stateful library APIs are called out explicitly
- Security / invariant decisions are labeled (look for `INVARIANT:`, `SECURITY:`, `PATH-LOCK INVARIANT:`, etc.)

Preserve this style. Godoc on every exported symbol; inline comments for anything that wouldn't be obvious to a Go-newcomer.

## Testing patterns

- **Table-driven tests** when there are 3+ similar cases. Keep the struct slice minimal; avoid speculative fields.
- **Golden files** in `testdata/` for CLI output (ANSI-colored) and OpenAPI spec
- **Byte-budget tests** for anything that reads files (see Design Contract #1)
- **Parity tests**: `internal/testparity/` exercises CLI + HTTP paths against shared fixtures
- **Race stress**: `go test -race -count=10` must be flake-free. Any test that fails 1-in-10 goes on the debug fix list.
- **Integration tests**: `internal/webapi/*_integration_test.go` spins up `httptest.NewServer` and hits real endpoints including real-tarball SPA serving

Fixtures:
- `testdata/` — per-package small fixtures (< 10 KB each)
- `internal/webapi/testdata/rx-viewer-v0.2.0-dist.tar.gz` — real SPA bundle (~11 MB) for integration tests
- `/Users/wlame/twitters_1m.txt` etc. — user-provided large fixtures for benchmarks; see `BENCHMARKS.md`

## Performance hot paths

| Surface | Complexity | Notes |
|---|---|---|
| `internal/trace/chunker.go` | O(file_size / chunk_size) | Native `os.File.ReadAt`, newline-aligned, per-chunk dedup via range-containment |
| `internal/trace/worker.go` | O(matches in chunk) | Pipes chunk bytes into `rg --json` via `exec.Cmd.Stdin`; cancels on `max_results` cap hit |
| `internal/samples/resolver.go` | O(endLine × avg_line_len) | Uses line index when cached (O(log N) seek); falls back to linear scan |
| `internal/index/builder.go` | O(file_size), O(reservoirCap) memory | Welford's algorithm for mean/stddev, reservoir sampling (10k samples) for percentiles |
| `internal/seekable/decoder.go` | O(frame decoded) per call | Uses pooled `zstd.Decoder` from `internal/compression/decoder_pool.go` |
| `internal/trace/seekable.go::scanFrameBatch` | O(1) frame in flight | `io.Pipe` + writer goroutine — streams frames into rg instead of materializing batch |

Benchmarks from real-world 1.3 GB fixture:
- Cold index build: **675 ms, 18 MB RSS** (was 1655 ms, 468 MB pre-optimization)
- Warm index read: **5 ms**
- Samples pagination (`-l 1-1000`): **5 ms**
- Trace `--max-results=10`: **44 ms** (was full-file-scan pre-R5)
- Trace unlimited: **38 s** (2.96× vs Python `rx-python`)

## Gotchas — real issues caught in prior work

### File I/O

- `os.File.ReadAt` is goroutine-safe; `Read` is not. Native chunking relies on this.
- `bufio.Scanner` default buffer is 64 KB — silently truncates long log lines. Use `scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)` (16 MB cap in rx-go).
- `filepath.Join("/abs", "/etc/passwd")` returns `/abs/etc/passwd` on Linux — absolute 2nd arg is treated as suffix. Tarball symlink-target checks must inspect `hdr.Linkname` directly with `filepath.IsAbs` rejection; do not rely on `filepath.Join` clamping.
- `archive/tar.Writer.Close()` refuses when bytes-written ≠ declared size. Some traversal tests can't lie about size as a result.

### Time & serialization

- Python's `datetime.isoformat()` omits trailing `.000000` when microseconds are zero; Go's custom formatters typically include them. For cache cross-compat, the formatter in `internal/index/store.go::formatMtime` branches on `Nanosecond() == 0`.
- Python writes mtime in **local time**, not UTC. `time.Time.ModTime()` must be converted via `.Local()` before formatting.
- `time.RFC3339Nano` is NOT Python-compatible (extra nanoseconds + Z suffix). Use the custom `ISOTime` marshaler.

### JSON

- Python's `json.dumps(sort_keys=True)` emits `', '` and `': '` (with spaces); Go's `encoding/json` omits spaces. For patterns-hash parity, the hash input is built byte-by-byte manually in `internal/trace/cache.go::computePatternsHash`.
- Go map iteration is randomized. Anywhere output depends on key order (CLI samples, cache files), sort keys explicitly before emitting.
- `bytes.Buffer` is **NOT goroutine-safe**. Test-only `safeBuffer` pattern exists in `internal/clicommand/serve_test.go`; do not promote to production.

### Concurrency

- Uncaught panics in `go func()` terminate the process. Chi/huma recover only protects the request goroutine — use `internal/webapi/run_detached.go::runDetached` for any detached work.
- `sync.Mutex + map` beats `sync.Map` for check-and-insert workloads (e.g. path locking in `internal/tasks/manager.go`). `sync.Map` is tuned for write-once / read-many.
- `atomic.Bool` for a "write once, read many" flag is cheaper than `sync.Mutex` around a bool. See `internal/analyzer/registry.go::frozen` and `internal/prometheus/metrics.go::enabled`.

### Third-party libraries

- `klauspost/compress/zstd` encoder levels are **coarse**: `Fastest=1`, `Default=2`, `BetterCompression=3`, `BestCompression=4`. Callers passing 6-22 get clamped. Documented in `docs/cli/compress.md`.
- `klauspost/compress zstd.WithEncoderConcurrency` parallelizes WITHIN a frame, not across. Seekable-zstd needs cross-frame parallelism — the encoder spins its own errgroup.
- `huma v2` forbids `*int` on query/path/header/form params (panics at `huma.go:189`). Use sentinel values (`0` for "unset MaxResults", `-1` for "unset Context").
- `chi` route pattern must be read via `chi.RouteContext(r.Context()).RoutePattern()` — using `r.URL.Path` in metrics labels is a cardinality bomb.
- `huma` emits Content-Type `application/openapi+json` for `/openapi.json`. Swagger UI + rx-viewer both accept it; note for clients that hard-match on `application/json`.
- `rg`'s `absolute_offset` in `--json` output is relative to rg's OWN input stream, not to the source file. When piping a chunk via stdin, the worker translates `chunk.Offset + rg.AbsoluteOffset` → file-absolute.

### Prometheus

- **Never use raw `r.URL.Path` as a metric label.** `/v1/tasks/{id}` produces a new series per request ID → unbounded memory. Always use the route pattern.
- Always gate metric calls behind `prometheus.enabled.Load()` (see Design Contract #5).

## Authoritative sources

In priority order (newer overrides older):

1. **Source code** — if something contradicts a doc, the source is right; update the doc
2. **`docs/`** — 33 user-facing MkDocs pages (installation, CLI, API, concepts, performance, troubleshooting, MIGRATION)
3. **`BENCHMARKS.md`** — baseline benchmark numbers + harness invocation
4. **`.go-rewriter/stage-10-final-report.md`** — comprehensive architecture + design decisions (Oct 2026 baseline)
5. **`.go-rewriter/stage-9-round5-report.md`** — bounded-reads contract details + byte-budget test pattern
6. **`.go-rewriter/comparison/FINAL-COMPARISON.md`** — comparison with sibling codebase `another-rx-go`; §10 is the "ideas worth borrowing" catalog
7. **`.go-rewriter/comparison/IMPLEMENTATION-PLAN.md` + `post-port-results.md`** — most recent cross-pollination work

Intermediate design docs under `.go-rewriter/` are chronological records of the build-up; consult when understanding WHY something was done a particular way.

## External context

- **Sibling codebase:** `/Users/wlame/dev/rewriting-rx-project/another-rx-go/` — an independent Go rewrite. Read-only reference. Contains several borrowable patterns already ported into this repo (Welford accumulator, `io.Pipe` streaming, `sync.Pool` decoder, `shouldRouteToTrace`). See `.go-rewriter/comparison/FINAL-COMPARISON.md` §10.
- **Predecessor:** `/Users/wlame/dev/rewriting-rx-project/rx-python/` — original Python implementation. Behavior reference for parity testing. See `docs/MIGRATION.md` for documented Python-bug / Go-correct divergences.
- **Frontend SPA:** GitHub `wlame/rx-viewer` releases → `dist.tar.gz` extracted to `~/.cache/rx/frontend/` on first `serve` start. Served by `internal/webapi/handlers_static.go`.

## When you're unsure

1. Run `go doc github.com/wlame/rx-go/internal/<package>` to see the package's public API with godoc.
2. `grep -rn "PATTERN" internal/` — the codebase has enough inline comments that most non-obvious patterns have a nearby explanation.
3. Check `docs/concepts/<topic>.md` — concepts are explained in prose form.
4. For performance questions, re-run the benchmark from `BENCHMARKS.md` before optimizing; measure-before-optimize.
5. If still stuck after 10-15 minutes, ask the human maintainer with a specific question — not "how should I…" but "I see X does Y, is that intentional?"

## What NOT to do

- Don't spawn raw `go func()` in webapi handlers (use `runDetached`)
- Don't add `omitempty` to schema-documented wire fields
- Don't use `r.URL.Path` as a Prometheus label (use the chi route pattern)
- Don't add a mutex to the analyzer registry (Freeze barrier is the whole point)
- Don't read the full file if you only need a range (bounded-reads contract)
- Don't weaken SSRF checks in `internal/hooks/config.go`
- Don't assume Python-compat without checking cache cross-read in both directions
- Don't claim "tests pass" without captured command output
- Don't write production code without a failing test first
