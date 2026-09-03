# AGENTS.md — rx-go

Instructions for AI coding agents working in this repository. If a sibling
checkout exists at `../AGENTS.md`, read it first: it holds the parity rules that
bind this repo to `rx-python` and `rx-viewer`. The same rules are repeated below
so this file stands alone.

## What this is

`rx` is a CLI and REST API for regex search, line indexing, sampling, anomaly
analysis and seekable-zstd compression of very large text files (built and
benchmarked at 1.3 GB, designed for 100 GB). It wraps `ripgrep` as the regex
engine and adds native parallel chunking, line-offset indexes, frame-parallel
zstd decoding, an HTTP API and a static SPA.

Target artifact: one statically linked binary (`CGO_ENABLED=0`, about 13 MB).
Runtime dependency: `ripgrep` 13+ on `PATH`.

This repo is the **flagship backend** of a product with three active repos:

| Repo | Role |
|---|---|
| `rx-go` (this repo) | Reference for the HTTP wire contract |
| `rx-python` | Second backend. Must be a drop-in replacement for this one. Reference for the cache format. Published on PyPI as `rx-tool`. |
| `rx-viewer` | Shared Svelte SPA, fetched from GitHub Releases at first `serve` start and served from `~/.cache/rx/frontend/` |

`rx-rust` also exists beside them. It is frozen. Do not read it for guidance.

## Parity rules (binding)

1. **Every behaviour change here is also made in `rx-python` in the same task**:
   CLI flags and defaults, exit codes, `--json` shapes, HTTP routes and bodies,
   `RX_*` variables, cache formats, webhook payloads. Tests go in both repos.
   If you cannot do the Python half, say so in your final report and add a
   `Parity gap:` line under `## [Unreleased]` in `rx-python/CHANGELOG.md`.
2. **The wire contract lives here.** `pkg/rxtypes/` plus the golden OpenAPI
   document `internal/webapi/testdata/openapi.golden.json` are the source of
   truth. Adding a field: update `pkg/rxtypes`, regenerate the golden file, then
   mirror in `rx-python/src/rx/models.py` and `rx-viewer/src/lib/types.ts`.
   Renaming, removing or changing a field's meaning is breaking: bump the
   contract version, note it in all three changelogs, release backends before
   the viewer.
3. **Do not break the cache format.** Python-built index and trace-cache files
   must stay readable here and the other way round. Field names, the mtime
   string format and the JSON key spacing used for the patterns hash are part
   of the contract (see Gotchas).
4. **Prove it.** Before reporting a CLI or HTTP change as done, run the same
   command or request against both backends and diff the JSON. Run
   `go test ./internal/testparity/...` when `../rx-python` has a `.venv`.
5. **Do not add new default differences.** Known gaps (default port 7777 vs
   8000, different detector sets, no `/v1/complexity` here) are tracked in
   `../tickets/10-backend-parity-gaps.md`.

## Quick orientation

| Where | What |
|---|---|
| `cmd/rx/main.go` | Entry point. `preprocessArgs` routes a bare pattern to `trace` |
| `internal/clicommand/` | One file per subcommand: `trace`, `index`, `samples`, `compress`, `serve` |
| `internal/webapi/` | HTTP layer (chi + huma v2), middleware, OpenAPI, SPA fallback, `runDetached` |
| `internal/trace/` | Search engine: `chunker.go`, `worker.go` (rg subprocess), `seekable.go`, `compressed.go`, `cache.go` |
| `internal/samples/` | Line and byte-offset resolver shared by CLI `samples` and `/v1/samples` |
| `internal/index/` | Line-offset index builder, stats (Welford + reservoir), on-disk store |
| `internal/seekable/`, `internal/compression/` | Seekable-zstd codec; format detection; pooled decoders |
| `internal/analyzer/` | Detector registry (Freeze barrier) and 9 detectors under `detectors/` |
| `internal/hooks/` | Webhook dispatcher with SSRF defence |
| `internal/paths/` | `--search-root` sandbox |
| `internal/frontend/` | Downloads and extracts the viewer bundle |
| `internal/tasks/` | In-memory background task manager for `POST /v1/index` and `/v1/compress` |
| `internal/prometheus/` | Metrics behind an `atomic.Bool` enable gate (off in CLI, on in `serve`) |
| `internal/output/` | Shared human-readable formatting |
| `internal/testparity/` | Harness that runs `../rx-python` and diffs output |
| `internal/testutil/counting/` | Byte-counting readers for bounded-read tests |
| `pkg/rxtypes/` | Wire types. OpenAPI source of truth |
| `docs/` | 35 MkDocs pages, built strict in CI, deployed to GitHub Pages |

