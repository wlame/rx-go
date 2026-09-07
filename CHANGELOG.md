# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `--search-root` is now a persistent flag: every subcommand accepts it,
  it repeats to name several roots, and `RX_SEARCH_ROOTS` is its
  environment form. Until now the sandbox could only be switched on by
  `rx serve`, so `rx compress in.log --output=/etc/cron.d/x` could not be
  confined by any flag and the security document described a control the
  binary did not offer. A path outside every root exits 4; a root that
  does not exist is a usage error rather than a silent "no sandbox". With
  neither the flag nor the variable set nothing changes. `rx serve` keeps
  its own flag and now falls back to `RX_SEARCH_ROOTS` before the current
  directory.

### Fixed

- `rx index` indexed binary files that rx-python skips, so the same
  directory produced two different sets of indexes. It now applies the
  same rule rx-python does — a NUL byte in the first 8 KiB means binary —
  which also makes the summary line "below threshold or not text" true.
- `POST /v1/index` with `analyze: true` refused a file below the size
  threshold with 400, while `rx index --analyze` indexes it. Analysis now
  bypasses the threshold on both surfaces.
- `rx samples` accepts `--line-offset` and `--byte-offset`, the names
  rx-python uses, alongside `--lines` and `--offsets`.

### Changed

- The five hand-rolled checkpoint lookups now call the binary search in
  the index package, which was tested but unused. One implementation
  instead of five, and O(log n) instead of a linear scan per lookup.

- A truncated or corrupted archive was searched as far as it could be
  read and the result was handed back as if it were complete. The
  decompression error was logged and then discarded, so a half-readable
  `.gz` reported its partial matches with nothing skipped. Such a file
  is now named in `skipped_files` while its matches are still returned,
  so the answer says both what was found and that the file was not read
  to the end.

- An index left behind by an older rx was read with today's rules. A
  version 2 index names the line *before* the byte offset it records,
  so `rx samples --lines=2500000` on a 226 MB log answered with line
  2,500,001. The index cache is shared with rx-python, which has always
  refused a version it does not know; rx-go checked nothing. Any index
  whose version is not the current one is now treated as absent, so it
  is rebuilt instead of trusted.
- A file rewritten in place kept a valid-looking index. Size and mtime
  are all rx compared, and neither moves when a copy is restored with
  `cp -p` or when an edit swaps one byte for another — turning a space
  into a newline near the top of a 226 MB log shifted every line number
  by one while `rx samples` kept answering from the old checkpoints. An
  index now also records the source inode, its ctime, and a digest of
  the size plus the first and last 64 KiB, and every one of those it
  carries must still match before it is used. The index format version
  moves to 4 in both backends together.

- `GET /v1/samples` with several byte offsets read the whole file once
  per offset, and again to collect each window: twenty offsets on a
  multi-gigabyte log meant more than twenty passes. That is how the
  viewer resolves the line numbers of a capped search's matches. The
  offsets are now answered in one pass that keeps the last few lines in
  hand for the windows that reach backwards — twenty offsets on a
  100 KB fixture read 100 KB, where they used to read 1.2 MB.
- A match reported a different line number depending on how the file was
  stored. A search of a `.gz` called the line unknown even though
  ripgrep had read the whole file in one pass, and a search of a
  seekable `.zst` reported the line's position inside its frame — line
  24,014 of a 600 MB log came back as 407. Both now report the line's
  place in the file: the frames count their newlines as they are
  decompressed for the scan, which is what places each frame in the
  file, and a frame the scan never reached leaves its matches unnumbered
  rather than numbered from the wrong place. The same log stored plain,
  gzipped and as seekable zstd now answers identically, match for match.
- `rx samples --lines=N` on a compressed file reports the byte offset of
  that line in the decompressed stream instead of -1. That is the
  coordinate system a search reports its matches in, so the two surfaces
  name the same byte for the same line.
- The unified index format is version 3, matching rx-python, where the
  checkpoints in a compressed file's index used to name the line before
  the one that starts at the recorded offset. Indexes written earlier
  are rebuilt on first use.
- `rx serve` on a fresh machine installed no viewer at all: rx-viewer
  v0.3.0 was published on 2026-09-03 while both backends only accepted
  `0.2.0 <= v < 0.3.0`, so the bundle was refused and `/` fell back to
  the API docs. The window now reaches `< 0.4.0`, and rx-viewer's own
  release checklist requires both backends to accept a minor before it
  ships.
- The scan summary called the number of pieces a file was split into a
  worker count, so a 137-frame archive reported "Parallel workers: 137"
  on a six-core machine. It reads "Parallel chunks" now, in both
  backends.
