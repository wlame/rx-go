# AGENTS.md — rx-go

Instructions for AI coding agents working in this repository. If a sibling
checkout exists at `../AGENTS.md`, read it first: it holds the rules that bind
this repo to `rx-viewer` and `rx-python`. The ones that bind this repo are
repeated below so this file stands alone.

## What this is

`rx` is a CLI and REST API for regex search, line indexing, sampling, anomaly
analysis and seekable-zstd compression of very large text files (built and
benchmarked at 1.3 GB, designed for 100 GB). It wraps `ripgrep` as the regex
engine and adds native parallel chunking, line-offset indexes, frame-parallel
zstd decoding, an HTTP API and a static SPA.

Target artifact: one statically linked binary (`CGO_ENABLED=0`, about 13 MB).
Runtime dependency: `ripgrep` 13+ on `PATH`.

This repo is the **flagship backend** of a product with two active repos and a paused one:

| Repo | Role |
|---|---|
| `rx-go` (this repo) | The focus. Reference for the HTTP wire contract and the cache format |
| `rx-viewer` | Shared Svelte SPA, fetched from GitHub Releases at first `serve` start and served from `~/.cache/rx/frontend/` |
| `rx-python` | The original backend, on PyPI as `rx-tool`. **Paused** since 2026-10-02; it stays as it is while this repo moves on. |

`rx-rust` also exists beside them. It is frozen. Do not read it for guidance.

## Go-first policy (binding while rx-python is paused)

1. **Behaviour changes land here alone**, plus in `rx-viewer` when they reach
   the UI. `rx-python` stays as it is.
2. **Record every divergence.** A change that makes this backend differ from
   rx-python in a CLI flag, default or exit code; a `--json` shape; an HTTP
   route, body, status or the contract version; an `RX_*` variable; a cache
   file or its format version; a webhook payload; or the viewer version
   window gets a row in `../tickets/PARITY-DEBT.md`, in the same task. That
   file is rx-python's work list for when it resumes.
3. **The wire contract lives here.** `pkg/rxtypes/` plus the golden OpenAPI
   document `internal/webapi/testdata/openapi.golden.json` are the source of
   truth, published to `docs/api/openapi.json` (kept in step by
   `just spec-sync`; `just ci` fails when they differ).

   Adding a field: update `pkg/rxtypes`, run
   `go test ./internal/webapi/ -update-golden`, `just spec-sync`, then
   `cd ../rx-viewer && just gen-types`.

   Renaming, removing or changing a field's meaning is breaking: bump
   `ContractVersion` in `internal/webapi/contract.go` and the supported major
   in `rx-viewer/src/lib/utils/contractVersion.ts`, note it in both
   changelogs, and release this backend before the viewer.
4. **The cache format lives here too.** Index and trace-cache files written
   by rx-python share `~/.cache/rx/` with this backend's, so the format keeps
   the Python quirks it was born with: field names, the mtime string format
   and the JSON key spacing used for the patterns hash (see Gotchas). A
   change to a field bumps `index.Version` and gets a ledger row; rx-python
   then treats the new files as absent and rebuilds its own.
5. **Show the change.** Before reporting a CLI or HTTP change as done, paste
   the `--json` output or the response body before and after it. `just
   parity` and `internal/testparity/` diff against `../rx-python`; use them to
   see what a change diverges, not as a gate.

## JSON object key order is not part of the contract

Two backends can return the same document with its object keys in a
different order, and that is not a defect. An object is unordered by
definition, every conformant parser gives the same result, and Go's
`encoding/json` always sorts map keys lexicographically — so a wire body
built from a `map[string]T` cannot carry any other order, whatever
rx-python does.

Compare parsed documents, not bytes. `samples --offsets=20000,1000` is
the case that shows it: rx-go's JSON lists `"1000"` before `"20000"`,
rx-python lists them in request order, and the two documents are equal.

