# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
