# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `GET /v1/trace` takes the remaining options of `rx trace` as query
  parameters: `context`, `before_context` and `after_context` (the
  `--context`, `--before` and `--after` window, resolved the same way:
  a given `before_context` or `after_context` wins over `context`, `0`
  included, and `-1` means "take `context`"), and `no_cache`,
  `no_index` and `no_recursive`. An answer equals the one
  `rx trace --json` gives with the same flags, so `context_lines`,
  `before_context` and `after_context` now carry the window over HTTP
  too, and `cli_command` renders every one of them. Each context count
  is capped at 100 lines per side; above it the request is a `422`, and
  the OpenAPI document declares the bound. The contract version is now
  1.4.

- `GET /v1/tasks/{task_id}` reports `progress`: the share of an index
  task's input read so far, from 0 to 1, or `null` for a task that does
  not report it (a compress task, or an index task that reused a stored
  index). Part of contract 1.4.

### Changed

- `rx samples --lines` and `GET /v1/samples?lines=` on a gzip, bzip2, xz
  or plain zstd file stop decompressing after the last wanted line,
  instead of reading the stream to its end for every request. With an
  index, a line counted from the end (`-1`) takes the line count from
  it and costs one pass, not two, and the pass starts counting lines at
  the checkpoint before the first wanted line. Answers are unchanged.

- `GET /v1/samples` no longer builds a file's line index inside the
  request. The build runs as a background `index` task, visible at
  `GET /v1/tasks/{task_id}`, and every request for the same file, in
  the same state, waits for that one build instead of starting its own;
  a running `POST /v1/index` task for the file is waited for as well. A
  request waits up to `RX_SAMPLES_WAIT_SECONDS` (default 5): when the
  build ends in time it answers `200` as before, otherwise `202` with
  the task (`task_id`, `status`, `message`, `path`, `started_at`), and
  the client polls the task and asks again. A client that disconnects
  stops waiting, not the build. `rx samples` still waits for the build
  however long it takes. Part of contract 1.4.

- The docs state the one way a line index, the trace cache or
  `--no-index` may change a trace answer: a line number that is `-1`
  without them may be the true line number with them. Every other field
  is equal, `-1` means "not computed", and a number rx fills in is the
  line holding the match's offset. Behavior is unchanged; see
  `docs/concepts/caching.md`, `docs/cli/trace.md` and
  `docs/api/endpoints/trace.md`.

- `rx compress` and `POST /v1/compress` without an output name drop the
  input's compression suffix: `app.log.gz` is written to `app.log.zst`,
  not `app.log.gz.zst`, since the output holds the text. The same holds
  for `.gzip`, `.bz2`, `.bzip2`, `.xz`, `.zst` and `.zstd`, in any case,
  and with `--output-dir`; any other name still gets `.zst` appended. An
  output that exists is refused without `--force` (`"force": true`) as
  before. A plain zstd input named `app.log.zst` would be its own
  output and is refused, `--force` or not, now with a hint: `the output
  path is the input file (use --output to name another file)`, over HTTP
  `(set "output_path" to another file)`. The `output_path` description in
  the OpenAPI document says so; the contract stays 1.3.

## [0.3.0] - 2026-10-03

### Added

- `rx samples --offsets` and `GET /v1/samples?offsets=` answer for a
  gzip, bzip2, xz, zstd or seekable zstd file. An offset is a position
  in the decompressed text, the coordinate `rx trace` reports, and the
  answer is the line the plain copy of the file gives for it, so
  `--lines=N` and `--offsets=` the offset of line N lead to each other
  on every format. Both used to refuse the request (exit 2, `400`),
  which left a `-1` line from a capped trace of a compressed file with
  no way to resolve it. An indexed seekable zstd file decompresses only
  the frames around each offset; the other formats decompress from the
  first byte up to the last offset asked about. A frame table whose
  line numbers do not add up to the file's line count is not used.
  Additive; the contract stays 1.3.

- A capped `rx trace` of a compressed file numbers the matches its scan
  left at `-1` the way it does for a plain file: through the file's
  index (for a seekable zstd file, from the frame before the match),
  or under `--no-index` by counting the decompressed text from its
  first byte. Without an index they stay `-1`.

- A test parses every `rx` command in the README and the docs with the
  real command tree, so a wrong flag, a bad value or a long flag
  written without `=` fails the build instead of a reader's shell.

- `/metrics` reports the standard Go runtime (`go_*`) and process
  (`process_*`) families beside the `rx_*` ones. They are read when
  `/metrics` is scraped and cost nothing in between.

- The index answers have named schemas in the OpenAPI document, so a
  client can generate types for them instead of writing its own:
  `IndexResponse` for `GET /v1/index`, `IndexTaskResult` and
  `CompressTaskResult` for the `result` of a finished index or compress
  task (which the document now declares as one of the two, or `null`),
  and `LineIndexEntry` for a `line_index` entry: `[line_number,
  byte_offset]`, or `[line_number, byte_offset, frame_index]` in a
  seekable-zstd index. They were free-form objects. The answers keep
  their shape, with three small exceptions where a key used to be left
  out or empty: an empty `line_index` is `[]` rather than `null`, an
  analyzed index without a longest-line position answers
  `"longest_line": null`, and a compress result always carries
  `index_error`, `null` unless building the index failed. A test
  validates real answers, a seekable-zstd index included, against the
  schemas. Additive; the contract stays 1.3.

- A `409` from `POST /v1/index` or `POST /v1/compress` names the task
  already running for the path in a `task_id` member beside `detail`,
  so a client can poll that task without reading the ID out of the
  sentence; the sentence is unchanged. The OpenAPI document declares
  the body as `TaskConflictError`. Additive; the contract stays 1.3.

- `GET /v1/trace` takes ripgrep's matching flags as five boolean query
  parameters: `ignore_case`, `word_regexp`, `line_regexp`,
  `fixed_strings` and `pcre2`. An answer is the one `rx trace` gives
  with `--ignore-case` and the rest, the trace cache keys the request by
  them, and the response's `cli_command` carries them. The CLI and the
  API read one table of the five, so a flag cannot reach one surface and
  not the other. Look-around and backreferences were unreachable over
  HTTP until now. Contract version 1.3.