Where order *is* visible to a person it is contract, and both backends
sort it the same way: human `samples` output goes in numeric order of
the position, with a range sorting by its left-hand value
(`rx-go/internal/output/samples.go::keyLess`,
`rx-python/src/rx/models.py::sample_key_order`).

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
| `internal/seekableindex/` | Frame → line-range index for a seekable `.zst`; the format rx-python defined |
| `internal/analyzer/` | Detector registry (Freeze barrier) and 9 detectors under `detectors/` |
| `internal/hooks/` | Webhook dispatcher with SSRF defence |
| `internal/paths/` | `--search-root` sandbox |
| `internal/frontend/` | Downloads and extracts the viewer bundle |
| `internal/tasks/` | In-memory background task manager for `POST /v1/index`, `/v1/compress` and the index builds `GET /v1/samples` waits for; a task's done channel and progress |
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
just test-race                                   # race detector (about 200 s on a Mac, mostly internal/trace)
just test-repeat ./internal/trace/               # 10x, to hunt a flaky test
just bench                                       # benchmarks (not a CI gate)
just cover                                       # tests + the coverage floor
just parity trace error app.log                  # diff --json against rx-python

just ci                                          # exactly what GitHub CI runs
just check                                       # ci + cover + build + vuln
```

`just ci` is `fmt-check vet lint tidy-check scaffolding-check spec-check
test-race docs-build`, in that order, and `.github/workflows/ci.yml` runs `just ci` — the two cannot
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

1. **One line numbering, everywhere.** A line number in any rx answer is
   the line's 1-based position in the file's text, counted from the
   first line. It means the same thing in a match, in a context line, in
   an index checkpoint, in an anomaly range, in `samples`, over HTTP and
   in the viewer, and it does not depend on how the file is stored:
   plain, gzipped and seekable-zstd copies of one log answer
   identically. The companion rule holds for bytes: an offset is a
   position in the file's text, which for a compressed file is its
   decompressed stream — the same coordinate `trace` reports and
   `samples` accepts. `samples --lines=N` and `samples --offsets=B` are
   inverses of each other on any file.

   The number is derived, never guessed. Chunk workers count newlines in
   the bytes they already read, frame scans count them as frames are
   decompressed, and a pass cut short by a `--max-results` cap leaves
   `absolute_line_number` at -1 rather than reporting a number counted
   from the wrong place. `relative_line_number` carries the same value
   whenever it is known, and only a cut-short scan leaves it
   chunk-relative. A capped search of a plain file meets that more often
   than one of a compressed file, because a frame carries its own line
   count while a chunk stops part-way; the caller resolves what it needs
   through `samples --offsets=…`, which answers a whole batch in one
   pass. Do not add a surface that numbers lines its own way.
2. **An index is an accelerator, never a source of truth.** No value rx
   reports may depend on whether an index exists. An index only changes
   how fast the answer is reached, so any code path that consults one
   must report the same thing when it does not. The regression harness
   builds each answer twice, once with the cache empty and once with a
   freshly built index, and compares them.

   The rule the comparison applies, to an index, to the trace cache and
   to `--no-index` alike: every field of the two answers is equal,
   except that a line number which is `-1` in one answer may be the
   true line number in the other. `-1` means "not computed". A number
   that is filled in is the line holding the byte offset, and
   `relative_line_number` beside it carries the same number. Tests
   compare trace answers through `internal/testutil/traceanswer`:
   `RequireAgree` applies this rule, and `RequireSame` is its strict
   form for two scans that ran to the end, where neither answer has a
   `-1`. A capped search of a chunked plain file keeps whichever
   matches its workers found first, so two capped runs can hold
   different matches; a test that compares capped answers uses a
   layout where the matches kept are fixed.

   There is exactly one thing an index does change, and it is *whether* a
   line number is known rather than *what* it is. A scan cut short by
   `--max-results` never read the bytes before the chunk that won, so
   numbering its matches means reading them now — gigabytes, to answer a
   search that stopped early on purpose. Without an index those matches
   keep `absolute_line_number` -1; with one the count starts at the
   nearest checkpoint and is cheap, so it is done. A caller that wants
   the number regardless asks `samples --offsets=…`, which answers a
   whole batch in one pass, or passes `--no-index`: that reads and
   writes no index and counts from byte 0 up to the last unnumbered
   match, so the flag changes the cost and never the answer. The trace
   cache numbers them too: a cache hit reads the file again from the
   nearest checkpoint, or from byte 0, so a capped trace answered from
   it numbers every line it returns. A number rx does report is
   identical either way.

   An index is used only when it still describes the file it was built
   from. That means the format version matches exactly — an index from
   another version is treated as absent, not read with today's rules —
   and every identity field it carries still matches: size, mtime, inode,
   ctime, and a digest of the size plus the first and last 64 KiB. The
   one case this does not catch is an edit confined to the middle of a
   large file that also preserves the byte count and the mtime, on a
   filesystem whose ctime does not move. Catching that needs a
   whole-file hash, which costs more than rebuilding the index.

   The trace cache is held to the same rule through the same check,
   `index.SourceIdentity`. Its identity is taken before the scan's
   chunks are planned, never after the scan, so a log that grows during
   a trace is never cached as larger than the part that was read.

   Bump `index.Version` whenever the on-disk shape or the meaning of a
   field changes. rx-python shares the cache directory and treats an index
   of another version as absent, so the bump also gets a
   `../tickets/PARITY-DEBT.md` row.
3. **Bounded reads.** No code path reads more bytes than the request needs,
   except an index build (`rx index`, and the index a samples lookup
   builds first), `rx trace` without `--max-results`, and `rx compress`.
   An HTTP request never builds an index inside itself: `GET /v1/samples`
   starts or joins a background `index` task, one per file and file
   identity (`internal/webapi/samples_index.go`); a request with
   `Prefer: respond-async` waits up to `RX_SAMPLES_WAIT_SECONDS` and
   answers `202` with the task after that, any other waits for the build.
   Every new file-reading path gets a budget test that uses
   `counting.InjectOpen` and asserts the byte count.
4. **Cache cross-compatibility with Python.** Keep every `IndexAnalysis` field.
   Never add `omitempty` to a schema-documented wire field; use explicit nulls.
   Go-only extensions go under `go_extras`.
5. **Freeze-barrier registry.** `internal/analyzer/registry.go` freezes before
   the server starts; later registration panics; readers are lock-free. Do not
   add a mutex. A detector registers a factory with `RegisterLineDetector`,
   the only registration call, so every detector listed is one that runs
   and each build gets fresh detector state.
6. **Sandbox on every path.** Every HTTP handler and every CLI command that
   receives a path, including output paths, calls
   `paths.ValidatePathWithinRoots` before touching the filesystem.
7. **SSRF defence stays layered.** `internal/hooks/config.go` rejects loopback,
   link-local, RFC 1918, CGNAT, multicast and unspecified addresses, and
   resolves DNS at validation time. Do not weaken it. Redirects must be refused
   or re-validated.
8. **Metrics are off by default.** Wrap every new metric call behind the
   enable gate. Never use `r.URL.Path` as a label; use the chi route pattern.
9. **Detached goroutines recover.** Any `go func()` spawned from a handler goes
   through `internal/webapi/run_detached.go::runDetached`.
10. **Exit codes are part of the CLI contract.** 0 success, 1 generic error,
   2 usage error, 3 file not found, 4 access denied, 5 interrupted. Scripts
   branch on them, so a change is breaking.

## Coding standards

- Follow the repo's existing style; `gofmt -s` and `goimports` clean; lint clean.
- **US spelling in Go code**, comments included: the `misspell` linter is
  a `just ci` gate, so "colour", "behaviour" and "unrecognised" fail the
  build. Prose in `docs/` and `CHANGELOG.md` is not linted and may use
  either.
- `context.Context` first on any function that does I/O; errors wrapped with
  `%w` when the wrap adds information; godoc on every exported symbol.
- Comments are more generous than typical Go, on purpose: explain the Go idiom,
  the goroutine lifecycle and the invariant. Label invariants (`INVARIANT:`,
  `SECURITY:`). Do not reference review rounds, stages, plans or ticket numbers
  in code comments; describe what the code guarantees instead. `just
  scaffolding-check` is a CI gate that fails on `Stage N`, `Round N` and
  `Finding N` in either case and with or without `#`, and on
  `Reviewer N`, `Rn-Xn` and `user decision N.N` (any case). In `docs/`
  it fails on `Stage N`, `Round N`, `Finding N` as whole words and on
  `Rn-Xn`; everywhere it ships it fails on ticket numbers.
