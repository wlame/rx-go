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
| `rx-viewer` | Shared Svelte SPA, fetched from GitHub Releases by `serve` (the newest release inside the supported range, checked once a day) and served from `~/.cache/rx/frontend/` |
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
| `cmd/rx/main.go` | Entry point. `preprocessArgs` routes a bare pattern to `trace`; a new subcommand joins `knownSubcommands` there |
| `internal/clicommand/` | One file per subcommand: `trace`, `index`, `samples`, `time-range`, `compress`, `serve`; `logs` and its subcommands (`list`, `show`, `time-range`, `index`, `samples`, `trace`) in `logs*.go` |
| `internal/webapi/` | HTTP layer (chi + huma v2), middleware, OpenAPI, SPA fallback, `runDetached` |
| `internal/trace/` | Search engine: `chunker.go`, `worker.go` (rg subprocess), `seekable.go`, `compressed.go`, `cache.go` |
| `internal/samples/` | Line and byte-offset resolver shared by CLI `samples` and `/v1/samples` |
| `internal/logchain/` | Log chains (rotated logs read as one text): the name-template table (`Templates`), a directory listing grouped into chains (`Group`, `Resolve`), a chain described (`Describe`: time order, checks, state, fingerprint, global starts; a memory cache), samples across parts (`Samples`) and the chain search (`Search`). Every read of a part goes through the per-file code (`samples`, `index`, `trace`) and the part's pin |
| `internal/index/` | Line-offset index builder, stats (Welford + reservoir), on-disk store |
| `internal/seekable/`, `internal/compression/` | Seekable-zstd codec; magic-byte format signatures; pooled decoders |
| `internal/filekind/` | The one classifier every command and route uses: format by magic bytes, seekable by seek table, text or not (with the reason) |
| `internal/seekableindex/` | Frame → line-range index for a seekable `.zst`; the format rx-python defined |
| `internal/analyzer/` | Detector registry (Freeze barrier) and 9 detectors under `detectors/` |
| `internal/hooks/` | Webhook dispatcher with SSRF defence |
| `internal/paths/` | `--search-root` sandbox |
| `internal/frontend/` | Downloads and extracts the viewer bundle |
| `internal/tasks/` | In-memory background task manager for `POST /v1/index`, `/v1/compress`, the index builds `GET /v1/samples` waits for, and the index task of each log chain (`chain_index`, `internal/webapi/chain_index.go`): a lock key apart from the path it shows (`CreateKeyed`, the chain directory's device and inode with its name), part builds as subtasks whose finished entries are capped apart from other tasks (`CreateSubtask`), and a `Watch` that keeps how a task ended; a task's done channel and progress |
| `internal/prometheus/` | Metrics behind an `atomic.Bool` enable gate (off in CLI, on in `serve`) |
| `internal/output/` | Shared human-readable formatting |
| `internal/testparity/` | Harness that runs `../rx-python` and diffs output |
| `internal/testutil/counting/` | Byte-counting readers for bounded-read tests: `OpenCounting` (a file), `NewReader`, `NewReaderAt` |
| `pkg/rxtypes/` | Wire types. OpenAPI source of truth |
| `docs/` | 44 MkDocs pages, built strict in CI, deployed to GitHub Pages |

## Build, run, test

`just` is the entrypoint. `just --list` shows every recipe.

```bash
just build                                       # static binary into dist/
just run trace "error" /var/log/app.log          # run from source
just serve --port=8080 --search-root=/var/log    # 8080 matches the viewer dev proxy

just test                                        # full suite
just test -run='TestTrace|TestIndex'             # each argument reaches go test as one word
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

A recipe that forwards its arguments carries `[positional-arguments]` and
passes them as `"$@"` (or `"$1"`), never as `{{args}}`, which the shell
splits again at spaces and at `|`. The same holds for every value that
does not come from the justfile's own text. just pastes a `{{…}}` value
into the recipe line before the shell reads it, so a quote, `;`, `$(` or
backtick in the value runs as code. The version from `git describe` holds
a tag name, which may contain all of them: the recipes read it as
`"$BUILD_VERSION"`, which the justfile exports. Only string literals of
the justfile (`test_timeout`, `coverage_min`) are pasted, each as a bare
`{{name}}`. `cmd/rx/justfile_args_test.go` fails on any other `{{…}}`:
a parameter, a variable just computes, and any expression that is not
one variable — a backtick, a function such as `env_var`, a `+`, an
`if` — even when its only variables are literals.
`cmd/rx/justfile_release_test.go` runs `release-notes`, `version`,
`build` and `build-all` on crafted tag names.

Go 1.25+ is required (`go.mod` says `go 1.25.0`; huma v2 needs it); CI runs
1.25 and 1.26. `golangci-lint` v2.x and `govulncheck` are needed for `just
lint` and `just vuln` — install them with `go install`. `just docs-build`
needs `uv`.

### Benchmarks

`just bench` runs every benchmark once and is not a CI gate; flags pass
through (`just bench -bench=Index` runs the index build's). To show what
a change costs, run the benchmarks it touches ten times before it and
ten times after it, and let benchstat compare the two runs:

```bash
go test ./internal/index/ -run='^$' -bench=Index -count=10 > /tmp/before.txt
# make the change, then:
go test ./internal/index/ -run='^$' -bench=Index -count=10 > /tmp/after.txt
go run golang.org/x/perf/cmd/benchstat@latest /tmp/before.txt /tmp/after.txt
```

A difference counts when benchstat gives it a small `p` (below 0.05).
The index build's benchmarks (`internal/index/timeindex_bench_test.go`)
report `ns/line` on generated lines whose timestamps rise, repeat and
step back as in a log that many threads write, from a seeded generator:
a constant timestamp would hide the cost of a branch on the running
maximum, which a branch predictor learns at once on such input. Keep
the per-line path measured that way.

## Architecture

```
CLI (cobra)                HTTP (chi + huma v2)
    │                           │
internal/clicommand/     internal/webapi/
    └──────────┬────────────────┘
               │
       internal/logchain/  log chains: parts grouped by name, numbered as one text
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

Data flow for `rx logs samples /var/log/syslog --lines=G`:

1. `clicommand/logs_samples.go` calls `logchain.DescribeHandle`:
   `Resolve` pins the directory, lists it once and groups the files
   matching the chain name by the template table; `Describe` reads each
   frozen part's line index (the CLI indexes a part without one in
   memory), orders the parts by first timestamp, runs the checks and
   computes each part's global start.
2. `logchain.Samples` finds the part holding G by a binary search on the
   starts, cuts the window into one piece per part it touches, and reads
   each part once through `samples.Resolve` with the part's pin.
3. The pieces are printed with global and local line numbers;
   `GET /v1/logs/samples` returns the same pieces as JSON.

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
   `relative_line_number` beside it carries the same number. The
   fields that describe how an answer was produced, not the answer, are
   outside the rule: `request_id`, `time`, `cli_command` and
   `file_chunks`. The chunk settings are not part of the trace-cache
   key, so a cache hit reports the chunk count of the scan that wrote
   the entry, which may differ from what a scan with today's settings
   would use. Tests compare trace answers through
   `internal/testutil/traceanswer`: `RequireAgree` applies this rule,
   and `RequireSame` is its strict form for two scans that ran to the
   end, where neither answer has a `-1`. Both leave out the four fields
   above; a test about one of them checks it on its own. A capped
   search of a chunked plain file keeps whichever matches its workers
   found first, so two capped runs can hold different matches; a test
   that compares capped answers uses a layout where the matches kept
   are fixed.

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
   and every identity field it carries still matches: size, mtime,
   inode, device, ctime, and a digest of the size plus the first and
   last 64 KiB. Times are compared as nanoseconds since the Unix epoch
   (`source_mtime_ns`, `source_ctime_ns`), never as the local-time text
   `source_modified_at` / `source_changed_at`, which changes with `TZ`. The
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
3. **A log chain answers as the concatenation of its parts.** A chain
   (`internal/logchain`) is the files of one rotated log, ordered by
   time and numbered as one text. Decompress its parts in the chain's
   order into one file, with a line break added after a part that lacks
   one: every chain answer equals the single-file answer on that file —
   the lines and `line_timestamps` of `rx logs samples`, the line a time
   query finds, the line counts and the first and last times, and the
   matches and line numbers of an uncapped `rx logs trace` — apart from
   saying which part a line comes from, and apart from two documented
   cases where `line_timestamps` is `null` ("not computed") and the
   concatenation has a value (`docs/api/endpoints/logs-samples.md`).
   Part arithmetic agrees: global line `G` is line
   `G − global_start + 1` of its part, where `global_start` is 1 plus
   the line counts of the parts before it, by the parts' indexes and by
   `wc -l` of the decompressed parts. Cold and indexed answers are equal
   under the `-1` rule of contract 2; global numbers exist only once the
   chain is `ready` and are `-1` before.
   Year-less timestamps take their year from each part's own mtime, so
   a year-less chain is compared with its concatenation part by part.

   A chain adds no reader of its own: every read of a part goes through
   the per-file code (`samples.Resolve`, `samples.TimeRange`, the line
   index, the trace engine) with the pin its directory listing made
   (`Part.File`); an index build of a part pins the part's path anew,
   and the index it gives is held to the listing's pin
   (`index.DescribesPinned`). The backend keeps no state between
   requests: a chain is found again from the disk each time, the
   description cache only makes it faster, and the fingerprint lets a
   client notice a rotation (409, exit 7). Tests build the parts and
   their concatenation in `t.TempDir()` and compare, cold and after
   indexing; a change to a chain answer gets such a test.
   `docs/concepts/log-chains.md` states the rules; its template table is
   checked against `logchain.Templates` by
   `internal/logchain/concept_page_test.go`, and every `rx logs` example
   in the docs runs in `cmd/rx/logs_doc_examples_test.go`.
4. **Bounded reads.** No code path reads more bytes than the request needs,
   except an index build (`rx index`, and the index a samples lookup
   builds first when the head of the file cannot answer it), `rx trace`
   without `--max-results`, and `rx compress`. A samples lookup on a file
   that wants an index and has none first tries the head
   (`samples.ResolveFromHead`): at most `RX_SAMPLES_HEAD_MB` of text per
   pass, every reader of the text stopped at the head, and the answer
   is the one without an index. `rx samples` answered that way builds
   no index; `GET /v1/samples` starts or joins the background build
   without waiting for it and names its task in `index_build`. The
   running build keeps how far the head reaches (`samples.HeadReach`,
   measured by the first lookup that runs past the head), and a lookup
   of a line or an offset past it reads nothing before it waits.
   An HTTP request never builds an index inside itself: `GET /v1/samples`
   starts or joins a background `index` task, one per file and file
   identity (`internal/webapi/samples_index.go`); a request with
   `Prefer: respond-async` waits up to `RX_SAMPLES_WAIT_SECONDS` and
   answers `202` with the task after that, any other waits for the build.
   At most `RX_MAX_INDEX_BUILDS` of these builds run at once; later ones
   wait as `queued` tasks in a queue of at most 256, with no goroutine
   until a slot frees. `POST /v1/index` builds are outside the limit.

   The log chain routes keep the same rules. `GET /v1/logs/chain` reads
   each frozen part's stored index and the head and tail of the active
   file, never a whole part; a pending chain starts or joins the chain's
   one index task (`internal/webapi/chain_index.go`, operation
   `chain_index`, its lock keyed by the directory's device and inode and
   the chain name), whose part builds go through the same queue and
   share `RX_MAX_INDEX_BUILDS`: a task submits at most that many parts at
   once, the part builds of all chains together take at most half of
   the queue, and each part build is a subtask whose finished entries
   are capped apart from other tasks. At most as many chain tasks run
   or wait as that half has places (128): past it a pending chain gets
   no task (`index_build` null, 503 from `POST /v1/logs/index` and from
   a samples request that needs the chain ready). A chain task that
   finds no room waits in line, and each build's end wakes one of them
   (`samplesIndexBuilds.awaitChainRoom`). The task table's cap counts
   finished tasks only, so tasks that run or wait never push one out. `GET /v1/logs/samples` reads each
   part a window touches once per request through the `GET /v1/samples`
   path, with the answer's limits summed over every piece; time bounds
   are searched in one forward pass, at most two reads per part.
   `GET /v1/logs/trace` and `rx logs trace` describe chains from stored
   indexes only, so a capped chain search reads what `rx trace` reads.
   `rx logs show`, `time-range` and `samples` index a part without a
   current index in memory (an index build) and store nothing;
   `rx logs index` builds and stores every part's.

   Every new file-reading path gets a budget test that counts what it
   reads and asserts the count: `internal/testutil/counting` wraps a
   file (`OpenCounting`) or a reader (`NewReader`, `NewReaderAt`) with a
   byte counter, and `internal/logchain`'s read seams (`loadPartIndex`,
   `openPart`, `buildPartIndex`, `readTimeRange`; see
   `describe_reads_test.go`) count the reads of each part.