## Build, run, test

`just` is the entrypoint. `just --list` shows every recipe.

```bash
just build                                       # static binary into dist/
just run trace "error" /var/log/app.log          # run from source
just serve --port=8080 --search-root=/var/log    # 8080 matches the viewer dev proxy

just test                                        # full suite
just test -run TestTrace ./internal/trace/       # arguments pass straight through
just test-race                                   # race detector (about 30 s)
just test-repeat ./internal/trace/               # 10x, to hunt a flaky test
just bench                                       # benchmarks (not a CI gate)
just cover                                       # tests + the coverage floor
just parity trace error app.log                  # diff --json against rx-python

just ci                                          # exactly what GitHub CI runs
just check                                       # ci + cover + build + vuln
```

`just ci` is `fmt-check vet lint tidy-check test-race docs-build`, in that
order, and `.github/workflows/ci.yml` runs `just ci` — the two cannot
disagree. The **coverage floor is 80%** (82.4% today), enforced by
`just cover`; raise it as coverage improves and never lower it to make a red
build green.

Go 1.25+ is required (`go.mod` says `go 1.25.0`; huma v2 needs it); CI runs
1.25 and 1.26. `golangci-lint` v2.x and `govulncheck` are needed for `just
lint` and `just vuln` — install them with `go install`. `just docs-build`
needs `uv`.

## Architecture

```
CLI (cobra)                HTTP (chi + huma v2)
    │                           │
internal/clicommand/     internal/webapi/
    └──────────┬────────────────┘
               │
       internal/trace/     chunker → workers → rg --json → dedup → response
       internal/samples/   line/offset resolver (index-aware)
       internal/index/     index builder + analyzer coordinator
       internal/seekable/  frame-parallel zstd
               │
   compression · paths · hooks · analyzer · tasks · prometheus
```

Data flow for `rx trace "pattern" big.log`:

1. `preprocessArgs` rewrites argv to `rx trace "pattern" big.log`.
2. `clicommand/trace.go` validates paths against the sandbox and builds `trace.Options`.
3. `trace/engine.go` classifies files (plain, compressed, seekable, cached) and plans newline-aligned chunks (`chunker.go`).
4. Each worker pipes its byte range into `rg --json` over stdin and translates `absolute_offset` from rg-stream-relative to file-relative.
5. Matches are deduplicated at chunk boundaries by range containment and sorted; `max_results` cancels sibling workers.
6. Output is rendered by `clicommand/trace.go` (human) or serialized as `rxtypes.TraceResponse` (`--json`, HTTP).

## Design contracts you must preserve

1. **Bounded reads.** No code path reads more bytes than the request needs,
   except `rx index` (new index), `rx trace` without `--max-results`, and
   `rx compress`. Every new file-reading path gets a budget test that uses
   `counting.InjectOpen` and asserts the byte count.
2. **Cache cross-compatibility with Python.** Keep every `IndexAnalysis` field.
   Never add `omitempty` to a schema-documented wire field; use explicit nulls.
   Go-only extensions go under `go_extras`.
3. **Freeze-barrier registry.** `internal/analyzer/registry.go` freezes before
   the server starts; later registration panics; readers are lock-free. Do not
   add a mutex. Stateful detectors register a factory with
   `RegisterLineDetector`, never a shared instance with `Register`.
4. **Sandbox on every path.** Every HTTP handler and every CLI command that
   receives a path, including output paths, calls
   `paths.ValidatePathWithinRoots` before touching the filesystem.
5. **SSRF defence stays layered.** `internal/hooks/config.go` rejects loopback,
   link-local, RFC 1918, CGNAT, multicast and unspecified addresses, and
   resolves DNS at validation time. Do not weaken it. Redirects must be refused
   or re-validated.
6. **Metrics are off by default.** Wrap every new metric call behind the
   enable gate. Never use `r.URL.Path` as a label; use the chi route pattern.
7. **Detached goroutines recover.** Any `go func()` spawned from a handler goes
   through `internal/webapi/run_detached.go::runDetached`.
8. **Exit codes are part of the CLI contract.** 0 success, 1 generic error,
   2 usage error, 3 file not found, 4 access denied, 5 interrupted. They must
   match rx-python.

## Coding standards

- Follow the repo's existing style; `gofmt -s` and `goimports` clean; lint clean.
- `context.Context` first on any function that does I/O; errors wrapped with
  `%w` when the wrap adds information; godoc on every exported symbol.
- Comments are more generous than typical Go, on purpose: explain the Go idiom,
  the goroutine lifecycle and the invariant. Label invariants (`INVARIANT:`,
  `SECURITY:`). Do not reference review rounds, stages, plans or ticket numbers
  in code comments; describe what the code guarantees instead.