- Prefer a lookup table over a chain of `if`/`switch` when the logic is a mapping.
- Long flags use `=` in help text, docs and printed commands.
- Small functions, early returns, no flag parameters that switch behaviour.

## Testing

- Table-driven tests for three or more similar cases. Golden files in
  `testdata/` for CLI output and the OpenAPI spec. Integration tests in
  `internal/webapi/*_integration_test.go` use `httptest` and a real viewer
  tarball fixture (`internal/webapi/testdata/rx-viewer-v0.2.0-dist.tar.gz`).
- `internal/webapi/openapi_conformance_test.go` calls every operation over
  the real router and validates each answer against the golden OpenAPI
  document's schema for its status (JSON Schema 2020-12, test-only
  dependency `santhosh-tekuri/jsonschema`). It fails on an undeclared
  status and on an operation no call reaches: a new operation or error
  status gets a call there.
- Byte-budget tests for anything that reads files. Binary-level tests for exit
  codes and `--help` (run the built binary, assert `$?`).
- `t.TempDir()` and `t.Setenv()`; `t.Parallel()` and `t.Setenv()` are mutually
  exclusive.
- No test touches the user's real cache. A package whose code can reach
  `~/.cache/rx` (index, trace cache, frontend, or a binary that does) has a
  `main_test.go` whose `TestMain` calls `isolatedcache.Main(m)`, which points
  `RX_CACHE_DIR` at a temporary directory. The `just` test recipes run under
  a throwaway `HOME` (`scripts/test-isolated-home.sh`) and fail when a cache
  file appears there, so a package that misses this turns `just ci` red.
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