5. **Cache cross-compatibility with Python.** Keep every `IndexAnalysis` field.
   Never add `omitempty` to a schema-documented wire field; use explicit nulls.
   Go-only extensions go under `go_extras`.
6. **Freeze-barrier registry.** `internal/analyzer/registry.go` freezes before
   the server starts; later registration panics; readers are lock-free. Do not
   add a mutex. A detector registers a factory with `RegisterLineDetector`,
   the only registration call, so every detector listed is one that runs
   and each build gets fresh detector state.
7. **Sandbox on every path.** Every HTTP handler and every CLI command that
   receives a path, including output paths, calls
   `paths.ValidatePathWithinRoots` before touching the filesystem.
8. **SSRF defence stays layered.** `internal/hooks/config.go` rejects loopback,
   link-local, RFC 1918, CGNAT, multicast and unspecified addresses, and
   resolves DNS at validation time. Do not weaken it. Redirects must be refused
   or re-validated.
9. **Metrics are off by default.** Wrap every new metric call behind the
   enable gate. Never use `r.URL.Path` as a label; use the chi route pattern.
10. **Detached goroutines recover.** Any `go func()` spawned from a handler goes
   through `internal/webapi/run_detached.go::runDetached`.
11. **Exit codes are part of the CLI contract.** 0 success, 1 generic error,
   2 usage error, 3 file not found (for `rx logs`, also a handle that
   names no chain), 4 access denied, 5 interrupted, 6 the log chain is
   invalid, 7 the log chain's files changed since `--fingerprint=`.
   Scripts branch on them, so a change is breaking.

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
- No test sends data to a hook of the developer's shell. The `just` test
  recipes drop every `RX_HOOK_*` variable from the environment, and the
  tests in `cmd/rx` start the binary through `rxCommand`, whose
  environment leaves them out too (`testEnviron`). `isolatedcache.Main`,
  the parity runners and `testEnviron` drop the same variables: one
  filter in `isolatedcache` decides them, reached from other packages
  through `WithoutHookVariables`. A test that needs a hook sets its own
  URL.
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
  `release.yml`, which builds the binaries and their sha256 sidecars. Its
  first step stops the job unless the tag is exactly `vX.Y.Z`, and
  `just release-notes` accepts only `X.Y.Z`. The job builds with
  `just --set version "$GITHUB_REF_NAME" build-all`, so the binaries
  stamp the pushed tag and not what `git describe` prefers (an annotated
  tag on the same commit), and it fails unless `rx --version` prints
  exactly `rx version <tag>`.
  `cmd/rx/justfile_release_test.go` holds the justfile to that `--set`.
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
  keep it that way. A line index loaded for a pinned file goes through
  `index.LoadForPinned` (or `DescribesPinned`), which drops an index of
  another inode. `internal/logchain` reads each part of a chain through
  the pin its directory listing made (`Part.File`; an index build of a
  part pins its path anew and is held to that pin), and takes `part=` /
  `--part=` only as the bare name of a member, never joined to a path.