- A pattern ripgrep refuses is reported as the caller typed it:
  `invalid regex pattern "(bad": unclosed group` rather than a complaint
  about `(?:(bad)`, which is the alternation ripgrep wraps the patterns
  in and a group the caller never opened.
- A file named on the command line that cannot be read reported "Files
  skipped: 1", no matches and exit 0 — a search that claimed success
  without reading anything. It now exits 4 (access denied) with the path
  in the message. An unreadable file inside a directory being scanned is
  still skipped. A response with nothing to search also carries its
  request id and the paths it was given, instead of an empty id and a
  null path.
- `cat file | rx pattern -` exited 1 without a message and
  `rx pattern < file` returned an empty result: stdin was never
  implemented, only rejected, and the rejection was swallowed. Piped
  input is now spooled to a temporary file and searched, the way
  rx-python does it, so byte offsets, context lines and `--samples` all
  work on it; the file is removed when the search ends. An explicit `-`
  with empty input searches nothing rather than falling back to the
  current directory.
- `rx samples file.gz --lines=100` printed raw compressed bytes. The
  decompressing path existed only inside the HTTP handler, so the CLI
  sent a compressed file down the plain-file reader; the same command
  against `GET /v1/samples` answered correctly. Both now call one
  resolver, which streams the file through its decompressor and returns
  the same lines the plain file would. Byte offsets on a compressed
  file are refused on both surfaces, with exit code 2 on the CLI and
  400 over HTTP.
- `rx index file.gz` built a line index over the compressed bytes and
  reported statistics about them: a 600 MB log came back as 209,365
  lines with a "mixed" line ending, and the checkpoints addressed
  compressed noise. A compressed file is now read through its
  decompressor, so the line numbers, offsets and statistics describe the
  text inside it, and the index records the compression format and the
  decompressed size. Both backends now report the same line count for
  the same file. A seekable `.zst` still gets byte-step checkpoints
  rather than rx-python's frame table (ticket 23).
- `--max-results` waited for a whole chunk to finish before it counted,
  so a cap could not stop a scan any earlier than the first chunk's
  completion: `--max-results=1` took 7.8 seconds on an 8.2 GB log and
  6.4 seconds on a 6.7 GB one, where ripgrep's own `-m1` returns at
  once. Every worker now charges a shared budget as each match arrives,
  and the match that spends the last of it cancels its siblings
  immediately. The same two searches take 0.05 seconds, and the counts a
  cap returns are unchanged.
- A search of a seekable-zstd file never returned when anything stopped
  it early: `--max-results` on a 55 MB `.zst` hung indefinitely, and the
  same request over HTTP held the handler open and blocked shutdown. The
  frames are fed to ripgrep through a pipe whose only reader is
  ripgrep's own stdin, so once ripgrep was killed the writer blocked on
  a write nobody would ever read. The reader is now closed before the
  scan waits for the writer, and a ripgrep killed by a cap is read as
  the cancellation it is rather than as a crash that made the file
  unreadable — the capped search returns exactly the requested number of
  matches in about half a second.
- A trace cache hit returned the wrong lines and took minutes. The
  cache stores a byte offset and a line number per match; the line
  number was the one counted inside a chunk, and rebuilding read "line
  N" by scanning from the start of the file once per match — 334
  seconds and the wrong text for 7,734 matches on a 487 MB log, and the
  same wrong text when rx-python read the cache rx-go had written.
  Rebuilding now makes one pass in offset order, so the byte offset
  addresses the line and the line number is counted along the way: the
  same search takes 1.7 seconds and returns exactly what a fresh scan
  returns. Caches written by earlier versions are discarded rather than
  read back wrong (trace cache version 3, matched in rx-python). A
  cache hit on a compressed file rebuilds through its decompressor,
  which had been reading the compressed bytes as if they were text.
- Building the context windows compared every match against every
  context line, which is quadratic on a large result set; the lines are
  now looked up by number.
- Line numbers on a file large enough to be split across chunks were
  the line's position inside its chunk, not inside the file: a match on
  line 255,437 of a 487 MB log was reported as line 30,627, and
  `absolute_line_number` was the -1 "unknown" marker. The workers now
  count newlines in the bytes they already stream to ripgrep, which
  gives every chunk its first line number and every match its real one
  at no extra I/O. When a `--max-results` cap cancels a chunk part-way
  the count cannot continue, and those matches keep the unknown marker
  and resolve against the file's index when one exists — rather than
  reporting a chunk-relative number as if it were a file line. The
  human output prints `?` for a line number that stayed unknown.