- **Scope: internal use on a trusted network. Not for internet exposure.**
  There is no authentication, no TLS and no multi-tenancy, by design. The
  operator builds the perimeter — loopback, VPN, SSH tunnel or an
  authenticating reverse proxy. Do not design identity, RBAC, sessions or
  certificate handling into rx; say it is out of scope and move on.
  What *is* in scope is hygiene inside that perimeter: sandbox
  containment, SSRF defence, safe extraction, and an opt-in viewer token.
- **Hidden entries are not served by default.** A path component starting
  with a dot is refused unless `--hidden` / `RX_HIDDEN=true` is set, the
  way ripgrep skips them. The rule lives in the path validator, not only
  in the directory walkers: hiding an entry from a listing does nothing
  about a caller who knows the path. Components of a `--search-root`
  itself are exempt. The error message is part of the contract.
- **A directory walk reads only what naming the path would allow.**
  Every walk of a user's directory goes through `paths.WalkDir` (or
  `paths.ListDir` for a one-level listing such as `/v1/tree`): a
  symlink is resolved and its target checked like a named path, and a
  refused one is reported with a reason, never read. Each directory is
  searched once per walk, which bounds the work by the number of real
  directories. Do not add a walk that calls `os.ReadDir` and opens what
  it finds.
- **A file is read only through its pin.** `paths.Pin` checks a path
  and records the device and inode of the file it leads to;
  `Pinned.Open` refuses the file when the path leads elsewhere by the
  time of the read. The trace engine, `samples`, `index.Build` and
  `compressfile` read a user's file only through a `paths.Pinned`. Do
  not add an `os.Open` of a user's path in them; take the pin the walk
  or the caller made, or pin the path yourself. A reader that needs
  the file more than once opens the pin once and passes the handle
  (`index.Build` hands its `*os.File` to `seekableindex`, the
  fingerprint and format detection). `Pin` records an identity only
  for a file it reaches from the root without passing through a link;
  keep it that way.