- `RX_API_TOKEN`: an opt-in shared secret for the API. When it is set,
  every `/v1` request must send `Authorization: Bearer <token>`, compared
  in constant time; others get `401` with a `WWW-Authenticate: Bearer`
  challenge and the usual `{"detail": ...}` body. `/health`, `/metrics`,
  `/docs`, `/openapi.json` and the viewer's files stay open. One value
  for every caller — not an identity system — and it crosses plain HTTP
  in clear text, which the docs say. The OpenAPI document declares it as
  an optional `bearerAuth` scheme with a `401` on every `/v1` operation.
  Contract version 1.2.

- `rx serve` warns on stderr at startup when it listens where other
  machines can reach it (any `--host` that is not a loopback address or
  `localhost`), and when a search root is `/` or the home directory.
  rx has no authentication by design; the warning names the remedies —
  `RX_API_TOKEN` or an authenticating proxy — and the server still
  starts, since a VPN or a proxy makes a wide bind legitimate. With a
  token set, it says instead that the token crosses plain HTTP in clear
  text.

- A test says stdout carries nothing but the JSON document whenever
  `--json` is passed — for every subcommand that takes the flag, and for
  a plain file, a gzip member and a seekable zstd. rx-python printed a
  progress note next to its JSON writer with nothing in the code saying
  the stream was reserved; rx-go never did, and now cannot start.

- Security response headers on every route: `X-Frame-Options: DENY`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and
  a `Content-Security-Policy` of `frame-ancestors 'none'`. The viewer
  renders untrusted content by definition, and a browser ignores these
  three in the meta tag the SPA carries, so they have to come from the
  server. The header CSP carries only what a meta tag cannot; the full
  policy stays in the meta tag, which is the artifact that knows what
  Monaco needs. rx-python sends the same table.

- The line-length percentile definition is documented in
  `docs/concepts/analyzers.md`: linear interpolation over the sorted
  sample, the sample standard deviation, exact below 10,000 lines and
  within 2% above it, where rx-go samples. A shared fixture asserts the
  same numbers in both repos.

- `rx compress --build-index` builds the index it always promised. The
  flag defaults to true in both backends; here it reported
  `index_error: "not implemented in this backend"`, so a `.zst` written
  by rx-go had no index and every lookup in it walked the stream, while
  the same file written by rx-python was indexed. `POST /v1/compress`
  said `index_built: true` without building one, which was worse — it
  told the caller something that was not so.

- `rx index` and `POST /v1/index` index a seekable `.zst` by its frames
  rather than skipping it. The index records which lines each frame
  holds, and it is the file rx-python writes for the same input: same
  fields, same checkpoints, same frame table, so either backend reads
  the other's.

- `rx samples --lines=N` on a seekable `.zst` with an index decompresses
  the frame holding the line and the one before it, rather than the whole
  archive. Two frames whatever the file's size. Without an index it still
  streams — an index only ever makes the answer faster.

- An index now records `permissions` and `owner`, which rx-python has
  always recorded and rx-go left null.

- The 403 body for a path outside every `--search-root` is published as
  `SandboxError` in the OpenAPI document, and every path-accepting route
  declares the response. The shape itself is unchanged; it was only ever
  implemented, never described, so a client could not generate a type for
  it. rx-python now returns the same five fields instead of a single
  prose `detail`, which is what makes one error panel work against both
  backends. Contract version 1.1.

### Changed

- `rx serve` installs viewer releases from 0.2.0 up to, not including,
  0.5.0 (it was 0.4.0), so viewer 0.4.0, the one that matches this
  backend's contract 1.3, is installed automatically.