- Prefer a lookup table over a chain of `if`/`switch` when the logic is a mapping.
- Long flags use `=` in help text, docs and printed commands.
- Small functions, early returns, no flag parameters that switch behaviour.

## Testing

- Table-driven tests for three or more similar cases. Golden files in
  `testdata/` for CLI output and the OpenAPI spec. Integration tests in
  `internal/webapi/*_integration_test.go` use `httptest` and a real viewer
  tarball fixture (`internal/webapi/testdata/rx-viewer-v0.2.0-dist.tar.gz`).
- Byte-budget tests for anything that reads files. Binary-level tests for exit
  codes and `--help` (run the built binary, assert `$?`).
- `t.TempDir()` and `t.Setenv()`; `t.Parallel()` and `t.Setenv()` are mutually
  exclusive.
- Tests must be deterministic. Do not assert on wall-clock speed or on how far
  a race got before a cancel fired.
- Tests that need an external tool (`rg`, `zstd`) skip with a reason when it is
  missing, except `rg`, which is a hard requirement.
- The completion gate before you say "done":

```bash
just ci
```

Paste the output. Do not summarize it.

## Git, changelog, release

- Commit: one imperative sentence, capital, full stop, no prefix, no body.
- `CHANGELOG.md` (Keep a Changelog) gets an entry under `## [Unreleased]` for
  every behaviour change. Consolidate rather than duplicate bullets.
- Version is stamped from `git describe --tags --dirty --always`; there is no
  version constant in the source. Tags are `vX.Y.Z` on `main` from a clean
  tree. `just release-dry patch` previews, `just release patch` runs the CI
  gate, promotes the changelog, commits and tags — and then prints the `git
  push` commands rather than running them. Pushing the tag is what triggers
  `release.yml`, which builds the binaries and their sha256 sidecars.
- Never push. Never `git reset`, `stash`, `rebase` or discard changes.
- Working files (plans, audits, notes) go in the gitignored `.claude/`.

## Security notes

- `serve` binds `127.0.0.1:7777` by default and has no authentication. Anyone
  who can reach the socket can run any operation inside the sandbox.
- User regex patterns are always passed to rg as `-e <pattern>` so a leading
  dash cannot become a flag. Keep it that way.
- The viewer bundle is downloaded from GitHub Releases; verify its checksum when
  the sidecar exists (`../tickets/07-frontend-bundle-integrity.md`).
- Threat model and defences: `docs/concepts/security.md`. Keep it current when
  you change a defence.

## Gotchas

- `os.File.ReadAt` is goroutine-safe; `Read` is not.
- `bufio.Scanner` truncates lines over 64 KB silently; use a 16 MB buffer as
  `rgjson.go` does. The chunker's newline lookahead is 256 KB; lines longer than
  that can split a chunk mid-line.
- `filepath.Join("/a", "/etc/passwd")` returns `/a/etc/passwd`; check tar
  symlink targets with `filepath.IsAbs` directly.
- Python's `isoformat()` drops `.000000` when microseconds are zero and writes
  local time; `formatMtime` in `internal/index/store.go` matches that.
- Python's `json.dumps(sort_keys=True)` emits `", "` and `": "`; the patterns
  hash in `internal/trace/cache.go` is built byte by byte to match.
- Go map iteration is random: sort keys before any output that must be stable.
- huma v2 forbids `*int` on query params; use sentinels (`0` unset MaxResults,
  `-1` unset Context). huma serves `/openapi.json` as `application/openapi+json`.
- rg's `absolute_offset` is relative to rg's stdin; add `chunk.Offset`.
- `klauspost/compress` zstd encoder levels are coarse (1 to 4); higher values clamp.
- `sync.Mutex` + map beats `sync.Map` for check-and-insert (see `internal/tasks`).

## Open work

Audit tickets live in `../tickets/` (outside this repo). Read
`../tickets/README.md` before taking one. Do not create a "known issues" list
in this file; file a ticket.

## What NOT to do

- Do not change behaviour here without the same change in `rx-python`.
- Do not change a `pkg/rxtypes` field without the golden spec, `models.py` and `types.ts`.
- Do not spawn raw `go func()` in `webapi`.
- Do not add `omitempty` to schema-documented fields.
- Do not use `r.URL.Path` as a metric label.
- Do not add a mutex to the analyzer registry.
- Do not read a whole file when a range is enough.
- Do not weaken the sandbox or the SSRF checks.
- Do not accept a path parameter, including output paths, without validating it.
- Do not swallow an rg error into an empty result; surface it and use the right exit code.
- Do not write production code without a failing test first.
- Do not report "tests pass" without the captured output.