- **A file rx writes for a user goes through its directory's root.**
  `compressfile` opens the output's directory with `paths.OpenDir`
  (reached from the search root without passing a link) and creates,
  renames, links and removes only through that `*os.Root`: a temporary
  file made with `O_EXCL`, then a rename (`--force`) or a hard link
  (without it) to the output name. Do not add a write that opens the
  output by its path.
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
  `ReadBytes`. The chunker searches forward for a newline however far
  away it is (`chunkStarts`), so a chunk never starts inside a line;
  keep the skip that stops a long line from being read once per
  boundary.
- Never `Wait` on an `rg` whose stdout nobody reads: a reader that stops
  early must kill rg first, or rg blocks on the full pipe and the
  `Wait` never returns. `ProcessChunk`, `ProcessCompressed` and
  `scanFrameBatch` kill rg on every early stop.
- `filepath.Join("/a", "/etc/passwd")` returns `/a/etc/passwd`; check tar
  symlink targets with `filepath.IsAbs` directly.
- Python's `isoformat()` drops `.000000` when microseconds are zero and writes
  local time; `formatMtime` in `internal/index/store.go` matches that. The
  text is for reading only: an identity check that compared it would
  call every entry stale after a `TZ` change.
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
- Every rg search takes its base arguments from `newRgArgs`
  (`worker.go`), and they include `--text` and `--encoding=none`.
  `filekind` is the one place that decides what a file is (`Of` for a
  listing, which probes with at most a 16 MiB window; `OfForReading`
  for a command that reads the file next, which probes with the
  128 MiB it reads with) — its
  format from the magic bytes (never the name), seekable from the seek
  table, text against binary from a NUL byte in the first 8 KiB of its
  text (decompressed for a compressed file) — and every command and
  route asks it, through the pin; without `--text`, rg on stdin turns each later NUL byte into a line
  break, which numbers the lines after it too high and splits the NUL
  line. Without `--encoding=none`, rg strips a UTF-8 byte-order mark at
  the start of each input (a chunk, a stream, a batch of frames) and
  counts every offset after it 3 bytes short. Add a flag every search
  needs there.
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