- `rx index` says why it skipped each file. The human output lists every
  skipped file with its reason under `Skipped N files:` instead of the
  single line `Skipped N files (below threshold or not text)`, and
  `--json` adds `skip_reasons`, a list of `{"path", "reason"}` in the
  order of `skipped`, which stays a list of paths. The reasons use the
  words of `POST /v1/index`'s `400`: `file size N bytes is below
  threshold M bytes` and `not a text file` (a `.tar.gz`, for instance).

- A trace with `--max-results=N` (or `max_results`) answered from the
  trace cache rebuilds only the first N matches and stops reading the
  file after them and the lines their context reaches. It rebuilt every
  cached match and read the file up to the last one first. The answer
  is unchanged. On a 465 MB log, `WARN --max-results=100` from the
  cache went from 0.22 s to 0.03 s; the whole entry is still parsed,
  so a capped hit on a very large entry stays slower than a capped
  scan (0.69 s from a 119 MB entry against 16 ms).

- `just scaffolding-check` also refuses review citations in `docs/`:
  `Stage N`, `Round N` and `Finding N` as whole words (so "around 256"
  passes) and `Rn-Xn` labels. In Go comments it now catches
  `USER DECISION N.N` in capitals too.

- `GET /health` reports under `constants` only the settings rx reads:
  `LOG_LEVEL`, `MAX_SUBPROCESSES`, `MIN_CHUNK_SIZE_MB` and `CACHE_DIR`.
  `DEBUG_MODE`, `LINE_SIZE_ASSUMPTION_KB`, `MAX_FILES` and
  `NEWLINE_SYMBOL` are gone: the variables behind them (`RX_DEBUG`,
  `RX_DEBUG_DIR`, `RX_MAX_LINE_SIZE_KB`, `RX_MAX_FILES`,
  `NEWLINE_SYMBOL`) were read only to be reported there. `environment`
  no longer echoes `NEWLINE_SYMBOL`; it still echoes every `RX_*`
  variable as it is set.

- `rx trace --debug` is hidden from help and prints a deprecation note
  on stderr. It never did anything; it stays accepted so a script that
  passes it keeps working.

- The Go package `pkg/rxtypes` no longer has `TraceRequest`. Nothing
  used it: `GET /v1/trace` reads query parameters, and `rx trace` builds
  its options directly. The wire contract is unchanged.

- The analyzers page describes the one way to add a detector,
  `analyzer.RegisterLineDetector`. It named `analyzer.Register`, whose
  detectors were listed by `GET /v1/detectors` and never run. That call
  is gone, along with the empty `Analyze` and `Supports` methods every
  detector carried and the per-analyzer cache directory nothing wrote;
  the caching page no longer lists `analyzers/`.

- `cli_command` follows one rule for every operation: a flag appears
  when the request gave a value and that value is not what `rx` uses
  without the flag. A value equal to the CLI default is left out, so a
  compress task that took the defaults renders `rx compress PATH`
  instead of `... --output=PATH.zst --frame-size=4M --level=3`, and
  `--output` appears only when the request named an output. The
  rendering comes from one table of request fields and flags per
  operation; a test compares the table's defaults with the real command
  tree and runs the commands of real trace and samples requests, whose
  `--json` answer must equal the HTTP answer.

- `rx trace` refuses a flag it does not know, with exit 2 and the
  flag's name, instead of skipping it. Skipping is what let `-i` and
  `-w` give wrong answers, and the forwarding it imitated would also
  pass ripgrep flags that run a program (`--pre`) or change the output
  rx parses (`--count`). A script that passed another ripgrep flag now
  fails loudly; the five matching flags under Fixed are the supported set.

- `GET /v1/tree` renders `modified_at` as RFC 3339 in UTC with exactly
  six fractional digits — `2026-09-06T00:53:24.438322Z`. It used to be
  `time.RFC3339Nano`, which reports nanoseconds and trims trailing
  zeros, so a file whose mtime landed on a whole second rendered
  `…:24Z` while its neighbour rendered `…:24.438322650Z`, and neither
  matched rx-python's `2026-09-06T00:53:24.438323` — a naive local time
  with no timezone at all.

  Six digits because Python's `datetime` holds microseconds and no
  finer. Both backends truncate rather than round, which is what put
  them one microsecond apart on the same file once the shape agreed.
  `/v1/tree` is now byte-identical between them.

  `source_modified_at` and `created_at` in an index are unchanged: they
  are cache format, both backends already write the same naive local
  form, and changing them would invalidate every index on disk.

- `cli_command` renders the command rx-python renders. The two
  backends produced two different strings for the same request — rx-go
  put the path last and wrote `--lines 2`, rx-python put the path first
  and wrote `-l 2` — so what the viewer showed a user depended on which
  backend the operator installed, and one of the two taught the space
  form of a long flag that the project's own docs were corrected away
  from.

  The rendering is now `rx <subcommand> <positionals...>
  <--long=value...>`, with the value quoted after the `=` when it needs
  it. `GET /v1/index` also gains `--info --json`, without which the
  rendered command builds an index instead of reading one.

  With this, `GET /v1/samples` on the same file is byte-identical
  between the two backends. The cases live in
  `testdata/cli-commands.json`, which rx-python holds a copy of at
  `tests/data/cli-commands.json`; a change made on one side and not the
  other fails on the other.

- Response bodies no longer carry a `$schema` field. huma's default
  configuration installs a link transformer that adds one to every body;
  rx-python emits no such key, it was declared nowhere in `pkg/rxtypes`,
  and a strict decoder — `DisallowUnknownFields`, a pydantic model with
  `extra='forbid'` — rejected the whole document over it. Dropping the
  transformer removes the field from the OpenAPI schemas as well, so the
  viewer's generated types no longer declare it.

  With this and the trailing newline gone, `GET /v1/index` on an
  unindexed file and the 403 sandbox envelope are byte-identical between
  the two backends, and `GET /v1/samples` differs only in
  `cli_command`.

  The `Link` header the same transformer emitted goes with it; nothing
  consumed it. `/schemas/*.json` still serves.

- `rx index --threshold=0` indexes every file instead of falling back
  to `RX_LARGE_FILE_MB`. Zero meant "use the env default" on this
  surface and "no threshold" on `POST /v1/index` and in rx-python, so
  the same number meant two things inside one backend, and a script
  asking to index everything got an empty `indexed` list and exit 0 —
  a silence that reads like "there was nothing to do". "Use the
  default" is now spelled by leaving the flag off, which is what an
  absent flag already meant.

- Response bodies no longer end with a newline. Go's
  `json.Encoder.Encode` terminates every value with one and huma's
  default JSON format uses an Encoder, so no rx-go response was ever
  byte-identical to rx-python's, on any route. No parser could tell the
  difference — but that is exactly the problem: every cross-backend check
  had to decode both sides before it could compare, which cannot see a
  key order change or a number rendered as `1.0` against `1`. The 403
  sandbox envelope is now the first route whose raw bytes match on both
  backends.

  `Content-Length` shrinks by one byte per response. Both backends have
  a test that pins the framing.

- The `Error:` line capitalizes its first letter. Go error strings are
  lower case by convention and the wrapped error keeps that form — it is
  what `errors.Is` callers and the HTTP layer see — but what a person
  reads after "Error: " is a sentence, and rx-python capitalizes it. The
  two backends printed the same message in two different cases for the
  same mistake; every `rx trace` error path is now byte-identical
  between them.

- `rx samples` and `GET /v1/samples` build a line index when the file is
  large or compressed and none is cached, so the next lookup in the same
  file is fast. rx-python has always done this and rx-go did not, so the
  same command left different state on disk and the first call cost very
  different amounts of time. The build runs without analysis: nothing on
  this path reads its output. `--no-index` on `rx samples`, and
  `RX_NO_INDEX=1` on either surface, turn it off — the lookup then
  streams, which is slower and gives the same answer.

- Comments no longer cite the review process that produced the code. 174
  references to stages, rounds, reviewers, findings and numbered
  decisions are gone from 63 files, along with two test files named after
  a finding. The plan documents they pointed at do not exist in any
  repository, so `// See Stage 8 Reviewer 2 High #6.` read as if there
  were somewhere to look. Where a citation carried a real constraint the
  constraint was written out; where it was only a citation the line was
  reconstructed or removed. Comments only: no identifier changed and no
  line of code moved.

- `just scaffolding-check` is a new CI gate that fails when such a
  citation reappears.

- `analyze_window_lines` in a `POST /v1/index` body is now an explicit
  optional integer rather than an int that omitempty hid: absent, null
  and 0 all mean "use the default", and the field appears in the schema.
  A negative value is refused with 400 before a task is created, rather
  than accepted and then failing a task the client is polling.
  `--analyze-window-lines=-5` is likewise exit 2. rx-python accepts the
  same field and the same flag now, so the contract test no longer needs
  to tolerate the difference.

- `rx samples --offsets` and `--lines` repeat as well as taking a
  comma-separated list, so `-b 100 -b 200` names both positions. It used
  to keep only the last value, which is the quiet kind of wrong: the
  command succeeded and answered a question nobody asked. rx-python
  accepts both spellings too now.

- `rx samples --color` accepts `auto` as well as the empty string, and
  refuses anything else with exit 2 instead of silently falling back to
  auto detection. rx-python's `--color` now takes the same three values,
  so the flag means one thing across the two backends.

- Colour auto-detection treats a writer that is not a terminal as "no
  colour", where it used to emit sequences when it could not tell. That
  only showed up for a non-file writer, which in practice means a test,
  but the rule now says what the documentation always claimed and what
  rx-python does.

- The `roots` list in a sandbox refusal is sorted. The operator's flag
  order is not something a client should have to know about, and sorting
  is what lets the two backends return identical bodies for the same
  configuration.

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
  the same file.
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

### Fixed

- `rx compress` and `POST /v1/compress` write the text of a compressed
  input. A gzip, bzip2, xz or plain zstd file used to be encoded as its
  compressed bytes, so a trace of the output searched those bytes and
  its line numbers and offsets meant nothing. Now the input is
  decompressed on the fly and streamed into the encoder, a trace of the
  output equals a trace of the decompressed file, and
  `decompressed_size` is the size of the text. Both surfaces go through
  one function and refuse the same inputs, nothing written: a compound
  archive such as `.tar.gz` (`compound archives (tar.gz, etc.) are not
  supported`), a file that is already seekable zstd unless `--force` /
  `"force": true` asks to re-encode it with the new frame size and
  level, and an output path that is the input file, which used to be
  truncated before it was read. The HTTP API refuses them with `400`
  before it creates a task. A corrupt or truncated input fails with the
  decoder's error and leaves no partial output.

- `rx compress --workers=N` holds one batch of N frames in memory
  instead of the whole input; the output is unchanged.

- `rx trace --before/--after` context windows hold only the lines next
  to their match in the file. In a capped trace, a line the scan could
  not number kept the number ripgrep gave it inside its chunk or frame,
  and windows looked their lines up by number, so a window could hold a
  line from another part of the file: on a 465 MB log, the window of
  line 9 held lines from bytes 48757329 and 317021164, and up to 153 of
  500 windows were wrong. Windows are now put together by
  byte offset, from the line that ends where a line starts and the line
  that starts where it ends. A cold scan also lost the part of a window
  that lay in the next chunk of a plain file or the next batch of
  frames of a seekable zstd file, while the trace cache had it; each
  worker now hands ripgrep the `--before` lines before its range and the
  `--after` lines after it, so a scan, a cache hit and `--no-index`
  answer the same. A seekable batch that finished as the cap fired
  could keep a match without the lines after it.

- The context section of `rx trace --samples` prints only lines whose
  number in the file is known. A line a capped scan could not number
  was printed at the number ripgrep gave it inside its chunk, on
  another line's place, and a match without a number marked such a
  line as a match.

- A trace capped by `--max-results` gives the matches it keeps the
  lines after them that `--after` asks for, as the trace without the
  cap does. On a plain file the match that reached the cap stopped
  ripgrep before it wrote those lines (`-A 2 --max-results=3` gave the
  third match no line after it), and so could the matches of other
  chunks stopped at the same moment. On a gzip, bzip2, xz or zstd file,
  and on a seekable zstd file, a match cut by the cap was dropped from
  the window of the last match kept, together with the lines after it.
  A match is now counted against the cap once its window is read, and
  a match read only to complete a window, or cut by the cap, is a line
  of the windows around it.

- `rx samples --lines` and `GET /v1/samples?lines=` on a gzip, bzip2,
  xz or zstd file, or on a seekable zstd file without an index, answer
  a line asked for twice once, as for a plain file: `--lines=2,2`,
  `--lines=5-7,5-7`, or `N` with the negative position that names the
  same line. Every line of the window came back once per position.

- `rx trace` of a seekable zstd file whose frames do not end at line
  breaks matches every line whole. Each frame was scanned on its own,
  so a line that a frame boundary cut was matched as two fragments:
  `line_text` and `submatches` held a fragment at the frame's first
  byte, a match the boundary cut in two was lost, and a pattern
  anchored at a line start could match a fragment. On a 465 MB log cut
  into 64 KB frames, 12 of 4338 matches came back as fragments. Batches
  of frames now meet at line breaks: a worker skips the end of the line
  its batch begins inside and reads on into the next frames to finish
  its own last line. Such files are now scanned 100 frames per worker,
  as `rx compress` output always was, instead of one frame at a time,
  which takes a full trace of that log from 9.6 s to 0.3 s. `rx
  compress` output is scanned as fast as before.

- A trace answered from the trace cache gives the lines around a match
  their byte offset, `absolute_offset` in `context_lines`, as the scan
  that filled the cache does. They came back as `-1`.

- `rx trace --json` gives each match the window `--before` and
  `--after` ask for, each on its own: `-B 12 -A 1` gave a match up to 12
  lines after it, because the window used the larger of the two on both
  sides and took the lines a neighbouring match's leading context had
  brought in. A line in the window that matches too is now part of it;
  it was left out, so the windows of neighbouring matches had holes.
  The human output, which merges the windows, is unchanged.

- Seekable zstd files whose frames do not end at line breaks (written
  by another encoder, which may cut a frame mid-line or inside a line
  longer than a frame) are numbered as their text. The index counted a
  frame holding no line break as holding one line, so every later
  frame, and every `rx samples --lines` answer after it, was one line
  too high per such frame; on a 465 MB log cut into 64 KB frames the
  last frame claimed line 1439124 of 1436842. A line longer than a
  frame also came back cut short. A full `rx trace` of such a file
  left every match after the first such frame at `-1`, and the trace
  cache stored them under their line number inside their frame. The
  index format is now version 6 and the trace cache version 5, so
  indexes and caches written before are rebuilt; `rx compress` output
  was never affected.

- `rx samples --lines=-1` and `GET /v1/samples?lines=-1` on a gzip,
  bzip2, xz or zstd file (and on a seekable zstd file without an
  index) name the last line when the text does not end with a line
  break. The count from the end skipped that line, so `-1` answered
  the line before it and every negative line was one too low.

- `rx samples --offsets` fails with the error when the file cannot be
  read part-way through, instead of answering the offsets after the
  failure as past the end of the file (`-1`).

- `rx index --analyze` and `POST /v1/index` with `"analyze": true`
  analyse a seekable `.zst`. They answered `analysis_performed: false`
  with no line-length statistics and no anomalies, and built the index
  again on every request, since the cached one never had the analysis
  asked for. The decompressed text now goes through the same detectors
  as a plain file's, in the pass that reads the frames, so the analysis
  equals that of the decompressed copy, line numbers and byte offsets
  included. gzip, bzip2, xz and plain zstd were already analysed; a
  compressed tar archive is still refused as not text (`400` over HTTP,
  `skipped` in the CLI).

- A `409` from `POST /v1/index` or `POST /v1/compress` names the
  operation of the task that holds the path. An index request refused
  because a compress of the file runs said "Indexing already in
  progress"; it now says "Compression already in progress", the
  operation `task_id` points at.

- Two traces that write the same trace cache entry at once no longer
  share one temporary file: each writer gets its own (`.tmp-<random>`
  beside the entry), so the entry left behind is one writer's whole
  answer and neither write fails on a rename. An entry that cannot be
  parsed is still treated as absent, and is now logged at Warn level
  as `trace_cache_unreadable` with its path.

- The `match_found` webhook sends `line_number` as the response's
  `absolute_line_number`: the line's number in the file, or `-1` where
  a scan cut short by `max_results` could not count it, always with the
  byte `offset`. It used to send a number counted from the start of the
  chunk for such a match. The events now go out once the trace has
  numbered and capped its matches, one per match of the response, so a
  capped trace no longer reports a match the response leaves out, and a
  match an index or `--no-index` numbers carries that number.

- The performance figures in the docs come from runs of the current
  binary on real logs (465 MB and 6.3 GB) instead of an older build on
  another host, and the bounded-read table names its exceptions: the
  first `samples` lookup in a large or compressed file reads the whole
  file to build an index, and a line lookup in a gzip, bzip2, xz or
  plain zstd file streams the whole file every time. Corrected on the
  way: an index build keeps only its checkpoints (22 MB RSS for a
  6.3 GB log, not "about the file size"), `--analyze` costs about 130
  times a plain build (not 2-4 times), and trace memory grows with the
  match count. The performance page no longer cites the review that
  found earlier regressions.

- The caching and line-index docs describe the files rx writes. The
  trace cache lives under `trace_cache/<patterns_hash>/`; its key has no
  `--max-results` (a capped request is answered from a full entry);
  writes use a temporary file and a rename with no `fsync`; the mtime is
  compared exactly; and an index is valid only at the current format
  version (5) with its size, mtime, inode, ctime and fingerprint
  unchanged. The sample index is a real version-5 one with `[line,
  offset]` checkpoints 1 MB apart, and an index build keeps only the
  checkpoints in memory (about 22 MB RSS for a 6.3 GB log). `serve`
  runs one task per path whatever the operation, so an index task
  refuses a compress of the same file with `409` and the other way
  round.

- The trace and samples docs describe the answers rx gives. HTTP trace
  takes no context window, so `context_lines` holds each match's own
  line and `before_context`/`after_context` are `null` there.
  `scanned_files` lists only files found by walking a directory.
  `relative_line_number` is chunk-relative when `absolute_line_number`
  is `-1`. The `X-Request-ID` header is not the body's `request_id`, and
  webhooks carry the latter. In `samples`, a missing position is `null`
  (not an empty array), `offsets` maps an offset to a line number, a
  range in `lines` maps to `-1`, `before_context` and `after_context`
  echo the request while each window is clamped at both ends of the
  file, and the first lookup in a large or compressed file builds a full
  index inside the request. The timings quoted come from a real 465 MB
  log.

- The compression docs match the encoder and the offsets rx reports.
  A byte offset rx reports for a compressed file is a position in the
  decompressed text (the docs said compressed bytes); `samples
  --offsets` refuses compressed files because it cannot seek there.
  `--level` has four encoder settings (1, 2-5, 6-9, 10-22), not 22
  distinct levels, and the sizes and times quoted come from a real
  465 MB log; a seekable file is not "4-5% larger" than plain zstd
  but 15% at the default frame size there. A seekable `.zst` is traced
  frame-parallel. `rx compress` does not decompress a compressed input,
  and its `--json` entries carry no `cli_command`.

- The configuration page lists what rx reads: it adds `RX_NO_INDEX`,
  `RX_ANALYZE_WINDOW_LINES` and the global `--hidden` and
  `--search-root` flags, drops the variables nothing reads, and gives
  the real defaults of `RX_WORKERS` and `RX_MIN_CHUNK_SIZE_MB` (a file
  below twice the chunk size is one chunk; the chunking page said 20 MB
  was enough for two). The troubleshooting page no longer offers a
  debug mode.

- The README, the docs home, the installation page and the quickstart
  describe the current binary. The README's `rx samples … -C 3` is
  `--context=3`; it no longer lists `RX_NO_CACHE`, which nothing reads,
  names `RX_SEARCH_ROOTS` with its real defaults, lists
  `POST /v1/compress` and `GET /v1/detectors`, and its Development
  section uses `just` (there is no Makefile). The install examples fetch
  the release binaries as they are published (`rx-<os>-<arch>` plus a
  `.sha256`) instead of a `v0.1.0` or `2.2.1-go` archive, and the
  version is the release tag. The quickstart's outputs come from a real
  run.

- `rx_large_file_threshold_mb` reports `RX_LARGE_FILE_MB`, the threshold
  its name says. It reported the chunk size, `RX_MIN_CHUNK_SIZE_MB`.

- A scan task stopped by a `max_results` cap no longer counts in
  `rx_worker_tasks_failed_total`. Every capped trace of a chunked file
  added the chunks it canceled there. A canceled task now counts as
  neither completed nor failed.

- An index of a log that grows while it is built covers exactly the
  size it records. The builder stated the file, then read to whatever
  end the file had by then, so the index described bytes past its
  recorded size and took its fingerprint after the read. It now takes
  the identity first and reads no further than that size; the next use
  sees the larger file and rebuilds, as before.

- A seekable-zstd trace cache records `frames_with_matches` and each
  match's `frame_index`, as rx-python's does. Both were always empty, so
  the cache could not say which frames to decompress again.

- The background task table of `serve` is capped at 256 tasks. A
  finished index task keeps its whole result, line index included, for
  `RX_TASK_TTL_MINUTES`, and the number of tasks had no limit, so a
  burst of requests could hold any amount of memory for an hour. Past
  the cap, starting a task drops the oldest finished ones; running and
  queued tasks are never dropped. The result keeps `line_index`, which
  the viewer reads to tell an index result from a compress result.

- A trace cache that cannot be written (a full disk, a cache directory
  that cannot be created) is logged once per process as a
  `trace_cache_write_failed` warning. The failure was dropped, so the
  cache could stay off without anyone knowing. Traces still answer;
  they are only not cached.

- A seekable-zstd scan whose ripgrep output cannot be read to the end
  (a line longer than the 16 MB parse buffer) lists the file under
  `skipped_files` instead of returning the matches before that point
  as if they were all.

- `GET /v1/samples` answers on a server without `ripgrep`. It returned
  `503`, though it reads the file itself and never runs `rg`; `rx
  samples` already worked without it. The OpenAPI document no longer
  declares a `503` for the operation.

- A cached analysis is reused only for a request it answers. `rx index
  --analyze` and `POST /v1/index` with `analyze: true` reused any
  analyzed index, whatever `analyze_window_lines` it ran with and
  whichever detectors were registered then, so a new window or an
  upgraded detector had no effect until `--force`. An analyzed index
  now records `analysis_window_lines` and `analysis_detector_set`
  (every detector as `name@version`), and a request with another window
  or another detector set rebuilds it. The index format version is 5;
  an index of version 4, which rx-python still writes, is treated as
  absent and rebuilt.

- `rx trace --no-index` no longer reads the line index. A scan cut
  short by `--max-results` used to number its unnumbered matches from
  the index even under the flag. Under `--no-index` those lines are now
  counted from the start of the file, up to the last such match, so the
  answer is the one an index gives and no index file is read or
  written. Without the flag, nothing changes: an index numbers them,
  and without one they stay `-1`.

- `rx index` and `rx compress` exit 3 when a file they were given does
  not exist, as `rx trace` and `rx samples` do; they exited 1. Both
  still go through every path and report each failure in their output.
  The exit code is 3 when every failure was a missing file, 4 when
  every failure was a path outside the search roots, and 1 when the
  failures were of different kinds.

- `rx_http_responses_total` counts each response once, with the status
  the client received. The trace handler counted its own responses as
  well, so one `GET /v1/trace` with an invalid pattern added both a
  `400` and a `500` sample. A request no route matches is labeled
  `endpoint="unmatched"` instead of with its path, so random paths no
  longer create a series each.

- Nineteen of the 37 `rx_*` metric families never moved in `serve`,
  so a dashboard built on them showed zero whatever happened. They are
  updated where the event happens: `rx_files_processed_total`,
  `rx_files_skipped_total`, `rx_bytes_processed_total` and
  `rx_file_size_bytes` per file a trace covers;
  `rx_patterns_per_request`, `rx_matches_per_request` and
  `rx_max_results_limited_total` per answered trace;
  `rx_parallel_tasks_created` per chunked file or seekable-zstd scan;
  `rx_trace_cache_skip_total`, `rx_trace_cache_load_duration_seconds`
  and `rx_trace_cache_reconstruction_seconds` in the trace cache;
  `rx_index_cache_hits_total`, `rx_index_cache_misses_total` and
  `rx_index_load_duration_seconds` per index lookup (a `GET /v1/tree`
  listing is not a lookup); `rx_samples_duration_seconds`,
  `rx_offsets_per_samples_request`, `rx_context_lines_before` and
  `rx_context_lines_after` per answered samples request; and
  `rx_analyze_duration_seconds` per index build with analysis. A trace
  of a seekable `.zst` now counts its trace cache hits and misses too,
  and a trace cache hit reads the cache file once instead of twice.

- `serve` no longer keeps a record of every failed `GET /v1/trace` for
  the life of the process. The trace handler kept each request in an
  in-memory store that nothing read, and only a successful trace
  became eligible for eviction, so a long-running server grew by one
  entry per failed search. The store is removed.

- An answer served from the trace cache reports the `file_chunks` of
  the scan that wrote the cache, on `rx trace --json` and `GET
  /v1/trace`. It reported `0` for a plain file (where the scan reported,
  for example, `20`) and the number of frames with a match for a
  seekable-zstd file, so the cache changed the answer. The cache
  records the count as `chunk_count`; a cache without it is treated as
  absent.

- The trace cache no longer keeps an incomplete answer for a log that
  grows during a trace. The cache records the file as it was when the
  scan's chunks were planned, and is not written when the file has
  changed by the end of the scan, so the next trace scans again and
  finds the new lines. It was stamped from a stat taken after the scan:
  the new size, with matches only up to the old one, and every later
  trace of the same pattern missed the appended lines. A cache is also
  checked against the file's inode, ctime and fingerprint, as the line
  index is, so a file replaced with one of the same size and mtime is
  scanned again. The trace cache format is version 4 and records
  `source_inode`, `source_changed_at` and `source_fingerprint`; a cache
  of version 3, which may hold such an incomplete answer, is treated as
  absent. A completed scan with no matches is now cached too, as
  rx-python does.

- The `cli_command` of `GET /v1/samples` names the context flags `rx
  samples` has, `--before=N` and `--after=N`. It rendered
  `--before-context=N` and `--after-context=N`, which the CLI does not
  know, so the pasted command exited 2. A test now parses every
  rendered command with the real command tree.

- `rx samples --before=0` and `--after=0` ask for no context lines on
  that side, as `before_context=0` does over HTTP. The CLI read a zero
  as "not given", so `--context=2 --before=0` answered
  `before_context: 2`, and the documented
  `--lines=5000-5100 --before=0 --after=0` printed three lines of
  context on each side.

- The `cli_command` of a `POST /v1/index` task renders `threshold` and
  `analyze_window_lines` (`--threshold=N`, `--analyze-window-lines=N`).
  It dropped both, so for a request with `"threshold": 0` the pasted
  command skipped the file the task had indexed.

- The `cli_command` of a `POST /v1/compress` task renders
  `--build-index=false` when the request turned the index off and
  `--force` when it asked to overwrite. Without them the pasted command
  built an index the task had not, and failed on the output the task
  had just written.

- The `cli_command` of `GET /v1/trace` renders the `request_id` and the
  `hook_on_file`, `hook_on_match` and `hook_on_complete` URLs the
  request gave, as `--request-id` and `--hook-on-*`. It dropped them,
  so the pasted command fired no webhook the request had asked for.

- The `GET /v1/index` page says what its `cli_command`,
  `rx index PATH --info --json`, prints: the whole stored index, of which
  the HTTP answer is a projection, and which members differ.

- The OpenAPI document allows `null` for the `result` of
  `GET /v1/tasks/{task_id}`, which is what every poll before the task
  completes answers; it declared a non-null object. rx-python declares
  it nullable too. Additive; the contract stays 1.3.

- The OpenAPI document declares every error status each operation can
  answer: `400`, `404`, `409`, `422`, `500` and `503` where the handler
  or huma's validation produces them, besides the `401` and `403` it
  declared before. Seven of nine operations declared only `200`, `401`
  and `403`, so a client generated from the document could not see the
  other errors. Each body is `ApiError` (`{"detail": ...}`); `403` is
  `oneOf` `SandboxError` and `ApiError`, since a hidden or unreadable
  path is refused with the plain envelope; and a `default` response
  covers the rarer ones. One table holds each status's description,
  and a test walks every handler's source to the error constructors it
  reaches and fails when a status it can return is not declared.
  Additive; the contract stays 1.3.

- A trace answer's `context_lines` and `file_chunks` are `{}` when they
  have nothing in them, over HTTP and in `rx trace --json`. They were
  `null` — `context_lines` on every search without context lines,
  `file_chunks` when the paths held no file to scan — where the OpenAPI
  document declares both as non-null objects, so a client generated
  from it rejected the answer. rx-python answers `{}` for `file_chunks`
  too, and `null` for `context_lines`.

- `POST /v1/compress` needs only `input_path`. It required all six body
  fields, so every example in its docs got a `422`. A field left out
  takes the default of the matching `rx compress` flag, which rx-python
  uses too: `frame_size` `"4M"`, `compression_level` `3`, `build_index`
  `true` (the docs said `false`), `force` `false`, and `output_path`
  `null`, meaning `<input_path>.zst`. An explicit `"build_index": false`
  is kept: `CompressRequest.BuildIndex` is a `*bool` in Go, because huma
  fills a default into every zero value. The OpenAPI document publishes
  the defaults and the level range 1-22; a level outside it, an explicit
  `0` included, is now a `422` validation error rather than a `400`, as
  in rx-python. A test compares the HTTP defaults with the CLI flag
  defaults, and another posts every example body in the docs. Additive;
  the contract stays 1.3.

- `GET /v1/trace` reads `path` and `regexp` as repeated parameters, one
  value per repetition, as the docs always said. It used to split one
  value at commas and ignore the repetitions: `regexp=a{2,5}` was
  refused as two broken patterns, a path with a comma could not be
  searched, and `path=a&path=b` or `regexp=x&regexp=y` searched only the
  first value, so the viewer's search over several files searched one.
  The OpenAPI document now declares both parameters `explode: true`. A
  client that sent a comma-joined list now sends one value per
  parameter; this is the documented form, so the contract stays 1.3.

- Webhook URLs given on `GET /v1/trace` (`hook_on_file`, `hook_on_match`,
  `hook_on_complete`) fire. `rx serve` shared one dispatcher whose URLs
  came from the environment at startup, so a URL given on the request
  was validated and then never called; with an `RX_HOOK_ON_*_URL` set,
  that URL was called instead, and every payload's `request_id` was
  empty. Each request now resolves its own URLs, with the precedence
  `rx trace --hook-on-*` follows (the request's URL over the
  environment's, unless `RX_DISABLE_CUSTOM_HOOKS` is set), and its
  payloads carry the `request_id` of its response. Concurrent requests
  still share one queue and one HTTP client, and each receives only its
  own events.

- The `trace_complete` webhook's `total_files_scanned` counts every file
  the trace searched, as rx-python does. It counted the response's
  `scanned_files`, which is filled only when a directory was walked, so
  a trace of named files reported `0`.

- The webhook docs describe the protocol the code uses: one `GET` per
  event with the payload as query parameters, the event names
  `file_scanned`, `match_found` and `trace_complete`, the real parameter
  names, the precedence between a request's URL and `RX_HOOK_ON_*_URL`,
  and the `dropped` status of `rx_hook_calls_total`. They described a
  `POST` with a JSON body and fields that do not exist, and called DNS
  rebinding unmitigated although the dialer checks the address it
  connects to. The `hook_on_*` descriptions in the OpenAPI document no
  longer say "URL to POST"; the contract is unchanged.

- A `--max-results` search of a seekable `.zst` keeps the first matches
  in the file. The frame-parallel scan ordered its matches by a line
  number that restarts in every frame before applying the cap, so it
  kept the first matching line of each frame instead: on a 55 MB
  PostgreSQL log, `--max-results=5` returned offsets 0, 8.8 M, 21.9 M,
  35 M and 48 M where `rg -m5` finds 0, 13 K, 618 K, 1.63 M and 1.64 M.
  It now orders by offset, as rx-python does.

- The viewer's `version.json` and `favicon.svg` are served with
  `Cache-Control: no-cache` instead of a year-long `immutable`. Only the
  files under `assets/` carry a hash of their content in their name; the
  others keep their name across viewer releases, so a browser kept the
  old `version.json` after an upgrade and showed the previous version,
  and its release link, in the header. Revalidating costs a `304`. A
  browser that already holds the year-long copy keeps it until a hard
  reload.

- `GET /v1/detectors` reported every detector's `severity_range` as
  `0..1`, which tells a client nothing it can scale an indicator by. Each
  detector now states its band through `analyzer.SeverityRanger` — every
  rx-go detector emits one fixed severity, so the band is a point, from
  `0.3` for a long line to `1.0` for a secret — and a detector that
  states none is still reported as `0..1`.

- `/health` reported every `RX_*` variable by value, and it answers
  without a token, so a secret set in the environment was public to
  anyone who could reach the port — including `RX_API_TOKEN`, which
  would have defeated it. A variable whose name contains `TOKEN`,
  `SECRET`, `PASSWORD` or `API_KEY` is now reported as `<redacted>`.

- `rx trace -i`, `-w`, `-x`, `-F` and `-P` gave wrong answers with exit
  0. The flags never reached ripgrep, and the argument after one was
  taken as its value: `rx trace error -i app.log` searched the current
  directory, and `rx trace -w error app.log` searched for the pattern
  `app.log`. They are now `rx trace` flags (`--ignore-case`,
  `--word-regexp`, `--line-regexp`, `--fixed-strings`, `--pcre2`),
  accepted anywhere on the line and passed to ripgrep on the plain,
  gzip and seekable-zstd paths and into the trace-cache key, so each
  answer is the one `rg` gives for the same flags.

- A line ripgrep matched could be dropped afterwards. rg does not say
  which `-e` pattern matched, so rx re-runs each pattern in Go to find
  out, and a line no pattern reproduced was discarded: every match of
  `-F 'foo('`, of a PCRE2 look-around under `-P`, or of a pattern in
  Rust-only regex syntax. The re-run now honours `-i`, `-w`, `-x` and
  `-F`, and a line Go cannot attribute is credited to the patterns it
  could not check, never dropped.

- A ripgrep config file changed rx's answers. ripgrep reads
  `RIPGREP_CONFIG_PATH`, so a personal `--fixed-strings` or
  `--smart-case` silently applied to every rx search. rx now runs
  ripgrep with `--no-config`.

- An error raised before a command runs — an unknown flag, a flag with
  no value — and a `--search-root` that does not exist exited with the
  right code and printed nothing. Commands print their own error line,
  so the root command silences cobra's, and nothing printed the errors
  that never reached a command. Every failure now ends with one `Error:`
  line on stderr: `Error: Unknown flag: --frobnicate`.

- `rx samples --offsets=B` past the end of the file reported the file's
  last line, which is a number counted from the wrong place. It now
  reports `-1` with a null sample, the same way a line number past the
  last line already answered, and the same way rx-python answers both.
  Line 0 and an empty file's line 1 answer that way too, on the plain,
  gzipped and seekable-zstd paths alike.

- `rx samples` prints the reason to stderr when a requested position is
  not in the file, so a person reading the terminal does not have to know
  the -1 convention to understand an empty answer. stdout stays
  parseable.

- `rx trace` printed plain text and ignored its own `--no-color`, while
  rx-python colourised the same output — so the two produced different
  text for the same search the moment a terminal was involved. It now
  emits rx-python's sequences in rx-python's places, byte for byte, and
  gained `--color=always|never|auto` so the choice is testable and means
  the same thing as it does on `rx samples`.

- `rx samples` on a plain file that ends with a newline reported an empty
  extra line after the last one. The final zero-length read is the end of
  the file, not a line; the compressed paths and rx-python have always
  known that, so the three storage forms of one log disagreed about their
  own last line.

- `rx trace --hook-on-file`, `--hook-on-match` and `--hook-on-complete`
  did nothing. The flags were declared, copied into the trace parameters
  and never read again, so a script that notified CI under rx-python
  silently notified nobody under rx-go — with no error and no log line.
  They now build a dispatcher, fire the same payloads rx-python sends,
  honour `RX_HOOK_ON_*_URL` and `RX_DISABLE_CUSTOM_HOOKS`, and drain the
  queue before the process exits. `--hook-on-match` without
  `--max-results` exits 2 with the message rx-python prints, rather than
  offering one HTTP call per matching line.

- `rx index` indexed binary files that rx-python skips, so the same
  directory produced two different sets of indexes. It now applies the
  same rule rx-python does — a NUL byte in the first 8 KiB means binary —
  which also makes the summary line "below threshold or not text" true.
- `POST /v1/index` with `analyze: true` refused a file below the size
  threshold with 400, while `rx index --analyze` indexes it. Analysis now
  bypasses the threshold on both surfaces.
- `rx samples` accepts `--line-offset` and `--byte-offset`, the names
  rx-python uses, alongside `--lines` and `--offsets`.


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