## [0.2.0] - 2026-09-03

### Added

- A test pins the `rx serve` bind address (`127.0.0.1:7777`), which is
  now also rx-python's default. The two backends answer on the same
  address, so swapping one for the other needs no URL change.

### Changed

- **Breaking:** files and directories whose name starts with a dot are no
  longer served by default. `rx serve` defaults `--search-root` to the
  current directory, so started from a home directory it used to serve
  `~/.ssh`, `~/.aws` and `~/.gnupg` through `/v1/samples` to anyone who
  could reach the port. `--hidden`, or `RX_HIDDEN=true`, restores the old
  behaviour; a hidden component of a `--search-root` itself is exempt.
  The rule matches ripgrep's and is enforced in the path validator as
  well as in directory listings, so a hidden file is refused when asked
  for by name and not merely omitted from the tree.

- The documentation now states the intended use plainly: rx is for
  internal use on a trusted network and is not intended to be exposed to
  the internet. `serve` has no authentication by design; the operator
  builds the perimeter. Added to the README, and for rx-go to the docs
  home page and the security concepts page, which also gained the third
  validation layer the dial-time webhook check introduced and lost a
  reference to an `error_type` label that is never emitted
  (`permission_denied`; the real one is `access_denied`).

### Fixed

- Seven metric families were declared and registered but never
  incremented, so they never appeared in a scrape:
  `rx_trace_requests_total`, `rx_samples_requests_total`,
  `rx_analyze_requests_total`, `rx_trace_duration_seconds`,
  `rx_errors_total` and `rx_hook_call_duration_seconds`. They are now
  recorded, with the same names, labels and `status` values rx-python
  uses, so a dashboard works against either backend. Request counters
  are incremented from a single deferred site per handler, so a request
  is counted exactly once however it exits.
- `rx_large_file_threshold_mb` and `rx_ripgrep_processing_seconds`, both
  of which rx-python exposes. The first publishes the chunking threshold
  a scrape needs to read `rx_parallel_tasks_created`; the second
  separates the ripgrep subprocess cost from the rest of a request.

- A pattern the regex engine cannot compile is now counted as
  `rx_errors_total{error_type="invalid_regex"}` rather than
  `invalid_params`, matching rx-python.

### Security

- Webhook targets are now checked at dial time, not only when the hook
  is configured. `ValidateURL` resolves the hostname and rejects internal
  addresses, but the HTTP client resolved that name again when the
  request went out, and nothing made the two answers agree — a name that
  resolved to a public address at configuration time could resolve to
  `127.0.0.1` by request time, whether through DNS rebinding or a
  short-TTL record that simply changed. The client's dialer now applies
  the same address policy to the literal IP it is about to connect to,
  which is the point with no window left. `RX_ALLOW_INTERNAL_HOOKS` is
  honored, so an operator who deliberately targets a local collector is
  unaffected.

## [0.1.0] - 2026-09-03

### Added

- `GET /health` reports `contract_version`, the HTTP wire contract this
  backend speaks (`1.0`). rx-python reports the same value and rx-viewer
  refuses a contract major it was not built for.
- The golden OpenAPI document is published at `docs/api/openapi.json`, and
  `just spec-check` — part of `just ci` — fails when it is stale, so the
  spec a client generates from is always the one the tests pin.
- The `/v1/detectors` schemas describe every field, so a client can render
  a detector set it has never seen without hardcoding names.

- `justfile` as the single dev entrypoint. `just ci` runs the gates in the
  order CI runs them — `fmt-check vet lint tidy-check test-race docs-build`
  — and `.github/workflows/ci.yml` invokes `just ci` rather than repeating
  the commands, so the two cannot drift apart. `just cover` enforces an 80%
  coverage floor (82.4% today).
- `.github/workflows/release.yml`: a `vX.Y.Z` tag builds static binaries for
  linux/amd64, linux/arm64, darwin/arm64 and darwin/amd64 with sha256
  sidecars, checks that `rx --version` reports the tag, and creates the
  GitHub Release from the changelog section.
- `scripts/release.sh`, driven by `just release` and `just release-dry`. It
  requires a clean tree on `main`, refuses to release an empty
  `[Unreleased]`, promotes the changelog, commits and tags — and prints the
  push commands instead of running them.
- `LICENSE` (MIT). The README referenced one that did not exist.
- Dependabot for GitHub Actions and Go modules, weekly.

### Changed

- `TraceResponse`'s `matches`, `path`, `scanned_files` and `skipped_files`
  are no longer nullable in the spec. The engine passes every one through
  `emptyIfNil*`, so they are always arrays on the wire; huma had inferred
  `null` from the Go nil slice and generated clients carried a null case
  that cannot happen.