- `serve` binds `127.0.0.1:7777` by default. Anyone who can reach the socket
  can run any operation inside the sandbox.
- User regex patterns are always passed to rg as `-e <pattern>` so a leading
  dash cannot become a flag. Keep it that way.
- The viewer bundle is downloaded from GitHub Releases; verify its checksum
  when the sidecar exists.
- Webhook targets are checked against internal address ranges twice: when the
  URL is validated, and again in the HTTP client's dialer against the literal
  IP it is about to connect to. The second check is the one that survives DNS
  rebinding — do not remove it.
- Threat model and defences: `docs/concepts/security.md`. Keep it current when
  you change a defence.

## Gotchas

- `os.File.ReadAt` is goroutine-safe; `Read` is not.
- ripgrep puts no limit on one JSON event — a matched line of any
  length, plus about 50 bytes per submatch — and neither
  `bufio.Scanner` (64 KB) nor `encoding/json` (a string is one token,
  even for `json.Decoder.Token`) can read one in bounded memory. Every
  rg output goes through `StreamEvents` (`rgjson.go`, scanner in
  `rgscan.go`), which keeps at most `RX_MAX_LINE_TEXT_BYTES` of a line
  and `RX_MAX_SUBMATCHES_PER_LINE` submatches and marks the rest as
  truncated. Do not add a path that buffers rg's output or decodes an
  event whole; read a user's line with `readBoundedLine`, not
  `ReadBytes`. The chunker's newline lookahead is 256 KB; lines longer
  than that can split a chunk mid-line.
- Never `Wait` on an `rg` whose stdout nobody reads: a reader that stops
  early must kill rg first, or rg blocks on the full pipe and the
  `Wait` never returns. `ProcessChunk`, `ProcessCompressed` and
  `scanFrameBatch` kill rg on every early stop.
- `filepath.Join("/a", "/etc/passwd")` returns `/a/etc/passwd`; check tar
  symlink targets with `filepath.IsAbs` directly.
- Python's `isoformat()` drops `.000000` when microseconds are zero and writes
  local time; `formatMtime` in `internal/index/store.go` matches that.
- Python's `json.dumps(sort_keys=True)` emits `", "` and `": "`; the patterns
  hash in `internal/trace/cache.go` is built byte by byte to match.
- Go map iteration is random: sort keys before any output that must be stable.
- huma v2 forbids `*int` on query params; use sentinels (`0` unset MaxResults,
  `-1` unset Context). huma serves `/openapi.json` as `application/openapi+json`.
- huma panics on `nullable:"true"` for a field that refers to a named object;
  make the object itself nullable with a `_ struct{} \`nullable:"true"\``
  field (`rxtypes.LineLengthStats`). A wire type whose JSON is not what
  reflection sees (`rxtypes.LineIndexEntry`, `rxtypes.TaskResult`) gets its
  schema from an alias in `internal/webapi/openapi_schemas.go`, since
  `pkg/rxtypes` imports the standard library only. Error statuses are
  declared through `errorResponses`; `error_statuses_test.go` fails when a
  handler can return one its operation does not declare.
- rg's `absolute_offset` is relative to rg's stdin; add `chunk.Offset`.
- `klauspost/compress` zstd encoder levels are coarse (1 to 4); higher values clamp.
- `sync.Mutex` + map beats `sync.Map` for check-and-insert (see `internal/tasks`).

## Open work

Audit tickets live in `../tickets/` (outside this repo). Read
`../tickets/README.md` before taking one. Do not create a "known issues" list
in this file; file a ticket.

## What NOT to do

- Do not open a difference from `rx-python` without a `../tickets/PARITY-DEBT.md` row.
- Do not change a `pkg/rxtypes` field without the golden spec and the viewer's generated types.
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