- The parity harness fails loudly when `RX_PYTHON_PATH` is set but the venv
  is missing, instead of returning the skip sentinel. Someone who set the
  variable asked for the comparison; skipping it made the harness report
  success while comparing nothing.

- `go.mod` and `go.sum` are tidy. Five direct dependencies (huma, chi, uuid,
  cobra, xz) were sitting in the `// indirect` block, and `go mod tidy
  -diff` is now a CI gate.
- The Makefile is gone; its `VERSION ?= 0.1.0-dev` default meant a build
  could claim a version that was never tagged.
- `mkdocs.yml` excludes `docs/plans/`, which is gitignored working material
  and failed the strict docs build with a git-revision warning.

### Fixed

- `golangci-lint` findings: an `exitAfterDefer` in `main`, a shadowed error
  in the trace command, and four US-spelling misspellings. The gosec
  path-traversal report on the static file handler is annotated with the
  validator it cannot see through.

### Security

- The viewer bundle is verified against the release's `dist.tar.gz.sha256`
  sidecar before it is unpacked. A digest that does not match is refused
  and the cached bundle is left alone; a missing sidecar is accepted with
  a warning, for releases published before sidecars existed. The verified
  digest is recorded in `.metadata.json`.
- A bundle is unpacked into a staging directory and only swapped in once
  it is known good, so a failed download no longer destroys the working
  viewer. The cache used to be cleared before extraction.
- `rx serve` installs the newest viewer release only when its version is
  inside the range this backend was built against (`0.2.0 <= v < 0.3.0`).
  A newer release is logged and skipped instead of being served to the
  browser; `RX_FRONTEND_VERSION` and `RX_FRONTEND_URL` override it.

- Webhook dispatch no longer follows redirects. A hook target that
  answers `3xx` cannot steer the request at a host that never passed
  validation; the hop is logged and the hook counts as a failure.
- The viewer-bundle downloader re-checks every redirect hop against the
  same address policy, so a redirect to a loopback, link-local, private
  or CGNAT address aborts the download.
- Hook URLs carrying credentials (`http://user:pass@host/`) are
  rejected, in the HTTP layer and on the command line.
- `rx serve` refuses to start when `RX_HOOK_ON_FILE_URL`,
  `RX_HOOK_ON_MATCH_URL` or `RX_HOOK_ON_COMPLETE_URL` points somewhere
  the SSRF guard blocks.
- `rx trace --hook-on-file/--hook-on-match/--hook-on-complete` validate
  their URLs with the same guard the HTTP layer uses.

### Changed

- `rx trace` human output now matches rx-python byte for byte: a header
  block, then `file:line:offset [pattern]` per match. The matched line
  text is no longer printed inline — ask for a context window to see it.
- `rx index` prints the statistics and anomaly counts rx-python prints,
  and its summary line is `Indexed N files in Ts` rather than
  `index built for N files`.
- `rx compress --build-index` no longer claims to build an index it never
  built: it reports `index_error` in `--json` and a warning on stderr.

### Added

- `rx trace` renders context lines. `--before`, `--after` and `--context`
  each work on their own; `--samples` is a shorthand for the default
  window of 3. Overlapping windows merge so no line is printed twice,
  non-contiguous regions are separated by `--`, and each line is marked
  `:` for a match or `-` for context.
- `rx trace` fills `request_id` and `path` in its response. Both were
  empty on the command line, so `--json` emitted `"path": null`.

### Fixed

- Every CLI failure exited 1, ignoring the documented table. `rx` now
  exits 2 for a usage error, 3 for a missing file, 4 for a path outside
  `--search-root` and 5 on SIGINT or SIGTERM, as `docs/cli/index.md` has
  always claimed. The 26 call sites that discarded `exitWithError`'s
  return value now return it, and `main` unwraps the code.
- A pattern ripgrep cannot compile is no longer swallowed. `rx trace 'a('`
  reported success with the file listed under `skipped_files`; it now
  prints ripgrep's message and exits 2, and `GET /v1/trace` answers 400
  instead of 200 with an empty result.
- The compress output path is now validated against the search roots.
  `POST /v1/compress` answers `403` for an `output_path` outside
  `--search-root`, including one reached through a symlink and including
  `force=true`, and `rx compress` refuses `--output` / `--output-dir`
  destinations outside the roots.
- The compress task now reports `compression_ratio` as
  `decompressed / compressed`, the direction its own API documentation and
  both CLIs already use.
