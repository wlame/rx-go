# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- rx reads systemd journal files: `system.journal`, its
  `system@….journal` archives and any other file in journald's binary
  format, regular or compact, with values stored plain or compressed
  with xz, lz4 or zstd. A journal is recognized by its first bytes
  (`LPKSHHRH`), whatever its name: a journal named `app.log` is a
  journal, and a text file named `system.journal` is text. Its text is,
  entry by entry, exactly what
  `journalctl --file=F --no-pager --utc -q -o short-iso-precise` of
  systemd 257 prints for it, and every command works on that text as on
  the decompressed text of a gzip file: `rx trace` (a directory search
  includes journals), `rx samples` by line, offset and time, `rx index`,
  `rx time-range`, the findings of `rx trace --signatures`, `rx scan`
  and `POST /v1/findings`, and `rx compress`, which writes the text as
  seekable zstd under the journal's whole name with `.zst` added
  (`system.journal.zst`). Line numbers and byte offsets refer to the
  text. Answers report `is_compressed: true` and
  `compression_format: "journal"`, and a journal's line index has
  `file_type: "compressed"`. An entry rx cannot read whole (a damaged or
  cut-short journal) is left out of the text, and the entries after it
  are read. A journal whose header rx cannot read (an incompatible flag
  rx does not know, a header cut short) is refused by every command as
  `not a text file: a systemd journal rx cannot read: <why>`, and
  `GET /v1/tree` lists it with `is_text: false`. A gzip copy of a
  journal stays a binary file. The progress of a journal's index build
  and findings scan runs to the end of the journal's last object, not
  to the end of the file: a live journal is preallocated in 8 MiB steps,
  and the zeros after its objects are never read (a findings task's
  `bytes_total` is that end). rx reads journals itself, with no
  `journalctl`, on every system rx runs on; the lz4 values are decoded
  with the new dependency pierrec/lz4/v4 4.1.33 (BSD-3-Clause license).
- rx says when a systemd journal is damaged. The line index of a
  journal records a `journal` section: the entries the header counts,
  those the text shows, those without a message, those left out as
  damaged and those not reached after the place where the entry list
  breaks off, the first damaged entry with the reason, and the header's
  state (`online`, `offline`, `archived`), compact mode, `seqnum_id` and
  first and last sequence numbers. A damaged journal is indexed all the
  same. `rx index --info` prints the section; `rx index --json`,
  `--info --json`, `GET /v1/index` and the result of a `POST /v1/index`
  task carry it as `journal`, which is `null` for every other file.
  `rx trace` lists a damaged journal it read to the end of its text in
  `skipped_files` once, as `not searched in full: a damaged journal; N
  entries left out, M not reached; the other entries are searched`,
  keeps its matches with their line numbers, and logs a
  `journal_damaged` warning.
- `GET /v1/tree` (and `GET /v1/search-roots`) gives each file that is
  not text a `not_text_reason`: the words every command refuses it with
  (`not a text file: a NUL byte in its first 8 KiB`, `… UTF-16 text …`,
  `… a systemd journal rx cannot read: <why>`); `null` for a text file
  and a directory.
- Rotated systemd journals form a log chain: `system.journal` and its
  `system@{id}-{seqnum}-{time}.journal` archives, by a new name template
  `journal` whose key is the head time an archive's name writes.
  Archives without their active journal are a chain too, a
  `….journal~` file stays a single file, and when
  `user-1000@….journal` archives are in the directory,
  `user-1000.journal` is the active file of their chain, no longer a
  numbered part of `user.journal` (without them it stays one). A
  journal chain is ordered by its parts' headers, as systemd orders
  journal files, not by the times its text shows, which may go back:
  the parts of one counter by the sequence number of their first entry,
  the counters by the time of their first part's first entry. The order
  needs no line index, so `GET /v1/logs/chain` lists a pending journal
  chain's parts in it already. Its checks: the new reason
  `entries_overlap` (two neighboring parts of one counter hold entries
  in common, as a copy of a part left beside it does),
  `active_not_last`, and `unreadable` for a part whose bytes are not a
  journal or whose header rx cannot read
  (`NAME cannot be read as a journal: …`). Such a part stays in the
  chain even when it is not text (a binary `system.journal`, an archive
  with an incompatible flag rx does not know), where a chain of text
  logs leaves a file that is not text out. The time overlap check does
  not apply, and no part is missing. For the parts of one
  counter the chain's text is what `journalctl` prints over all of
  them.

### Changed

- HTTP wire contract 2.1, an addition to 2.0: `journal` in the index
  answers, `not_text_reason` in the tree and the chain reason
  `entries_overlap`. A client of 2.0 keeps working.
- Line index format 12, for the journal section: every index stored by
  an earlier version is treated as absent and built again once.
- `rx serve` installs viewer releases from 0.8.0 up to, but not
  including, 0.10.0 (the limit was 0.9.0). Contract 2.1 only adds
  members, so viewer 0.8.x keeps working, and viewer 0.9.x is the first
  to show journal files.
- Dependencies updated: huma 2.39.1, chi 5.3.1, klauspost/compress
  1.19.1, the Prometheus client 1.24.1 and its model 0.6.3,
  ulikunitz/xz 0.5.17 and golang.org/x/sync 0.22.0 (and pierrec/lz4/v4
  4.1.33 added, see Added).

### Fixed

- The progress of a findings scan of a compressed file (a findings
  task's `bytes_read`) is how far into the file the scan has read. It
  added the bytes the timestamp detection read from the head of the
  file to the bytes of the scan itself, so it reported the whole file
  read before the scan had read it all.
- `rx serve` listens for SIGINT and SIGTERM from its first moment. A
  Ctrl-C during the startup (the viewer download, the loading of the
  signature catalogue) was ignored, and the server came up and ran; it
  now stops as soon as it is up.
- A stopped index build of a zstd stream file (Stop on a folder's
  index in the viewer, `DELETE /v1/tasks/{id}`) ends at its next read
  of the text. The zstd decoder reads its input ahead, so on a small
  file it could have read every compressed byte before the stop, and
  the build then ran to its end.

## [0.8.0] - 2026-10-12

Signatures replace the index analysis: rx finds tracebacks, crashes,
secrets, SQL, bursts of lines and more by itself, with built-in and
user-written signatures, and caches the findings per file. This
release speaks HTTP wire contract 2.0 and writes line index format 11,
and `rx serve` serves rx-viewer 0.8.0 to before 0.9.0 only: release it
back to back with rx-viewer 0.8.0, this one first. The docs page
History maps every removed surface to what replaces it.

### Added

- Signatures: named definitions of something worth noticing in a long
  file, such as a Python traceback, an AWS access key or a run of blank
  lines. rx has built-in signatures (70 in eleven groups, listed
  below) and reads more from YAML files: every `*.yml` and `*.yaml`
  file in the directories or files of `RX_SIGNATURES_PATH` (separated
  by `:`), else
  in `$XDG_CONFIG_HOME/rx/signatures/`, else in
  `~/.config/rx/signatures/`. A signature has an id, a name, a group,
  tags, a level, a description, links, metadata, `default`, `redact`,
  keywords, examples, and one kind: `regex` (one line, with `not`,
  `capture`, `entropy`, `validate`), `block` (a start line and the lines
  after it, in a simple form or as states), `run` (consecutive lines
  that match, are blank or repeat the one before) or `measure` (Go code
  with typed parameters). A user signature with a built-in's id replaces
  it, `disable:` switches built-ins off, and `groups:` declares groups.
  Each anchor regex gets keyword clauses derived from its syntax tree: a
  line is tested only when it holds a member of every clause. Problems
  are reported with their file, line and signature id: a YAML syntax
  error, an unknown top-level key or a limit (1 MiB per file, 1,000 user
  signatures, 4 KiB per regex, 10,000 nodes of alias expansion,
  `max_lines` 10,000) refuses the whole file; a bad regex, an unknown
  field, a missing or second kind, an unknown group, a duplicate id or
  `redact: true` on a `blank` or `same` run (nothing there to mask)
  refuses that signature. Only the commands that use signatures read
  the files. The docs page Concepts / Signatures describes the model and
  the file format.
- The built-in signature catalogue: 70 signatures in eleven groups, each
  with a description that says what it finds and its main false
  positive, links to its sources, and invented examples that every load
  runs. `traceback`: `python-traceback` (chained exceptions and
  exception groups in one finding), `java-exception` (`Caused by:`,
  `Suppressed:`, `... N more`), `dotnet-exception`, `js-error`,
  `go-panic` (goroutine dumps, the `goroutine N gp=… [status]:` form,
  up to `exit status N`), `ruby-backtrace` (both quote styles),
  `php-fatal`, `rust-panic`. `crash`: `sanitizer-report`,
  `glibc-abort`, `killed-by-signal`, `kernel-segfault`,
  `systemd-coredump`, `kernel-oops`, `oom-killer`, `k8s-crash-loop`,
  `jvm-fatal-error`. `secret`, every one masked: `aws-access-key`,
  `aws-secret-key`, `github-token`, `gitlab-token`, `slack-token`,
  `slack-webhook`, `stripe-key`, `google-api-key`, `anthropic-key`,
  `openai-key`, `jwt`, `private-key` (the whole PEM or PGP block, its
  body masked as line lengths), `password-assignment`, `url-password`,
  `authorization-header`, `high-entropy-token` (the `entropy` measure
  at its defaults: nine random base64 keys in ten or more of 24 to 64
  characters, no hex hashes or ids; base64 of a short English text,
  14 to 48 bytes, is found about half to two thirds of the time, and
  about 1 in 100 identifiers that mix cases and digits). The examples of the secret
  signatures write one character of each token as a YAML escape, so
  the files hold no string shaped like a provider's key, and a test
  scans them, the reference page and the test data for one.
  `pii`, opt-in: `email-address`, `ipv4-address`, `ipv6-address`,
  `card-number` (Luhn), `iban` (mod 97). `sql`: `sql-statement`,
  `dangerous-sql` (DROP, TRUNCATE, DELETE or UPDATE without WHERE),
  `postgres-explain`, `postgres-slow-statement`, `postgres-deadlock`,
  `postgres-lock-wait`, `mysql-slow-query`. `http`: `http-5xx` (common
  and combined access logs, JSON `status`, logfmt). `severity`:
  `error-line` (upper-case level words) and the opt-in `warning-line`.
  `system`: `disk-full`, `too-many-open-files`, `out-of-memory`,
  `connection-failure`, `network-timeout`, `tls-failure`, `dns-failure`.
  `lifecycle`: `signal-received`, `exit-status`, `systemd-unit-change`.
  `structure`: `blank-run`, `long-line`, `repeated-lines`,
  `repeat-suppressed`, `json-block`, `binary-bytes`, `base64-blob` (the
  `base64-run` measure), `hex-dump`, `unprefixed-run`. `time`:
  `log-burst`, `time-gap`, `time-backwards`. 64 of them are in the
  default set. They find every case the nine detectors of
  `rx index --analyze` found. Some ranges differ: a chained Python
  exception is one finding where `traceback-python` gave one per
  traceback; a kernel oops runs from its `BUG`, `Oops` or `kernel BUG
  at` line to its `end trace` line; a private key covers its whole
  block where `secrets-scan` marked the BEGIN line; a secret is a
  finding of its line with the secret as the value span. `long-line`
  compares a line with the 100 lines on each side of it where the
  `long-line` detector compared it with the whole file's percentiles,
  so lines as long as their neighbours are no longer found. The
  reference page Reference / Built-in signatures is
  generated from the catalogue (`just docs-signatures`), and a test
  fails when it is not current.
- `rx signatures` lists the signatures by group with their level, kind,
  source and marks (`overrides`, `masked`, `opt-in`, `disabled by
  file:line`), then the load errors and the search path; `--group=`
  and `--tag=` narrow it and `--json` prints it as one document.
  `rx signatures show ID` prints one definition as YAML with every
  default, its source, its keyword clauses and whether it runs on every
  line. `rx signatures check [FILE...]` reports every error and warning
  of the signature files and exits 1 on an error, for CI.
  `rx signatures template` prints a commented file with one signature
  of each kind.
- `RX_SIGNATURES_PATH`: where the signature files are.
- The signature scan engine: one pass over a file's text, in parallel,
  for a selection of signatures at once. A plain or seekable zstd file
  is read in line-aligned chunks (about eight per worker, at least
  4 MiB each); a gzip, bzip2, xz or zstd stream is decoded once and
  handed to the workers in batches of whole lines. Every line is
  matched as its view (without its `\n` or `\r\n`, without a UTF-8
  byte-order mark at the start of the text, cut at
  `RX_SIGNATURES_LINE_BYTES`); one Aho-Corasick pass per line finds the
  keywords of every signature, and an anchor runs only on a line that
  holds a member of each of its keyword clauses, or on a line that holds
  the Kelvin sign or the long s (which a `(?i)` `k` or `s` matches), so
  a match through those letters is found, and masked when it is a
  secret. Findings carry exact
  1-based line numbers and text offsets, the same for any number of
  workers, any chunk size and any storage of the text: blocks and runs
  that cross a chunk edge are finished across it. Each signature keeps
  its first `RX_FINDINGS_MAX_PER_SIGNATURE` findings by line with an
  exact count; an excerpt is the first line cut to 200 bytes, always
  valid UTF-8 (each byte that is not part of a UTF-8 character becomes
  U+FFFD, after masking). With masking on (see the next entry), for a
  signature with `redact: true` every secret on that line is masked
  before the excerpt is kept: the value of every match of its anchor
  (the `capture` group, or the whole match when the group took no part)
  and every other copy of such a value on the line. A mask keeps a
  quarter of the value's characters, at most 4, and the rest becomes
  `*` (at most 12). Masking takes linear time and fails closed: a line
  with more than 8 distinct values or a value over 256 bytes shows only
  its text before the first secret, and one with more than 64 distinct
  values or 4,096 matches is masked whole. Every excerpt is masked by
  every loaded redacting signature, selected or not, so a finding of a
  signature that does not redact never shows a secret either. The
  bodies of redacting blocks and runs are masked as well, since a PEM
  key's material is in lines no value mask finds: every scan also walks
  every loaded redacting `block` and `run` signature, selected or not,
  for where its bodies lie (every line a block joined after its anchor,
  a block under its `min_lines` and its trailing blank lines included;
  every line of a run after its first), the same for every chunking and
  storage, and a finding of any signature whose first line is a body
  line has the excerpt `[masked, N bytes]` (N the line's length) and is
  marked masked. A signature's first 100,000 bodies are kept line by
  line; past that, every line from its last kept body to the end of the
  file counts as a body.
- Masking of secrets is opt-in. By default rx shows the text of every
  finding as the file holds it, secrets included: no signature masks
  anything, `meta.ranges.json` holds no range and `masked` is never
  set. `--mask-secrets` on `rx` and `rx trace` (with `--signatures`)
  and on `rx serve`, or `RX_MASK_SECRETS` in the environment (the
  boolean forms of `RX_HIDDEN`; any other value leaves it off), turns
  on the masking every entry here describes; the flag wins over the
  variable, either way. `redact: true` means "mask this signature's
  values when masking is on". The redacting signatures enter every key
  of the findings cache and the key of the masked ranges, and with
  masking off that set is empty, so a masked answer never comes from an
  entry written without masking, nor the reverse: switching the mode
  scans the file again. For the HTTP API the mode is the server's: the
  start banner of `rx serve` says `Secrets: masked` or `Secrets: shown;
  start with --mask-secrets or RX_MASK_SECRETS=1 to mask them`, and
  `cli_command` carries `--mask-secrets` when the server masks. `rx
  signatures` ends its list with a `Secrets:` line that says the mode
  `RX_MASK_SECRETS` gives.
- Every finding of a signature scan has a time: the timestamp of its
  first line, or else that of the nearest earlier line with one when
  that line starts at most `RX_TIMESTAMP_LOOKBACK_KB` before it (the
  rule of `line_timestamps`); none past that, and none in a file
  without timestamps. The scan reads the file's timestamp format from
  the head of its text, and each line's timestamp, with the functions
  an index build uses, and never reads a stored index: a finding's time
  is the time the index records for that line, in the file's own frame,
  whether the file has an index or not. Every chunking and every
  compressed copy give the same times. A scan reads the head of the
  file once more for the format.
- The signature scan runs the measures `entropy` (tokens of
  `[A-Za-z0-9+/=_-]` of `min_length` bytes or more, default 20, whose
  Shannon entropy reaches `ratio`, default 0.94, of the entropy a
  random token of the same length has on average over the 64 base64
  characters, and which hold an upper-case letter, a lower-case letter
  and a digit; a token of hex digits only needs `hex_ratio` of a random
  hex token's average and is never found at its default 0, since
  hashes and request ids are as random as hex keys; UUIDs and
  digit-only tokens skipped; the first token is the value; the
  parameters `base64` and `hex` of fixed thresholds are refused with a
  message that names their replacement),
  `base64-run` (a run of base64 bytes with up to two `=`, found by a
  byte scan; the run is the value), `binary` (runs of lines that hold a
  NUL byte or bytes that are not UTF-8) and `unprefixed` (runs of lines
  without a timestamp of their own, nothing when stamped lines are under
  `min_share` of the file), `long-line` (a line of `min_bytes` or more
  whose length is at least `factor` times the lower median length of
  the `window` lines on each side, windows reaching across chunk edges)
  and `json-block` (a line that is only `{` or `[` through its matching
  closer at the same indent, by a string-aware bracket count; the old
  `json-blob-multiline` detector's cases give the same ranges, and a
  block still open at `max_lines` or at the end of the text is a
  finding of the lines it holds), and the time measures `time-backwards`
  (a line stamped more than `tolerance` before the stamped line before
  it), `time-gap` (a silence of at least `min_gap` and `factor` times the
  median interval between consecutive stamped lines, found as the line
  before it and the stamped line after it) and `density` (bursts of
  lines per time bucket by count, `factor` times the median and a
  modified z-score, the bucket `auto` picking about 1,000 buckets over
  the span; the bucket series is kept with the findings, at most 65,536
  buckets). With `redact: true` an `entropy` or
  `base64-run` signature masks its value and every excerpt of a line
  that holds one; `redact: true` on any other measure refuses the
  signature (`redact: the measure long-line finds no value to mask`).
  A measure duration finer than a millisecond (`1500us`) refuses the
  signature.
- `rx signatures`, `show` and `check` run every signature's examples:
  a `match` text that gives no finding, or a `no_match` text that gives
  one, refuses the signature with its file, line and id
  (`examples: match example 1 gives no finding`), and `check` exits 1.
  The built-in signatures pass theirs.
- `RX_SIGNATURES_LINE_BYTES` (default 65536, 1024 to 1048576): how many
  bytes of each line a signature scan matches against; a longer line is
  cut there for matching and still counted at its full length.
  `RX_FINDINGS_MAX_PER_SIGNATURE` (default 10000, 100 to 1000000): the
  most findings of one signature a scan of one file keeps, the first by
  line, with the count kept exact past it. Out-of-range values follow
  the rule of every integer setting.
- The findings cache: the findings of a signature scan are kept in
  `<cache>/findings/`, beside `indexes/` and `trace_cache/`, one
  directory per file (`<basename>_<path hash>`) with `meta.json` (the
  file's identity, as the line index records it, per signature its
  key, count, stored findings and scan time, and the key and count of
  the masked ranges), `meta.ranges.json` (the file's masked ranges: the
  body lines of every redacting block and run, keyed by the engine
  version, `RX_SIGNATURES_LINE_BYTES` and the redacting signatures, and
  the line count and length of the text the scan read) and
  one `<signature-id>.json` per signature (its findings with their
  times and the zone offset each time's stamped line writes, masked
  excerpts, value spans and `masked` flags, and a density
  measure's bucket series). Cached entries are used only with a current
  `meta.ranges.json`; without one the file's signatures are scanned
  again. A later scan of the file answers every
  signature with a current entry from the cache, equal field by field,
  and scans only the others, in one pass. An entry is stale when the
  file changes (size, mtime, inode, device, ctime or fingerprint), when
  its signature's matching part changes (not its name, description,
  group or other descriptive fields), when masking is turned on or off,
  when any redacting signature changes with masking on, and when
  `RX_SIGNATURES_LINE_BYTES`,
  `RX_FINDINGS_MAX_PER_SIGNATURE`, `RX_TIMESTAMP_LOOKBACK_KB` or
  `RX_LOG_TZ` changes. A file is full when every signature of the
  default set has a current entry, partial with at least one, none
  without; reading that reads only `meta.json`. Entries are written
  before `meta.json`, the commit point, under an advisory lock of the
  directory; a writer reads `meta.json` again first and keeps what
  another process added, and removes entry files `meta.json` does not
  name. A file that changes during a scan is answered and not cached,
  one that gets shorter or is replaced fails the scan with an error that
  names it, and a canceled or failed scan writes nothing. One scan of a
  file runs at a time in a process; a second waits and scans what is
  still missing. Every cache operation goes through the file's directory
  opened as a root that cannot be left; a symbolic link below
  `findings/` (the file's directory, `meta.json`, an entry or `.lock`)
  is never followed, written through or deleted through: that file's
  findings are not cached, and one Warn line per process,
  `findings_cache_unsafe_entry`, names it (`findings/` itself is
  followed when it is a link, as `indexes/` is). A file another writer
  renames into place while it is being opened is checked again, up to
  three times, and never logged. A missing opt-in signature
  never keeps a file from full; a file scanned only with opt-in
  signatures is partial.
- `RX_MAX_SIGNATURE_SCANS` (default 1, 1 to 64): how many signature
  scans read files at once in `rx serve`; later ones wait their turn,
  and a scan answered from the cache does not wait.
- `rx signatures show ID` prints the signature's cache key with the
  current settings (`# cache key: …`, after `# line pass:`).
- `rx scan PATH... [--signatures=SEL] [-f|--force] [-r|--recursive]
  [--json] [--mask-secrets]`: scan files for signatures and keep their
  findings in the findings cache, the command `POST /v1/findings` is
  the equivalent of. A file whose every selected signature is current
  is not read again (`current`) unless `--force`. A directory is
  scanned as `rx index` indexes one: the files the file tree lists
  directly inside it and, with `--recursive`, every file below it, one
  at a time, in the walk's order (hidden entries only with `--hidden`);
  a file that is not text is skipped with the reason. The human output is one
  line per file (`logs/app.log: full, 3 findings (error-line 2,
  python-traceback 1)`, `, current` when nothing was read), the skipped
  files and a total line (`Total: 3 files, 2 scanned, 1 skipped (0
  current), 0 failed, 3 findings in 0.0s`); `--json` gives
  `signatures`, `files` (`path`, `outcome` scanned or current, `state`,
  `counts` of the signatures that found something, `findings`),
  `skipped`, `errors`, `totals` (`files_total`, `done`, `skipped`,
  `current`, `failed`, `findings`) and `total_time`. The settings are
  those of `rx trace --signatures`; the exit codes those of `rx index`.
- New dependency: `go.yaml.in/yaml/v3` v3.0.5, the YAML parser, under
  the MIT licence (the files ported from libyaml) and the Apache
  License 2.0 (the rest).
- `rx trace --signatures[=LIST]` (and `rx --signatures …`): scan every
  text file the search covers for a selection of signatures as well
  (`--signatures=traceback,sql`, `--signatures=all,-pii`; a bare
  `--signatures` is the default set), through the findings cache, one
  entry per file. A pattern is optional and goes with `-e`; without one
  the trace runs no ripgrep. A selection that cannot be resolved exits 2
  with the word and up to five close names, before any file is read.
  `--no-cache` skips the findings cache too, `--no-index` the line index
  the text may be read with, `--max-results` caps the matches only, and
  `-B`, `-A` and `--context` add lines before and after each finding. A
  file that is not text or cannot be read is listed under the skipped
  files with its reason. The human output adds, after the matches, a
  count line (`Findings: 3 in 1 file (python-traceback 1, error-line
  2)`) and each finding as `path:start-end  id  group · level` with its
  lines numbered (20 at most, then `… 380 more lines: rx samples
  app.log --lines=24-403`), coloured by level with `--color`. `--json`
  adds `signatures` (`{id: {name, group, level, kind}}`), `findings`
  (one `Finding` shape: `signature`, `file`, `start_line`, `end_line`,
  `start_offset`, `end_offset`, `time_ms`, `excerpt`, and optional
  `value_start`, `value_end`, `masked`, `lines`, `before`, `after`,
  `lines_truncated`; ordered by file id, first line, last line and
  signature id) and `finding_summary` (`{file_id: {state, signatures:
  {id: {count, stored}}}}`), and `cli_command` spells the selection out
  (`--signatures=default`). A finding's `time_ms` is an instant: in a
  file whose timestamps carry no zone it is read in `RX_LOG_TZ`, as
  `rx samples` reads it. A trace without `--signatures` answers exactly
  as before. `rx logs trace` has no `--signatures`.
- The lines of a finding are read from the file when the answer is made,
  in one pass per file that seeks to each finding in a plain or seekable
  file, each cut at `RX_MAX_LINE_TEXT_BYTES` and made valid UTF-8, and,
  with masking on, masked as excerpts are: every value a loaded redacting signature
  matches on the line, before the line is cut, so a secret the cut falls
  inside is masked whole; every body line of a loaded redacting `block`
  or `run`, selected or not, becomes `[masked, N bytes]` in the lines of
  every finding and in the lines before and after one, so a private
  key's base64 lines never show around an error line next to it. No
  line past the end of the text the scan read is shown, and a last line
  written on since shows the bytes the scan saw: a log that grew after
  its scan (the rest of a key block appended, say) shows its new lines
  after the next scan.
  `masked` is true when anything in the excerpt, the lines, or the
  lines before and after was masked, or the finding's signature
  redacts and masking is on.
  The matches of `-e PATTERN` (`line_text`, `context_lines`) are not
  masked: they are the pattern's own output.
- `RX_FINDINGS_TEXT_MAX_BYTES` (default 67108864, 1 MiB to 16 GiB): the
  most bytes of text the findings of one answer hold together, in their
  order, each line counted with one byte more for its line break; the
  finding at which it runs out and every one after it keep their place
  and lose their lines (`lines_truncated: true`).
- HTTP wire contract 2.0: the trace answer (`GET /v1/trace`) declares
  the optional `signatures`, `findings` and `finding_summary` members and
  the `Finding`, `SignatureRef`, `FileFindingSummary` and `FindingCount`
  schemas. A trace answer without them is the 1.7 answer.
- `rx serve` loads the signatures once when it starts, with every
  example checked, and logs each load error as a `signature_load_error`
  warning with its file, line and id; the server starts either way. It
  reads the scan settings then too, and runs at most
  `RX_MAX_SIGNATURE_SCANS` scans at once.
- `GET /v1/signatures`: the catalogue the server loaded, the document
  `rx signatures --json` prints: every signature with every field of its
  definition, its kind, source (`{"builtin": true}` or `{"file",
  "line"}`), keyword clauses, line pass, `overrides` and `disabled_by`;
  the groups with their order and count; the levels; the load errors
  (`file`, `line`, `id`, `message`); the search path and the files read.
- `GET /v1/findings?path=…`: a file's findings from the findings cache
  alone; it never scans. Every answer has the summary of the requested
  selection (`signatures=`, default the default set): `state`,
  `checked`, `missing`, `counts`, `series`, then `total` and
  `cli_command` (`rx trace PATH --signatures=…`). `findings` is a page
  (`offset`, `limit` up to 10,000, default 1,000) of the stored findings
  that pass the filters `signature=` and `group=` (repeatable), `q=` (in
  the excerpt or the signature name, without case), `lines=A-B`
  (overlap), `after_line=N` and `before_line=N` (listed from the nearest
  down); `total` counts them. `text=true` fills each finding's `lines`
  and `before=N` (0 to 5) lines before it, masked on a server that
  masks secrets as every carrier of finding text is (every value a loaded redacting signature matches,
  every body line of a redacting block or run as `[masked, N bytes]`,
  nothing past the text the scan read), within `max_bytes` or
  `RX_FINDINGS_TEXT_MAX_BYTES`. `ticks=W` (1 to 4,000) places the
  filtered findings with a time on `W` columns over the file's time
  range, `start_ms` and `end_ms` as `GET /v1/time-range` gives them,
  with each column's count and first line per signature. `file_tz` reads
  `time_ms`, `series` and `ticks` as it reads every time: a finding's
  `time_ms` is the `line_timestamps` value `GET /v1/samples` gives its
  line under the same `file_tz`, with a line index and without one (a
  zoned file's series are left out under `file_tz`). `rx serve` keeps
  the decoded findings of the last 8 files read, at most 64 MiB, so
  paging and filters of one file decode its cache files once; a kept
  answer is used only for the file's identity and the entries' keys it
  was read with. An answer over 64 MiB on its own is not kept, and the
  files already kept stay: it does not evict them first.
- `POST /v1/findings {path, signatures, force}`: a scan of the file as a
  background task (operation `findings`, `202`): it scans the selected
  signatures the cache lacks (all of them with `force`) and caches what
  it finds. A second `POST` while a scan of the file runs returns that
  task. `GET /v1/tasks/{task_id}` shows `queued` while it waits for the
  scan queue, then `running` with `bytes_read`, `bytes_total` (new
  members of every task status, `null` for the other operations) and
  `progress`; its result is the answer of `GET /v1/findings` without
  filters, with the first page. A scan runs beside an index build or a
  compression of the same file. At most 256 findings tasks are queued or
  running at once; past that a `POST` for another file answers `503`
  with `Retry-After: 5` and starts nothing.
- `DELETE /v1/tasks/{task_id}`: cancels a queued or running findings
  task: the scan stops and caches nothing, and the task ends at once
  with the new status `cancelled`. A task of another operation, one
  that has ended, or one whose scan has finished reading and is writing
  its answer answers `409`, so a task never ends `cancelled` with
  findings written; an unknown id `404`.
- `POST /v1/index` and `POST /v1/findings` accept a directory (inside a
  search root): one task (operation `index` or `findings`) takes every
  file the file tree lists under it, recursively, one at a time, in the
  order `rx index --recursive` and `rx scan --recursive` take them (the
  same walk). A file whose index or findings are current is skipped
  unless `force`, and so are a file that is not text, a file below the
  index threshold (`threshold`, else `RX_LARGE_FILE_MB`) and the entries
  the walk refuses; a file whose build or scan fails is recorded and the
  task goes on. The files of a findings task go through the scan queue
  one at a time (`RX_MAX_SIGNATURE_SCANS` holds), and the task takes one
  of the 256 places of the unfinished findings tasks. `DELETE
  /v1/tasks/{task_id}` cancels such a task: the file it is on stops at
  its next read (an index build as a scan) and writes nothing, no later
  file is taken, and the files ended before keep their index or
  findings. The task then shows the new status `cancelling`, keeps the
  directory until its worker has stopped (a `POST` for it answers
  `409`, `POST /v1/findings` declares the `409` now), and ends
  `cancelled`, so a `POST`+`DELETE` loop never runs two workers for one
  directory. A second `POST` for the directory while its task runs
  joins its findings task or answers `409` for its index task. The result is the new `FolderTaskResult`:
  `path`, `files_total`, `done`, `skipped`, `current` (the skipped ones
  that were current), `failed`, `findings` (`null` for an index task),
  `skipped_files` (`{path, reason}`, the first 1,000 skipped for another
  reason than being current), `failed_files` (`{path, error}`, the first
  1,000) and `cli_command`. A directory was refused with `400` before.
- Task answers carry `cli_command`, the `rx` command that does what the
  task does: the `202`/`200` of `POST /v1/index` and `POST /v1/findings`
  (`TaskResponse`) and every `GET` or `DELETE /v1/tasks/{task_id}`
  (`TaskStatusResponse`) of an index or findings task: `rx index PATH`
  (`--force`, `--threshold=N` as requested), `rx scan PATH
  --signatures=SEL` (`--force`, and `--mask-secrets` when the server
  masks), with `--recursive` for a directory and `--hidden` when the
  server serves hidden entries (`rx serve --hidden`, `RX_HIDDEN`). Other tasks give `""`. A
  task over a directory also reports `files_done` and `files_total`
  (`null` for a task over one file); its `bytes_read` and `bytes_total`
  follow the file it reads (findings) or stay `null` (index), and its
  `progress` is the files ended plus the share of the current file,
  over `files_total`. These members are part of contract 2.0.
- `GET /v1/trace?signatures=…`: the answer of `rx trace --signatures=…
  --json` over HTTP: every file the search covers is scanned for the
  selection through the findings cache, `regexp` may be left out, and
  `no_cache`, `no_index` and the context window reach the findings as
  they reach the search. A selection that cannot be resolved answers
  `400` with the load error.
- `GET /v1/tree`: each file entry has `findings`, `{state, count}` from
  the file's findings `meta.json` alone (`full` or `partial`, the
  findings count of every current entry), and `null` for a directory, a
  file that is not text and a file the cache holds nothing current for.
- `/health` lists the feature `signatures`.
- Metrics `rx_signature_scans_total{status}` (each findings task that
  ended: `completed`, `failed`, `cancelled`),
  `rx_signature_scan_duration_seconds` (each completed findings task
  that read the file) and `rx_findings_cache_hits_total` (each signature
  answered from the findings cache by a `GET /v1/findings` or a findings
  task). No label carries a value from a file.
- HTTP wire contract 2.0 also declares `GET /v1/signatures`,
  `GET /v1/findings`, `POST /v1/findings`, `DELETE /v1/tasks/{task_id}`,
  the `signatures` parameter of `GET /v1/trace`, the `findings` member
  of a tree entry, the `cancelled` task status, the task members
  `bytes_read` and `bytes_total`, and the schemas `SignaturesResponse`,
  `SignatureInfo`, `GroupInfo`, `LoadErrorInfo`, `FindingsResponse`,
  `FindingsTicks`, `FindingSeries`, `FindingsScanRequest` and
  `TreeFindings`. All of it is additive.
- `rx serve --tree-refresh=SECONDS` and `RX_TREE_REFRESH_SECONDS`
  (default 5, from 0 to 3600; 0 turns it off): how often the viewer
  reads the loaded folders of its files list again while its tab is
  visible, so new, deleted and grown files show without a reload. The
  flag wins over the variable, and a flag value outside the range exits
  2. `/health` gives the interval to the viewer as `tree_refresh_seconds`
  (an integer, an additive member of contract 2.0), and the start banner
  names it (`Tree refresh: every 5 s` or `Tree refresh: off`).

### Changed

- With `--signatures`, every positional argument of `rx` and
  `rx trace` is a path and a pattern goes with `-e`:
  `rx --signatures=traceback ERROR app.log` reads `ERROR` as a file.
  Without the flag the first positional is still the pattern.
- `RX_WORKERS` also sets how many goroutines a signature scan reads
  lines with.
- HTTP wire contract 2.0, a breaking change: `/health` reports
  `contract_version` `"2.0"`. It removes `GET /v1/detectors`, the
  analysis members of the index answers and the analysis fields of
  `POST /v1/index` (see Removed), and holds every addition listed above.
  A client that knows only contract major 1 must refuse this server.
- Line index format 11: an index stores no analysis. An index stored by
  an earlier rx (format 10) is treated as absent, so every index is
  built again once, smaller, the first time a command needs it.
- `rx serve` installs viewer releases `0.8.0 <= v < 0.9.0` only: 0.8.0
  is the first viewer that speaks contract 2, and every earlier one
  (0.7.1 included) knows only contract major 1, so a cached one is not
  served. Release this rx-go and rx-viewer 0.8.0 back to back: between
  the two releases `rx serve` has no viewer to serve and comes up with
  `Viewer: none (/ redirects to /docs)`; the API answers as usual.

### Removed

- The index analysis and its nine detectors (`traceback-python`,
  `traceback-java`, `traceback-js`, `traceback-go`, `coredump-unix`,
  `secrets-scan`, `long-line`, `repeat-identical`,
  `json-blob-multiline`). Signatures replace them: the built-in
  catalogue finds every case they found (see Added), with
  `rx trace --signatures` and `GET`/`POST /v1/findings`.
- `rx index --analyze` (`-a`) and `--analyze-window-lines`. They stay as
  hidden flags so an old script learns what replaced them: any use, with
  any value, prints `Error: --analyze was replaced by: rx trace
  --signatures PATH` (for the window flag: `Error: --analyze-window-lines
  was removed with --analyze, which was replaced by: rx trace
  --signatures PATH`) and exits 2 before anything is read. `rx index`
  no longer prints `Indexed and analyzed`, the statistics and anomaly
  lines, `analysis_performed` under `--info`, or `analysis=` in the
  summary of several paths. A seekable zstd index has no line-length
  statistics any more: only `--analyze` computed them.
- `RX_ANALYZE_WINDOW_LINES`.
- The members `analysis_performed`, `anomalies`, `anomaly_count`,
  `anomaly_summary`, `analysis_window_lines` and `analysis_detector_set`
  of `rx index --json`, `rx index --info --json`, `GET /v1/index` and
  the result of a `POST /v1/index` task, and the schema
  `AnomalyRangeResult`.
- The `analyze` and `analyze_window_lines` members of a
  `POST /v1/index` body: a body that holds either answers `422`
  (`validation failed; unexpected property (body.analyze: …)`), as any
  member the schema does not declare. Asking for analysis no longer
  skips the size threshold; `threshold: 0` indexes a file of any size.
  The task's `cli_command` no longer renders `--analyze` or
  `--analyze-window-lines`.
- `GET /v1/detectors`, which now answers `404` as any route the server
  does not have, its schemas `DetectorsResponse`, `DetectorInfo`,
  `CategoryInfo`, `SeverityRange` and `SeverityScaleLevel`, and the
  OpenAPI tag `Analysis`. `GET /v1/signatures` lists the signatures.
- The metrics `rx_analyze_requests_total` and
  `rx_analyze_duration_seconds`; `rx_signature_scans_total` and
  `rx_signature_scan_duration_seconds` count signature scans.
  `rx_errors_total` counts failed trace and samples requests.
- The docs pages Concepts / Analyzers and API / Detectors. The
  line-length statistics are described in Concepts / Line indexes, the
  signatures in Concepts / Signatures.

### Fixed

- The OpenAPI description of a finding's `masked` is corrected: it
  names the lines before and after the finding as well as the excerpt
  and the lines, as the field covers all of them. The field itself is
  unchanged.

## [0.7.0] - 2026-10-10

### Changed

- `rx serve` installs viewer releases up to 0.8.x: the range of viewer
  releases it accepts widens from `0.2.0 <= v < 0.8.0` to
  `0.2.0 <= v < 0.9.0`.

## [0.6.0] - 2026-10-09

### Added

- Log chains: a rotated log (`syslog`, `syslog.1`, `syslog.2.gz`,
  `auth.log-20261001.gz`, `app-2026-10-01.3.log.gz`, …) is read, searched
  and numbered as one text. rx finds a chain from the names in one
  directory listing, by a table of four name templates; orders its parts
  by their first timestamps; checks that every part with lines has
  timestamps, that the parts follow each other in time and that the
  active file comes last; and numbers the lines of the chain as those of
  its parts decompressed in that order into one file, so every chain
  answer equals the answer for that file, with line indexes and without.
  It keeps no state between requests: a fingerprint of the chain's files
  lets a client notice a rotation (`409`, exit 7), and the active file
  may grow. A chain is `pending` until its frozen parts are indexed (one
  background task per chain builds them), `ready`, or `invalid` with its
  reasons. New: `rx logs list`, `show`, `time-range`, `index`, `samples`
  and `trace`; `GET /v1/logs/chains`, `GET /v1/logs/chain`,
  `POST /v1/logs/index`, `GET /v1/logs/samples` and `GET /v1/logs/trace`
  (contract 1.7, feature `log_chains` in `GET /health`); exit codes 6
  (the chain is invalid) and 7 (its files changed since a fingerprint);
  `RX_CHAIN_OVERLAP_SECONDS`; the docs page Concepts / Log chains. In
  detail:
  - `rx logs`, a command group for log chains (rotated logs read as one
    text), with `rx logs list [DIR...]` (default: the current directory):
    per directory, a line with its path and number of chains, then a
    table of name, parts, size, `idx` and missing parts; `--json` prints
    the `GET /v1/logs/chains` body, one object for one directory and an
    array for several. Exit 3 for a directory that does not exist, 4 for
    one outside `--search-root`, hidden or unreadable, 2 for a file; with
    several directories the others are still listed. Exit codes 6 (the
    chain is invalid) and 7 (the chain's files changed since a given
    fingerprint) join the table for the `rx logs` commands that read one
    chain.
  - `GET /v1/logs/chains?path=DIR` lists the log chains of a directory:
    the files of each rotated log (`syslog`, `syslog.1`, `syslog.2.gz`,
    `syslog-20260401-1775001601.gz`, `app.1.log.gz`,
    `app-2026-10-01.3.log.gz`, …), found from their names alone by a
    table of four name templates (numbered, dated, and both with the
    number or date before the extension). Each entry gives the chain's
    handle (its directory joined with the active file's name), its parts
    oldest first by the number or date in their names, whether the active
    file exists, the missing numbers (`missing`, at most 100 names, and
    `missing_count`, how many are missing in all), the total size, the
    compression formats and whether every frozen part has a line index
    built from the file the listing found (an empty part needs none).
    A four-digit number from 1970 to 2100 where a rotation number goes
    (`report.2023`) is a year, so yearly files are ordered by year and
    name no missing parts; missing numbers are named only when no more
    are missing than numbered parts are present. One generation in several encodings is
    one part (plain, seekable zstd, zstd, gzip, bzip2, xz, by the bytes);
    directories, hidden entries, `.tmp` files and files that are not text
    are no parts, so `wtmp` and `wtmp.1` form no chain; a file that cannot
    be opened stays a part and is named in `unreadable`; a chain needs two
    parts and may have 10,000. A larger chain is listed with
    `too_many_parts` and no parts, and its files are checked only until
    that is known. Errors as `GET /v1/tree` for the same path.
    `GET /health` lists the feature `log_chains`. Contract 1.7.
  - `GET /v1/logs/chain?path=HANDLE[&file_tz=ZONE][&fingerprint=FP]`
    describes one log chain: its parts in time order (by first
    timestamp; an empty part keeps its place), each with its line count,
    first, last and highest timestamp (`max_ms`, an upper bound marked
    `max_is_bound` under `file_tz` in a part that writes several zone
    offsets), `global_start` (the chain's number of its first line),
    compression, size, `is_indexed`, time format, its first timestamp as
    its line writes it (`example`) and `day_first` (as `GET
    /v1/time-range` gives them, so a client shows the part's times in its
    own layout), and duplicates; the
    chain's state (`pending` until every frozen part has a current line
    index, `ready`, or `invalid` with `reasons`: `no_timestamps`,
    `overlap` with `overlap_ms`, `active_not_last`, `unreadable` (worded
    as a search words the failure in `skip_reasons`, never with the
    error's own text), `too_many_parts`, which lists no part), its first
    and last time, `frozen_line_count` and
    `line_count`, the time gaps (with four parts or more, where a part's
    end and the next part's start are more than 1.5 times the median
    distance between first timestamps apart), the missing parts and
    `missing_count`, and a fingerprint of its files (16 hex digits; the active file growing does
    not change it). It reads each frozen part's stored index and the head
    and tail of the active file, never a whole part; descriptions of
    chains whose frozen parts are all indexed (or empty) are kept in
    memory (64 chains, 40,000 parts in all), keyed by a digest of every
    part's stat with its ctime, and a kept description is dropped when a
    frozen part's index file was removed or rebuilt, and not used by a
    request whose listing could not open a frozen part.
    A pending chain starts its index task in the background (or joins the
    running one) and names it in `index_build`, unless 128 chains' index
    tasks run or wait already, when it names none and says why in
    `index_build_refused` (null in every other case); otherwise
    `index_build` names the chain's last index task. How that task ended is kept with
    the chain for `RX_TASK_TTL_MINUTES` after its end while the chain's
    files keep their fingerprint, even once the task table has dropped
    the task; a failed task's `message` names the part and the error.
    Year-less timestamps take their year from their own part. 409 with
    the current description when `fingerprint` differs or a part is
    replaced while the request reads it; 404 when the handle names fewer
    than two parts; 400, 403 and 422 as the other routes.
  - `rx logs show CHAIN... [--json] [--file-tz=ZONE] [--fingerprint=FP]`
    prints each chain's description: its state, reasons and fingerprint,
    then a table of its parts in order (name, compression, lines, global
    lines, first time, highest time, `idx`), its gaps and missing parts;
    `--json` prints the `GET /v1/logs/chain` body. It never waits: a part
    without a line index is indexed in memory. A chain of more than
    10,000 parts says `too many parts (N)`. `rx logs time-range
    CHAIN... [--json] [--file-tz=ZONE]` prints each chain's first and last
    time in the layout of `rx time-range`. Both write times in the layout
    the parts write them (`Sep 29 00:00:00`, `2025-12-10 07:00:30`) when
    every part with lines writes them one way, as `rx time-range` writes
    one file's, and to the millisecond otherwise. Exit 3 when a handle names no
    chain, 6 when a chain is invalid, 7 when `--fingerprint=` differs
    (each after printing), 2 for a malformed fingerprint or one given
    with several chains.
  - `POST /v1/logs/index?path=HANDLE[&force=true][&fingerprint=FP]`
    starts the index task of a log chain, or joins the one running for
    it, and answers 200 with the task. The task (operation `chain_index`
    at `GET /v1/tasks/{id}`, its `path` the handle) builds and stores the
    line index of every part without a current one, the active file too
    (with `force=true`, of every part), whatever a part's size, through
    the same background builds a samples lookup starts: at most
    `RX_MAX_INDEX_BUILDS` at a time, the next part submitted as one ends,
    so a chain of thousands of parts never fills the build queue, and the
    part builds of all chains together, queued or running, take at most
    half of it (128), so a lookup in another file finds room however many
    chains are pending; a chain that finds no room waits in line, and
    each build's end lets one waiting chain go on, not all of them. At
    most 128 chains' index tasks run or wait at once: past that the
    request starts nothing and answers `503` with `Retry-After: 5`. Each
    part build is a task of its own; finished part builds are kept apart
    from other tasks (at most 256 of them), so a large chain never drops
    another client's task from the task table. Its
    `progress` is the share of parts done; it fails with the first part
    whose build fails, naming the part, as the build itself reports it
    (whether or not the table still holds the build's task); its result
    (`ChainIndexTaskResult`) lists the parts it built and gives
    `rx logs index HANDLE` as `cli_command`. One task per chain: it is
    keyed by the chain directory's device and inode and the chain's name,
    so handles that reach one directory by different paths (a symbolic
    link, another case on a case-insensitive disk) share it, and a task
    on the active file runs beside it; on a filesystem that gives inode 0
    the key is the handle.
    The task `GET /v1/logs/chain` starts for a pending chain is the same
    task, for the parts the chain waits for; it is not started again on
    describe while the last one failed for the same files. 409 with the
    current description and no task when `fingerprint` differs; 400, 403,
    404, 422 and 500 as `GET /v1/logs/chain`; 503 with `Retry-After: 5`
    past the 128 tasks.
  - `rx logs index CHAIN... [--json] [--force]` builds and stores the line
    index of every part of each chain, the active file too, in the
    foreground in the chain's order, whatever a part's size
    (`RX_LARGE_FILE_MB` does not apply), keeping a current index unless
    `--force`; it reports each part as `rx index` reports a file,
    counted as parts (`Indexed 4 parts in 0.1s`), then prints the chain
    as `rx logs show` does. `--json` prints per chain
    `{path, indexed, skipped, skip_reasons, errors, total_time, chain}`.
    Exit codes as `rx index`, and 3 when a handle names no chain.
  - `GET /v1/logs/samples?path=HANDLE&(lines=SPEC[&part=NAME]|timestamps=T...)`
    gives lines of a log chain as `GET /v1/samples` gives a file's: by the
    chain's global line numbers (`-N` from the chain's end), by a part and
    its own numbers (`part`), or by time (the first line in the chain's
    order at or after T, found in the first part whose highest time
    reaches it; a range runs to the line before the first line later
    than T2). Each key's lines come as pieces, one per part its window
    touches, each with the part, its first local and global line, the
    lines, their `line_timestamps`, `part_start`, `part_end` and the
    `rx samples PART --lines=A-B` command for exactly those lines. Context
    crosses part edges in a ready chain, and so does the look back of
    `line_timestamps`: lines that continue at the start of a part a record
    the part before began carry its timestamp, from the earlier part's
    line index, as the parts read as one file give them; they carry none
    (`null`) when that index does not give the timestamp in the
    `file_tz` zone (a part whose zone offset changes more often than the
    index records), and a line at exactly the look back's distance
    carries none when the earlier part's last byte decides and reading it
    would decode more than is left of `RX_SAMPLES_MAX_BYTES`. Before the
    chain is ready a part is read alone (`part` and `lines`, global numbers
    -1; `index_build_refused` says why the chain has no index task when
    none could start), and a request by global line or by time waits for
    the chain's index task, `202` with the task under
    `Prefer: respond-async` once `RX_SAMPLES_WAIT_SECONDS` has passed,
    `503` with `Retry-After: 5` when the task does not run and cannot
    start because 128 chains' index tasks run or wait. Parts are read
    through the path of `GET /v1/samples` (their index, an answer from
    the head while a part's index builds, `202` for a part build that
    outlasts the wait). `RX_SAMPLES_MAX_LINES` and `RX_SAMPLES_MAX_BYTES`
    bound the whole answer. 409 with the current description when `fingerprint`
    differs or a part changed while it was read; 422 for an invalid
    chain (the detail lists the reasons); 404 when the handle names fewer
    than two parts; 400 for a part that is not a member and the samples
    parameter errors.
  - `rx logs samples CHAIN (--lines=SPEC [--part=NAME] | --timestamps=T...)
    [--context=N] [--before=N] [--after=N] [--file-tz=ZONE]
    [--fingerprint=FP] [--json]` gives the same answer from a terminal,
    without waiting for background work (the chain is described as
    `rx logs show` describes it); each line is printed with its global
    number and `part:local`, and a `-- NAME --` line where a part's lines
    start. Exit 3 when the handle names no chain, 6 when it is invalid, 7
    when `--fingerprint=` differs or a part changed while it was read, 2
    for a part that is not a member and the usage errors of `rx samples`.
  - `GET /v1/logs/trace` takes the parameters of `GET /v1/trace` and
    searches rotated logs: each `path` is a directory (the files of each
    directory its walk lists are grouped into log chains, as
    `GET /v1/logs/chains` groups them), a chain's handle (even without an
    active file), or a file (a part's own path is a file); a handle or a
    file opens only the files that can belong to the chain of its name,
    never the rest of its directory. What a request reaches more than
    once (a path given twice, a directory and a handle or a part in it, a
    link to a directory, a directory under another case on a
    case-insensitive disk or through a bind mount) is searched once: one
    entry per chain, one file id per file, each match once and in its
    chain. A chain is known by its name and the device and inode its
    directory had when it was listed (by its path where the filesystem
    gives inode 0, and then also by its parts' paths with every link
    resolved), and a part as one directory entry (the directory's device
    and inode, the part's name, the file's whole stat), so one directory
    under two spellings is one chain whose parts are searched once; two
    chains that still give one identity are both searched, the second's
    parts that are not the first's as files of their own, never left
    out. Device and inode alone never make two paths one file: where the
    search cannot tell, it searches both, so a line may come twice but
    never goes missing, and every order of the same paths gives the same
    answer. The parts of
    each chain are searched in the chain's order (by time once it is
    ready, by name before) by the trace engine, where the walk met the
    chain's first file, so the file ids, the order of the matches and the
    `max_results` cut follow it; context never crosses a part's edge, and
    each part keeps its own trace cache entry and line index. The answer
    is the trace answer plus `chains` (`c1`, `c2`, …: handle, name, the
    parts' file ids in order, fingerprint, state, reasons), and each match
    gives `chain` and `chain_line`, its global line (`-1` for a file of
    its own, a pending or invalid chain, and a match the trace left
    unnumbered). Another encoding of a part is skipped with the reason
    `duplicate_part: …`, also when it is named on its own beside its
    chain, whichever comes first, under its own path, through a link to
    it or under another spelling of its directory (never both searched
    and skipped, so each of its lines comes once, in the chain; a hard
    link of it named on its own elsewhere is a file of its own, a link
    to it that is another chain's part is searched in that chain, and a
    path that is it under another name — a link to it, or the file it
    links to, named on its own or listed by a walk — is named in
    `skipped_files` too, under its own path),
    a part that cannot be read with its read error,
    and a chain of more than 10,000 parts is searched as files of their
    own. Chains are described from their parts' indexes, and no index
    build starts. 409 when a part changed while its chain was described;
    the other statuses as `GET /v1/trace`. The webhooks fire with the
    payloads of a trace. `GET /v1/trace` is unchanged.
  - `rx logs trace PATTERN [CHAIN|DIR|FILE ...]` with every flag of
    `rx trace` gives the same search from a terminal: a match in a part
    prints as `CHAIN:LINE (PART:LINE): TEXT`, one in a file of its own as
    `FILE:LINE: TEXT`, and `--json` prints the `GET /v1/logs/trace` body.
    Each chain is described from its parts' stored line indexes, as the
    route describes it, never by reading a part, so a search capped with
    `--max-results=` reads what `rx trace` reads on the same files; a
    chain with a part not indexed yet is pending, its matches print `?`
    as their line in the chain, and stderr says
    `run rx logs index -- CHAIN for chain line numbers` (`--` before
    the handle, so a handle that starts with a dash pastes as it is). An
    invalid chain is named on stderr. Exit 3 for a path that is no
    directory, chain or file, 4 outside the search roots or for a
    named file that cannot be read, 7 when a part changed while its chain
    was described, 2 for a pattern that does not compile, `-` and the
    usage errors of `rx trace`.
  - `RX_CHAIN_OVERLAP_SECONDS` (default 60, 0 to 86400): how far a part
    of a log chain may reach past the first timestamp of the next part
    before the chain is invalid.
- `rx samples --file-tz=ZONE` (all three modes) and
  `rx time-range --file-tz=ZONE` read a file's timestamps as the wall
  clock each line writes, in ZONE, for a log whose zone is missing or
  wrong: a zone a line writes is ignored, and ZONE takes the place of
  `RX_LOG_TZ` for the command. It moves the time search,
  `line_timestamps`, `time_format.assumed_zone` (ZONE), and every member
  of the time range (`display_zone` is ZONE; `first_ms` and `last_ms`
  are read in it); `has_zone` stays the file's, and a query without a
  zone is read in ZONE unless `RX_QUERY_TZ` is set. ZONE is `UTC`, an
  IANA name or `±HH:MM`, as `RX_LOG_TZ` takes it; another value exits 2
  naming it. Nothing stored changes. The index of a file whose
  timestamps carry no zone serves it as before. The index of one whose
  timestamps carry zones holds instants and records where the written
  offset changes (`zone_offsets`), so the query is shifted by each
  stretch's offset: the time search still starts at a checkpoint (about
  one index step per stretch of one offset, and no line read twice for
  one query however many offset changes one step holds), and the time range comes
  from the index with no read, for a gzip, bzip2, xz or plain zstd file
  too. A file whose offset changes too often for 1,024 entries is searched
  from its first line under `--file-tz`, and its time range reads the
  last timestamped line again at the offset the index stores (source
  `none` for a stream-compressed file). An epoch value counts as its
  UTC wall clock. The human `rx time-range` line shows the times in
  ZONE.
- `file_tz=ZONE` on `GET /v1/samples` and `GET /v1/time-range` does
  what `--file-tz` does; a value that names no zone answers `400`
  naming it (its first 64 bytes when it is longer), and `cli_command` renders `--file-tz=…`. `GET /health`
  lists the feature `file_tz`. Contract 1.6.
- `rx serve --update-viewer` checks GitHub for a newer `rx-viewer`
  release inside the supported range at this start, whatever the last
  check says. Together with `--skip-frontend` it exits 2.
- The `rx serve` banner names the viewer served and why: `Viewer:
  v0.6.0 (cached)`, `(updated from v0.2.0)`, `(installed)`,
  `(set by RX_FRONTEND_URL or RX_FRONTEND_VERSION)`, or `Viewer: none
  (/ redirects to /docs)`. A failed download or check prints one
  warning line that ends with the same text.
- `RX_SAMPLES_HEAD_MB` (default 64, 0 to 4096; 0 switches it off): how
  many MiB of a file's text, from its first byte, a samples lookup may
  read to answer without a line index when the file wants one and has
  none.
- `RX_MAX_INDEX_BUILDS` (default 2, 1 to 64): how many line-index
  builds that `GET /v1/samples` and the index tasks of log chains start
  run at once in `rx serve`. A
  build past it waits in a queue, in order, as a task whose status stays
  `queued` until a running build ends; lookups name it and wait for it
  as before, and a lookup in the head of its file is still answered at
  once. The queue holds at most 256 builds; past that a lookup starts
  none and one that needs the index reads the file without it. Builds
  started by `POST /v1/index` are not counted. Before, a lookup in each
  large file of a tree started a whole-file build on every one at once.
- `index_build` in every `GET /v1/samples` answer (and `rx samples
  --json`, always `null` there): the background build of the file's
  line index that the answer started or joined (`task_id`, `status`,
  `message`, `path`, `started_at`), to follow at `GET /v1/tasks/{id}`;
  `null` when no build runs or the request waited for it. Contract 1.6.

### Changed

- `rx samples` on a file that wants a line index and has none (any
  compressed file, a plain file of `RX_LARGE_FILE_MB` or more) answers a
  lookup whose lines all lie in the first `RX_SAMPLES_HEAD_MB` MiB of
  its text (decompressed for a compressed file) from that head, at once,
  and builds no index; the answer is the one the index gives. It used to
  build the index first, reading the whole file. A position counted back
  from the end (`--lines=-1`), anything past the head, a time range
  open to the end and a time of day without a date build the index first
  as before. `rx index` builds it on its own.
- `GET /v1/samples` answers such a lookup from the head the same way, at
  once, instead of waiting for the index build (or answering `202`), and
  starts the build in the background, or joins the one running, naming
  its task in `index_build`. A refusal the head can give (an answer over
  the limits, a time named wrongly) is answered at once and starts no
  build. On a 6.7 GB log with no index, the first `lines=1-1000` took
  3.2 s waiting for the build; it now takes 0.01 s. Once a lookup has
  read the head and found its lines past it, the server keeps how far
  the head of that version of the file reaches for as long as the build
  runs: a later lookup of a line or an offset past that waits for the
  build (or answers `202`) without reading the head again, and one
  inside it is still answered from the head. A time is placed only by
  reading, so a time past the head still reads the head each time.

- Line index format 10: the time section records `zone_offsets`, where
  the zone offset the lines write changes, as `[line, minutes]` pairs
  from the first timestamped line on (`[[first, 0]]` for a file whose
  timestamps carry no zone; a line that writes no zone counts as 0;
  `null` when 1,024 entries cannot hold them), and `max`, the line with
  the highest timestamp of the file (`{ms, line, offset}` like `first`
  and `last`; the first of them when several lines share the highest
  value; `null` when no line has a timestamp). `max` is not `last` when
  the lines go back in time, and it covers the lines after the last
  checkpoint, which `max_before` does not. `rx index --json` shows both.
  A stored index whose list is out of order, names a line outside the
  timestamped lines, repeats the offset in force or holds an offset
  beyond 18 hours, or whose `max` is missing, names a line outside the
  timestamped lines, is below `first` or `last`, has an offset out of
  the order of the lines of `first`, `max` and `last` (equal offsets
  exactly for equal lines), or disagrees with `max_before` (each entry
  after `max`'s line must be `max`'s value, each one up to it null or
  below it), is treated as damaged. Every index stored by an earlier
  version is rebuilt once.

### Fixed

- On Linux, a file that was checked and then replaced by a named pipe
  before rx opened it is refused as changed, as on macOS. Linux gives
  the new pipe the removed file's inode number, so the device-and-inode
  check alone took the pipe for the checked file; the check compares the
  file type too.
- The task table of `rx serve` counts only finished tasks against its
  cap of 256. Queued and running tasks used to count too, so with more
  than 256 of them unfinished at once (a full queue of samples index
  builds, many log chains waiting to be indexed), starting any task
  dropped every finished one, and `GET /v1/tasks/{id}` answered `404`
  for a task a client was still following. A queued or running task is
  still never dropped.
- `rx trace` and `GET /v1/trace` order files and patterns by the number
  in their id, not as text: the matches of the tenth file given (`f10`)
  come after those of the second (`f2`), and on one line `p10` comes
  after `p2`. The cut to `--max-results` / `max_results` runs after this
  order, so with ten or more files or patterns it now keeps the matches
  of the first files and patterns given, where it used to keep `f10`'s
  before `f2`'s. `match_found` webhooks go out in this order. The human
  output lists the patterns in it too, and its context section lists the
  files in the order of the match list, where it used to sort them by
  path.
- A request whose client went away before the answer is logged and
  counted as status `499`, not `500`: the `http_request` log line and
  `rx_http_responses_total{status_code="499"}` report it, the trace and
  samples counters count it as `canceled`, and `rx_errors_total` does
  not count it. The status a client receives is unchanged; only a
  server error written after the client left is reported this way.
- `rx serve` serves the directory `RX_FRONTEND_PATH` names as it is.
  The daily viewer check used to treat it as the viewer cache: on a
  local `rx-viewer` build without `.metadata.json` it downloaded the
  newest release and replaced the build's files. Now rx asks GitHub
  nothing for that directory, downloads nothing into it, writes no
  `.metadata.json` in it, does not create it and applies no range check;
  the banner reads `Viewer: <version> from <dir> (RX_FRONTEND_PATH,
  served as it is)`, with the version from the build's `version.json`.
  A directory without `index.html` and `assets/`, or a missing one,
  serves no viewer and prints one warning, where it used to receive a
  download. `RX_FRONTEND_URL` and `RX_FRONTEND_VERSION` are ignored
  beside it, with a warning, and `--update-viewer` exits 2. To move the
  cache rx manages, set `RX_CACHE_DIR`.
- A zone name in `--file-tz`, `file_tz`, `RX_LOG_TZ` or `RX_QUERY_TZ`
  is accepted only as the zone database spells it, letter case
  included. On macOS, whose file system ignores case, `utc` and
  `europe/berlin` used to be accepted while Linux refused them; a name
  with an empty or `.` component (`Europe//Berlin`) is refused too. An
  environment variable with such a value keeps its default, with the
  usual `invalid_setting` warning.
- A stored line index whose time section holds a value outside the
  years 1 to 9999 (`first.ms`, `last.ms` or a `max_before` entry) is
  treated as damaged, as any index rx could not have written: a search
  under `--file-tz` adds up to 18 hours to those values, which could
  wrap on a tampered index.
- Upgrading `rx` never upgraded its viewer: `rx serve` served a cached
  viewer for ever. Now, with no `RX_FRONTEND_URL` or
  `RX_FRONTEND_VERSION` set, it asks GitHub once a day (when
  `.metadata.json`'s `last_check` is a day old, missing or unreadable)
  for the newest published release inside the supported range and
  installs it when it is newer than the cached one, before the server
  binds. The release list request has a 10-second limit when a viewer is
  cached. A failed check (offline, an error status, the rate limit, a
  release without `dist.tar.gz`) keeps the cached viewer, prints one
  warning, records `last_check` and never stops the server.
- With no cached viewer, `rx serve` came up without one when GitHub's
  latest release was past the supported range. It now reads the release
  list, skips drafts and pre-releases, and installs the newest release
  inside the range. A cached viewer outside the range is treated as no
  cache: it is replaced, or not served when no release inside the range
  can be installed.
- `rx serve` installs viewers 0.6.0 and 0.7.0: the range of viewer
  releases it accepts is now `0.2.0 <= v < 0.8.0`. Release 0.5.0 refused 0.6.0 and
  came up without the viewer unless one was already cached or
  `RX_FRONTEND_VERSION` pinned an older one.
- The `just` recipes that take arguments (`run`, `test`, `test-repeat`,
  `bench`, `serve`, `parity`, `release`, `release-dry`) pass each one on
  as one word. They used to paste the arguments into a shell line, so
  `just run trace 'a b' app.log` searched for `a` in the files `b` and
  `app.log`, and `just test -run='A|B'` ran `B` as a command.
- A tag name no longer runs as shell code on the release path. git
  allows a quote, `;`, `$(` or a backtick in a tag name, and a tag such
  as `v1.0.0';id;'` matches the `v*.*.*` filter of `release.yml`, whose
  job may write releases. That workflow now stops at its first step
  unless the pushed tag is exactly `vX.Y.Z`. `just release-notes`
  refuses a version that is not `X.Y.Z` (exit 2) and finds the section
  by plain text, not by a pattern. `just build`, `build-all` and
  `version` read the version from `git describe` as the variable
  `BUILD_VERSION`, so it stays one word; they stamp the same string as
  before. The release binaries stamp exactly the pushed tag
  (`just --set version "$GITHUB_REF_NAME" build-all`), and the job fails
  unless `rx --version` prints exactly `rx version <tag>`: `git describe`
  prefers an annotated tag, so a second tag on the release commit, such
  as `a';id;'v1.0.0`, was stamped in its place and passed the old check,
  which only looked for the tag name inside the version.

## [0.5.0] - 2026-10-06

### Added

- `GET /health` lists `features`: the names of the features this build
  serves, sorted (`trace_matching_flags`, `trace_context_and_switches`,
  `samples_index_build`, `samples_timestamps`, `line_timestamps`), so a
  client checks for a name instead of comparing contract versions.
  Part of contract 1.5.
- `GET /v1/time-range?path=…` and `rx time-range PATH… [--json]` give a
  file's time range: its timestamp `format`, `has_zone`, `day_first`,
  the zone its lines show times in (`display_zone`: `RX_LOG_TZ` for a
  file whose timestamps carry no zone, the first timestamp's offset for
  one whose timestamps do), its first timestamp as written (`example`),
  the first and last timestamp as UTC instants (`first_ms`, `last_ms`,
  read in `RX_LOG_TZ` as `line_timestamps` reads them) and `source`:
  `index` when the line index holds them (nothing of the file is read),
  `scan` for a plain or seekable zstd file without one (its first
  mebibyte, and at most 16 MiB back from its end, decoding at most
  32 MiB of a seekable file's frames; past either `last_ms` is `null`),
  `none` for a gzip, bzip2, xz or plain zstd file without
  one, which a request never decompresses whole. A request writes
  nothing to the cache; `rx time-range` builds the index of such a
  stream first, as `rx samples` does. The command prints one line per
  file with the first and last timestamp in the file's own layout and
  zone, and `--json` prints one object for one path, an array for
  several. `features` lists `time_range`. Part of contract 1.5.
- `RX_SAMPLES_MAX_LINES` (default 100,000, from 1,000 to 10,000,000)
  caps the lines one `GET /v1/samples` answer holds, summed over its
  samples. A request whose answer would hold more is refused with
  `400`, naming the setting and the count reached; the lines are
  counted as they are read, so it stops reading there. A thousand ranges
  open to the end of a 5 MB file used to hold 34 million lines and
  allocate 7.8 GB. `RX_SAMPLES_MAX_BYTES` (default 256 MiB, from 1 MiB
  to 16 GiB) caps the bytes of their text the same way, since a log's
  lines can be megabytes long; a line longer than it is never held
  whole, also on the way to an offset after it. `rx samples` has no
  limit.
- Every line index records the timestamps of the file's lines in a time
  section, `time_index`: the detected format (ISO 8601 and its everyday
  relatives, the access-log, `ctime` and syslog forms, slash and dotted
  dates, epoch values), the first and the last timestamped line, how
  many lines carry a timestamp, how many step back by more than a second
  (`backward_steps`, `max_backward_ms`), and for each checkpoint the
  latest timestamp before it (`max_before`), and the first timestamp as
  its line writes it (`first_text`: printable ASCII, other bytes written
  as `\xHH`, at most 64 bytes). The format is decided from
  the first mebibyte of the text; `time_index` is `null` when none is
  recognized. A few crafted lines cannot steer the choice: lines that
  start with a timestamp outrank a timestamp further into each line only
  when they are at least 1% of the lines, and a slash date is read day
  first only when at least three lines read only that way and outnumber
  those that read only month first. A timestamp outside the years 1 to
  9999, read with or without its zone, counts as none. Plain, compressed
  and seekable zstd files are covered, by
  the same pass that builds the line index, with or without `--analyze`.
  The first mebibyte of a zstd file is decompressed a stream at a time
  with at most a 128 MiB window, the most `zstd -d` accepts without
  `--long=N`; a file whose frames need more cannot be indexed, and the
  build names the frame.
  Values are milliseconds in the file's frame: UTC instants for a file
  whose timestamps carry zones, the wall-clock time as written for one
  whose timestamps do not (a line with a zone in such a file keeps the
  wall clock it shows); a syslog timestamp takes its year from the
  file's mtime. `rx index --info` prints the format and how its zone is
  read, the first and last timestamps, the count of timestamped lines
  and the backward steps; `rx index --json` gives `time_index` for each
  indexed file, and `--info --json` gives it with the rest of the index.
  `GET /v1/index` and the result of a `POST /v1/index` task carry
  `time_summary`: `{format, has_zone, first_ms, last_ms,
  timestamped_lines, backward_steps, max_backward_ms}`, or `null` for a
  file with no timestamp format (`max_before` is left out). Part of
  contract 1.5.
- `rx samples --timestamps=T` (`-t`) finds lines by time: the line at T
  is the first line, in file order, whose own timestamp is T or later,
  printed with its context as `--lines` prints a line. `T1..T2` gives
  the lines from the line at T1 to the line before the first line later
  than T2, without context; `..T2` and `T1..` leave an end open; both
  ends are inclusive to the millisecond. A time may be ISO 8601 or
  RFC 3339, a date, a time of day (dated from the file when its first
  and last timestamps fall on one date, refused with both dates
  otherwise), epoch seconds or milliseconds, or a timestamp copied from
  a line. The flag repeats, and a value is never split on commas
  (`2025-12-10 12:34:56,123` is one value); at most 1,000 per request.
  It cannot be combined with `--lines` or `--offsets`, and a value that
  is not a time or a file without a timestamp format exits 2. A time no
  line reaches answers `-1` and a `null` sample, with a warning, as a
  line past the end does. With an index the search starts at the
  checkpoint whose `max_before` says the answer is past it and reads at
  most one index step; the line and its context are then read by the
  `--lines` code, so the two agree. Plain, compressed and seekable zstd
  files answer the same. `--json` adds `timestamps`, each query mapped
  to the line it found (a range to its first line), `{}` in the other
  modes.
- `GET /v1/samples?timestamps=T` answers the same time queries over
  HTTP: the parameter repeats (`?timestamps=A&timestamps=B`), is never
  split at commas, and cannot be combined with `lines` or `offsets`
  (400). An invalid value, a time of day on a file of two dates, a file
  without timestamps and more than 1,000 values are 400 with the reason.
  The request builds or waits for the line index as a `lines` request
  does (202 with `Prefer: respond-async`), and `cli_command` renders one
  `--timestamps=…` per value. The 400 for a request with no address now
  reads "Must provide one of 'offsets', 'lines' or 'timestamps'.".
  `timestamps` and `time_format` in the answer, and the parameter, are
  part of contract 1.5.
- Every `samples` answer reports the file's timestamp format as
  `time_format`, `{format, has_zone, assumed_zone}`, or `null` when none
  is recognized in the first mebibyte. With an index it costs nothing;
  without one, an answer now also reads the head of the text (at most
  1 MiB) to detect it.
- `RX_LOG_TZ` (default `UTC`) names the zone a file's zone-less
  timestamps were written in, and `RX_QUERY_TZ` the zone a time query
  without a zone is read in (unset: the file's own frame, so a time
  copied from a line finds it; `local`: the process's zone). Each takes
  `UTC`, an IANA zone name or `±HH:MM` (a sign, then two digits
  in each field); another value keeps the default
  with an `invalid_setting` warning. The zone database is built into
  the binary (about 450 KB), so zone names work without
  `/usr/share/zoneinfo`.
- Every `samples` answer gives each sample line its time in
  `line_timestamps`, keyed and ordered as `samples`, in all three modes
  (CLI `--json` and HTTP): milliseconds since the Unix epoch as a UTC
  instant (a zone-less file's wall clock read in `RX_LOG_TZ`), or `null`.
  A line without a timestamp of its own, such as a traceback line,
  carries the timestamp of the nearest earlier line that has one when
  that line starts at most `RX_TIMESTAMP_LOOKBACK_KB` KiB before it
  (default 64, from 0 to 1024; 0 gives such a line none). A key whose
  sample is `null` maps to `null`, and the field is `null` for a file
  without a timestamp format. When a sample's first line has no
  timestamp, the answer reads back at most that many KiB before it,
  never through the index; a gzip, bzip2, xz or plain zstd file
  decompresses its text up to that line once more for it. Human output
  does not change. Part of contract 1.5.

### Changed

- Line index format 8. An index stored by an earlier rx is treated as
  absent and rebuilt once, the first time a command needs it. A stored
  index whose time section cannot be trusted (a `max_before` that does
  not match the checkpoints or decreases, an unknown format, a negative
  count of timestamped lines, a first or last timestamped line missing
  or outside the file's lines, or a zone offset beyond 18 hours) is
  treated as damaged: absent, with an `index_unreadable` warning.
- An index build reads each line where its read buffer holds it instead
  of copying it, which pays for reading the timestamps on lines of a few
  hundred bytes: a 465 MB log (1.4 million lines) builds in 120 ms with
  the time section, against 133 ms for format 7 without it, and peaks at
  16 MB instead of 21 MB. A log of short lines pays for it: a 6.3 GB log
  of 42.5 million 150-byte lines takes 2.7 s instead of 2.3 s.

### Fixed

- A `line_timestamps` read back that fails (a damaged frame of a
  seekable zstd file before a sample, which the sample's own lines do
  not need) no longer fails the whole samples answer: that sample's
  lines without a timestamp of their own are `null`, and the failure is
  logged as `line_timestamps_read_back_failed`.
- `samples` reads every line window of a request in one pass over the
  text, in order of position, instead of one pass per window: by line
  (`--lines`, `?lines=`) and by time. Without an index, 1,000 lines near
  the end of a 5 MB file read 5.5 GB; they now read the file once. With
  an index the pass seeks over the gaps between windows, and a seekable
  zstd file decodes each frame it needs once. `GET /v1/samples` stops
  reading when its client disconnects. A negative `--after` on a
  compressed file now reports the asked line's offset, as a plain file
  does, and the line after the last line of a plain file that ends with
  a line break reports `-1` with its context, as the compressed copies
  do, rather than the file's size.
- Reading a seekable zstd file by position (the read back of
  `line_timestamps`, the read back from the end for a time query)
  decodes each frame once per answer. The read back for each sample
  decoded every frame it touched again: 200 samples on a file of 4 MiB
  frames decoded gigabytes.
- A time query that reads back from the end of a file for its last
  timestamp reads each line of the tail once. A tail of lines whose
  128th byte was the `\r` of a `\r\n` cost one more 4 KiB read per
  line, 23 times the text on a plain file; on a seekable zstd file each
  of those reads decompressed a whole frame.
- Deciding what a zstd file is no longer costs the memory its frame
  header asks for. The 8 KiB text probe decompresses a stream at a time
  and holds one window of at most 16 MiB: a 28 KB file whose one frame
  declares 256 MiB used to cost 256 MiB in every command and in each
  `GET /v1/tree` that listed it, and a single-segment frame could declare
  up to 64 GiB. `GET /v1/tree` takes a zstd file whose first frame
  declares a window above 16 MiB (`zstd --long` writes 128 MiB) for text
  without probing it. Every command that goes on to read the file
  (trace, samples, index, compress) probes it with the window it reads
  with, at most 128 MiB, so a binary file stored that way is still
  skipped or refused as binary rather than searched with `rg --text`.
- An xz file no longer costs the dictionary its block headers declare
  before rx has checked it. An xz block header names the dictionary its
  decoder reserves, up to 4 GiB, so a 64-byte file whose header named
  2 GiB cost 2 GiB in every command and in each `GET /v1/tree` that
  listed it. rx now reads the xz container itself and checks each block
  header, of every block, before the dictionary is reserved.
  `GET /v1/tree` decodes blocks that declare at most 16 MiB and takes a
  file whose first block declares more for text without probing it
  (`xz -9` declares 64 MiB); every command probes and reads xz with at
  most a 128 MiB dictionary and refuses a block that declares more.
- An xz block whose header declares the size of its text now reserves
  a dictionary of that size (at least 4 KiB) instead of the one the
  header names, which no match in the block can use. A 99 KB file of
  5,500 empty blocks that each declared their size and named 128 MiB
  cost `rx samples` 18 s and 420 MB; it now costs 0.08 s and 21 MB.
  `xz -T` writes the size in every block, so its files decode with
  less memory, to the same text. A block that leaves the size out
  still reserves what it names, up to the 128 MiB limit.
- An xz file cut short where a block or its index should start was read
  as complete, its text up to the cut answered as all of it; it is now
  a stream that ends early.
- `rx trace`, `rx samples` and index builds no longer hold whatever a
  small zstd file declares. A seek table can give one frame up to 4 GiB
  of text, which each of them decoded whole (a 14 KB file with one
  129 MiB frame cost a trace 138 MiB, a samples lookup 148 MiB and an
  index build 217 MiB), and a plain zstd stream could declare a 512 MiB
  window or a single-segment frame of 64 GiB. Every zstd decoder now
  refuses a frame whose window, or single-segment size, is above
  128 MiB before reserving it, the limit `zstd -d` applies without
  `--long=N`. A file refused this way, or for an xz dictionary above
  128 MiB, is answered by none of its parts: `rx trace` and `rx index`
  skip it with `decompressing it needs more than 128 MiB at once: …` in
  `skip_reasons` (it was a stream that "ends early" with the matches
  before the refusal kept, or an index error), `rx samples` exits 1 and
  `GET /v1/samples` answers 400 with the same words. A seekable file
  whose seek table gives a frame more than 128 MiB is read as the
  plain zstd stream it also is, a window at a time, with the same
  answers (the 129 MiB frame above now costs 18 to 29 MiB); a trace
  logs `seek_table_unused` for it.
- Building the index of a seekable zstd file no longer holds a slice
  for each line of a frame. A frame of 128 MiB, the most rx decodes at
  once, can hold 134 million empty lines and compress to a few KB; the
  build kept 24 bytes for each, so a 98 KB file of 24 such frames cost
  `rx index`, and the `rx samples` or `GET /v1/samples` that builds the
  index first, almost 10 GB. The build now finds the line breaks where
  the frame holds them and keeps only its checkpoints, one per 10,000
  lines: the same file costs 556 MB, and every index is the same as
  before.
- An index build no longer copies the first lines of a file to decide
  its line ending; it counts the endings where it reads them. A file
  whose first line is 10 MiB long cost 10 MiB more. The answer is the
  same.
- `rx serve` installs viewer 0.5.0: the range of viewer releases it
  accepts is now `0.2.0 <= v < 0.6.0`. Release 0.4.0 refused 0.5.0 and
  came up without the viewer unless one was already cached or
  `RX_FRONTEND_VERSION` pinned an older one.

## [0.4.0] - 2026-10-05

### Added

- `rx trace --json` and `GET /v1/trace` give `skip_reasons`: one
  `{path, reason}` for each path of `skipped_files`, in the same order,
  the shape `rx index --json` gives its `skip_reasons`. A reason names a
  file that is not text (`not a text file: …`), one the process may not
  read (`permission denied`), a link the walk refuses, a file whose
  stream ends early or whose seekable frame is damaged (`not searched in
  full: …`, its other matches kept, the damaged frame named), and a
  matched line no pattern could be credited with. The human output lists
  each skipped path with its reason under `Files skipped:`. A
  subdirectory the process may not list is now listed in
  `skipped_files` with its reason, where a search used to pass over it
  silently. Part of contract 1.4.

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

- `GET /v1/samples` caps `context`, `before_context` and
  `after_context` at 100 lines per side, the cap the trace endpoint
  has; a larger value is a `422`, and the OpenAPI document declares the
  bound. `rx samples` keeps no cap. Part of contract 1.4.

### Changed

- A file the user names and the process may not read fails every command
  alike: `permission denied: <path>` and exit code 4 from `rx trace`,
  `rx samples` and `rx index` (which used to exit 1 and 0, `rx index`
  skipping it as "not a text file"), and `403 Permission denied: <path>`
  from `GET /v1/trace`, `GET /v1/samples` and `POST /v1/index` (which
  used to answer 200 with the file skipped, 500, and 400). A file or a
  subdirectory a directory walk meets and may not read is skipped with
  the reason `permission denied` by `rx trace` and `rx index -r` alike,
  and the rest of the tree is searched or indexed: `rx index -r` used to
  fail the whole directory over one unreadable subdirectory. The closing
  line of a run whose failures all exited 4 now reads "one or more
  files were outside the search roots or could not be read".

- Every command and every HTTP route decides what a file is by one rule,
  from the file's own bytes read through its pin: the magic bytes name
  the format and the extension is never consulted (a text file named
  `.gz` is searched, sampled and indexed as text; a gzip file named
  `.log` is decompressed); a zstd file is seekable when its seek table
  describes it, whatever its name (`.zstd` included); a zstd stream that
  starts with a skippable frame (pzstd's output) is zstd; and a file is
  not text when the first 8 KiB of its text, decompressed for a
  compressed file, hold a NUL byte. Each refusal gives the reason, which
  starts with "not a text file". Consequences: `rx samples` and
  `GET /v1/samples` refuse a file that is not text (exit 2, `400`)
  instead of answering a `.tar.gz` or a UTF-16 file as lines of raw
  bytes, and build no index for it; a compressed copy with a NUL byte in
  its first 8 KiB of text is skipped like the plain file, where it used
  to be searched; `rx compress` and `POST /v1/compress` refuse any input
  that is not text, with the reason, where they used to refuse only
  names such as `.tar.gz` (`compound archives (tar.gz, etc.) are not
  supported`); `rx index` and `POST /v1/index` give the reason in
  `skip_reasons` and the `400` detail (`not a text file: …: <path>`);
  `GET /v1/tree` reports `is_text` by the same rule (it read 512 bytes
  of the raw file, and called every compressed file text) and
  `compression_format` from the bytes. A line that starts with the
  bytes `FF FE` is not taken for UTF-16 unless a NUL byte follows.

- Every integer environment variable follows one rule and has a range:
  unset keeps the default; a value that is not a whole number, or is
  below the minimum, keeps the default; a value above the maximum is
  used as the maximum; each value not used as it is logs one
  `invalid_setting` warning per process. The ranges: `RX_WORKERS` and
  `RX_MAX_SUBPROCESSES` 1–256, `RX_MIN_CHUNK_SIZE_MB` and
  `RX_LARGE_FILE_MB` 1–1048576 (1 TiB), `RX_MAX_LINE_TEXT_BYTES`
  1–268435456 (256 MiB), `RX_MAX_SUBMATCHES_PER_LINE` 1–1000000,
  `RX_ANALYZE_WINDOW_LINES` 1–2048, `RX_TASK_TTL_MINUTES` 1–10080 (one
  week), `RX_SAMPLES_WAIT_SECONDS` 0–3600. `RX_LARGE_FILE_MB=0` or a
  negative value used to be accepted: the index checkpoint step became
  0 or negative, every line got a checkpoint (a 15 MB index for a
  120 MB log, `samples` about 60 times slower) and every plain file's
  trace answer was cached; it now means the default, 50. `RX_WORKERS`
  had no upper bound; a larger value now runs 256 ripgrep processes.
  `RX_TASK_TTL_MINUTES=0` or below used to drop finished tasks at the
  next sweep and now keeps the default, 60. See "Integer settings" in
  the configuration docs.

- `rx compress` and `POST /v1/compress` write the seek-table footer in
  the layout of the zstd seekable format specification: frame count,
  descriptor (0, no checksums), magic `0x8F92EAB1`, so the last four
  bytes of the file are `b1 ea 92 8f`. Tools that follow the
  specification can now seek in rx's files. rx still reads files in
  the old layout (magic, frame count, flags) as seekable, with their
  stored indexes; rx-python reads only the old layout, so it reads new
  files as plain zstd. A seekable file from another tool that follows
  the specification is now refused by `rx compress` as already
  seekable unless `--force` (`"force": true` over HTTP) is given, as
  rx's own files are.

- Trace-cache entries are written as compact JSON instead of indented
  JSON. With the submatch spans each record now stores, an entry is
  still about 12% smaller than before them: on a 465 MB log, `WARN`
  (51,817 matches) takes 4.3 MB instead of 4.9 MB and `INFO`
  (1,327,224 matches) 110 MB instead of 125 MB. Readers accept either
  layout.

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
  request that sends `Prefer: respond-async` (RFC 7240) waits up to
  `RX_SAMPLES_WAIT_SECONDS` (default 5): when the build ends in time it
  answers `200` as before, otherwise `202` with the task (`task_id`,
  `status`, `message`, `path`, `started_at`) and
  `Preference-Applied: respond-async`, and the client polls the task and
  asks again. A request without the header waits for the build and
  answers `200`, so a client that cannot follow a task (viewer 0.4.0, a
  script) works as before. A client that disconnects stops waiting, not
  the build. `rx samples` still waits for the build however long it
  takes. Part of contract 1.4.

- The docs state the one way a line index, the trace cache or
  `--no-index` may change a trace answer: a line number that is `-1`
  without them may be the true line number with them. Every other field
  is equal, `-1` means "not computed", and a number rx fills in is the
  line holding the match's offset. The fields that describe how an
  answer was produced are outside the rule: `request_id`, `time`,
  `cli_command` and `file_chunks`, since a trace-cache hit reports the
  chunk count of the scan that wrote the entry, whatever
  `RX_MIN_CHUNK_SIZE_MB` and `RX_MAX_SUBPROCESSES` are now. Behavior is
  unchanged; see
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

- With several patterns, the `file_scanned` webhook of a trace fires
  for a file once its matched lines' patterns are decided, which is done
  for up to 64 files (or 8 MiB of matched text) at a time, so the event
  can trail the scan by up to 63 files; with one pattern it fires as
  each file's scan ends, as before. Events keep the order the files were
  read in, and their payload is unchanged.

### Fixed

- A seekable zstd file of no text, which `rx compress` writes for an
  empty log, is indexed as a file of no lines (`line_count` 0, no
  frames, no checkpoints). `rx index` used to fail on it with "empty
  seek table" and exit 1, so `rx index -r` failed a directory of
  rotated logs that held one, and `rx compress` reported an
  `index_error` for its own output.

- A pattern PCRE2 cannot compile (`rx trace -P -e '('`, or `pcre2=true`
  over HTTP) is now a usage error: exit 2, or a `400`, with PCRE2's
  reason, as for a pattern the default engine cannot compile. It used to
  be answered as 0 matches with every file in `skipped_files`, exit 0
  and `200`. `-P` against a ripgrep built without PCRE2 is the same
  usage error, and its message says PCRE2 is missing. rx now runs `rg`
  once on empty input with the search's patterns and flags before it
  reads any file, so a pattern error never looks like a file error, on
  any kind of file; it also fails when no file is found to search,
  which used to answer 0 matches. Every later `rg` run (chunks,
  compressed streams, seekable frames, pattern crediting) tells a
  pattern error from a file error by every wording ripgrep 13 and 14
  use.

- `rx trace` no longer writes a trace-cache entry for piped input. The
  input is copied to a temporary file that is deleted when the command
  ends, so every trace of 50 MB or more of piped input (the large-file
  size) left an entry nothing could ever read or remove. A file named
  beside `-` is still cached.

- A relative `RX_CACHE_DIR` or `XDG_CACHE_HOME` is now made absolute
  against the directory `rx` starts in. It used to stay relative, so
  the cache was looked up under whatever directory a command ran from,
  two CLI calls from two directories used two caches, and `/health`
  reported `CACHE_DIR` as the relative path.

- A line index records its source file by the absolute path
  (`source_path`), whatever path the caller gave. It used to record the
  path as typed, so `rx index a.log` stored `a.log`: `rx index --info`
  and `rx index --json` showed a path that depended on the directory,
  and the pruning loop in the caching docs, run from another directory,
  deleted valid indexes. Trace-cache entries already recorded the
  absolute path.

- A line index or a trace-cache entry built under one time zone is now
  reused under another. Both compared the file's mtime and ctime as
  local wall-clock text, so an `rx serve` started with `TZ=UTC` and a
  CLI in the user's zone rebuilt each other's indexes and rescanned
  each other's traces on every switch, and an mtime moved by an hour
  inside the hour a daylight-saving change repeats went unseen. Both
  now record the two times as nanoseconds since the Unix epoch
  (`source_mtime_ns`, `source_ctime_ns`) and compare those; the text
  fields stay, for reading. They also record the device beside the
  inode (`source_device`), and a stored index is used for a file only
  when both match, so two files with one inode number on different
  filesystems are told apart. The index format version is now 7: an
  index written before is rebuilt once. Trace-cache entries stay at
  version 6.

- Every checkpoint of a line index now names a line the file has, at
  the byte where it starts (for a seekable `.zst`, a byte inside the
  line). An index used to end with a checkpoint one line past the end
  of the text whenever its last line crossed a checkpoint step (the
  line count plus one, at the text's size), an empty file's index held
  the checkpoint `[1, 0]` for a line it does not have, a seekable frame
  of exactly 10,000, 20,000 … lines that ends with a line break got an
  interior checkpoint at its end, and a frame of no text got one too.
  An empty file's index now has an empty `line_index`. No answer
  changes: a lookup starts from the checkpoint before its line, and
  `GET /v1/index` and `rx index --json` no longer list a checkpoint a
  client could seek to and find nothing. Indexes stored by earlier
  versions keep working; their extra checkpoint is never chosen for a
  line the file has.

- When the line index could not be stored (a read-only cache directory,
  `RX_CACHE_DIR` naming a regular file, or a log whose base name made
  the cache file name longer than 255 bytes), every `rx samples` call on
  a compressed or large file built the whole index and threw it away,
  silently: about 1.5 s per lookup on a 465 MB log instead of 0.01 s,
  and over HTTP one index task per request. `rx samples` and
  `GET /v1/samples` now check that the index can be stored before they
  build it; when it cannot, they build nothing, read the file without
  an index (the same answer) and log one `index_not_stored` warning per
  process naming the cause. `rx index` and `POST /v1/index` fail before
  reading the file, with an error that starts `cannot store the line
  index:` and names the cause, instead of failing at the save after a
  full build. A stored index that cannot be read is now rebuilt by the
  next lookup even for a plain file below the large-file size, so it no
  longer warns on every lookup.
- A log whose base name is longer than about 233 bytes got no line
  index ("file name too long") and no trace cache entry. Both cache file
  names now cut the base name so the whole name fits in 255 bytes; the
  hash of the full path keeps two such names apart. A name that already
  fitted keeps its file name, so no stored entry moves.
- With `RX_CACHE_DIR` naming a regular file, each `rx trace` of a large
  file logged `trace_cache_unreadable` beside `trace_cache_write_failed`,
  and each lookup of a line index logged `index_unreadable`, as if a
  stored entry were damaged. A cache location that cannot hold an entry
  is now an ordinary miss; only the failed write is reported, once.
- `rx samples --no-index`, `RX_NO_INDEX=1` and `GET /v1/samples` under
  `RX_NO_INDEX` skipped only the index build and still read a stored
  index, so the flag meant for a suspect index answered from it. They
  now read no index at all, as the docs say. A line index file that is
  cut short or cannot be read is now treated as absent everywhere, with
  one `index_unreadable` warning naming the file: `rx samples --lines=`
  on a plain file no longer exits 1 and `GET /v1/samples` no longer
  answers `500` with "unmarshal …: unexpected end of JSON input" or
  "permission denied"; the lookup reads the file without the index, and
  rebuilds it when the file is worth one.
- `rx samples --offsets=` and `GET /v1/samples?offsets=` with a line
  index returned fewer lines before the offset than the context asked
  for whenever the index checkpoints were closer together than the
  context, which on a log of long lines they are: the pass started only
  one checkpoint back. It now starts at a checkpoint at least `context`
  lines before the offset, so the window is the one `--lines` and
  `--no-index` give, on plain, gzip, bzip2, xz and zstd files alike
  (seekable zstd was already right). A trace-cache hit with
  `--before`/`--context` on such a file had the same short leading
  context and now rebuilds all of it, as the scan reported it.
- `rx compress` and `POST /v1/compress` write the output to a hidden
  temporary file (`.rx-compress-*.tmp`) in the output's directory, sync
  it, and only then put it under the output name. Two compressions to
  one output used to write into the same file at once and both report
  success over a mix of their bytes, which no tool could read; now each
  writes its own file. Without `--force` (`"force": true`) the name
  must still be free when the output is put there, so of two such runs
  exactly one succeeds and the other fails with "output file already
  exists"; with it, each replaces the name whole and the last one
  stays. `compressed_size` is the size of the file the run wrote, not
  of whatever holds the name afterwards. A failed or
  cancelled compression leaves nothing under the output name, and a
  forced one leaves the file it would have replaced as it was (the HTTP
  task used to delete it first). A symbolic link at the output name is
  never written through: it counts as an existing output, even when it
  leads nowhere, and `--force` replaces the link itself. The output's
  directory is reached from the search root without passing a link, so
  a directory swapped for one after the path check gets nothing
  written. A `POST /v1/compress` task now holds its output path as well
  as its input until it ends: a second compression into the same
  output, and a `POST /v1/index` of that output, get `409` naming the
  output and the running task, where both used to be accepted.
  `rx compress app.log app.log.gz` printed `wrote app.log.zst` twice
  and kept only the second file; two inputs of one command that would
  be compressed to one output (the same path given twice included) are
  now refused before anything is written, with or without `--force`,
  and the command exits 2. `Ctrl-C` or SIGTERM during `rx compress`
  now stops the encoding and removes the temporary file (exit 5); it
  used to go on to write the whole output and only then exit 5.

- A seekable zstd file written by another tool to the zstd seekable
  format specification (facebook/zstd, `contrib/seekable_format`) is
  now seekable for rx. Its seek-table footer is frame count,
  descriptor, magic, and rx read only its own order (magic, frame
  count, flags), so such a file was read as one plain zstd stream:
  `file_chunks` 1, no frame index, every lookup decompressing from the
  start. rx now reads both footer layouts and prefers the
  specification's; a footer whose descriptor sets the checksum flag
  (bit 7, not bit 0 as rx tested) has 12-byte entries, read past
  without checking the checksums, and one that sets a reserved bit is
  not trusted. A table in either layout is still trusted only when it
  describes the file, and rx's own files keep working with their
  stored indexes.

- A `.zst` file is read as seekable only when its seek table describes
  it: the table's skippable frame runs to the end of the file, the
  frames it lists end where it starts, and each starts with a zstd
  frame header whose recorded content size, if any, is the table's.
  Two seekable files joined with `cat` (or written by two compressions
  at once) end with the second file's table alone, and rx used to read
  that table as the whole file's: `rx trace` answered 0 matches where
  the text holds 66, with exit 0, and cached it, and `rx index` failed.
  A damaged table placed frames wrongly the same way. Such a file is
  now read as the plain zstd stream it is, by `rx trace` (one stream,
  `file_chunks` 1, with a `seek_table_mismatch` warning), `rx samples`,
  `rx index` and `rx compress` alike, and every answer is the text's.
  The check reads the table and one frame header per frame. `rx samples`
  on a seekable file without an index now reads it frame by frame
  through the table, so a damaged frame fails the lookup with
  "seekable zstd frame is damaged: frame N" instead of a zstd decoder
  message, as `rx index` reports it; a line before the damage is still
  answered.

- `rx trace` and `GET /v1/trace` no longer answer "no matches" for a
  seekable zstd file with a damaged frame, and no longer cache that
  answer. A frame that failed to decompress ended its batch's input to
  ripgrep; when the frames before it held no match, ripgrep's exit 1
  hid the error, the up to 99 frames after it in the batch were never
  searched, and the scan was stored in the trace cache as complete, so
  every later trace served the same wrong answer. When ripgrep had
  found a match first, the error surfaced instead and every match of
  the file was dropped, those of intact frames in other batches
  included. Now the scan goes on around the damage, since frames
  decompress independently: every line that lies wholly in intact
  frames is searched, the lines that touch a damaged frame are not
  (never a fragment of one, so `$` cannot match where the line was
  cut), the file is listed in `skipped_files` with its matches kept, a
  `seekable_damaged_frames` warning names the frames, the matches after
  the first damaged frame have `absolute_line_number` -1, and nothing
  is written to the trace cache. A frame that decompresses to another
  length than its seek-table entry is damaged too. A frame whose bytes
  cannot be read at all (an I/O error) fails the file whatever ripgrep
  exited with.

- `rx trace` and `GET /v1/trace` keep a line longer than 256 KiB whole
  when it lies across a chunk boundary of a large plain file. The
  chunker looked only 256 KiB past each boundary for a newline and
  otherwise cut the line there, so each chunk's ripgrep saw a fragment
  as a whole line: a match came back at an offset inside its line with
  part of its text, the same line could be reported twice, a match
  spanning the cut was lost, `^`, `$` and `-w` matched at the cut, a
  `--before` or `--after` window could lose or repeat a match, the trace
  cache stored the wrong offset, and the answer changed with the number
  of cores and `RX_MIN_CHUNK_SIZE_MB`. The boundary search now reads on
  to the end of the line, so every chunk starts on a line and a chunked
  scan answers as a scan in one piece. A line that covers several
  boundaries merges their chunks; planning still reads each byte at
  most once, so a file that is one long line is not read once per
  boundary.

- A trace answered from the trace cache gives every match the
  submatches the scan gave it. A hit used to run each pattern again
  with Go's regular expressions, which differ from ripgrep's: with `-P`
  (look-around, backreferences) every match came back with
  `submatches: null`, and Unicode `\b`, `\w` and `-w` around non-ASCII
  words, `-w` on a word ending in punctuation (`-w 'Agent:'`), `\r$`
  on a CRLF line and a pattern across bytes that are not UTF-8 came
  back with no submatches or different ones, so the viewer showed no
  highlight. Each trace-cache record now stores the start and end of
  its pattern's submatches as ripgrep reported them, and a hit takes
  them from there, cutting them under lower `RX_MAX_LINE_TEXT_BYTES` or
  `RX_MAX_SUBMATCHES_PER_LINE` bounds as a scan would. A scan that
  leaves any pattern's submatches out (over `RX_MAX_SUBMATCHES_PER_LINE`
  on one line) is no longer cached, and a cached record whose spans its
  line cannot hold fails the file (listed in `skipped_files`) instead of
  dropping the match. The trace cache format stays at version 6; an
  entry without the spans, written by a build between 0.3.0 and this
  one, is treated as a miss and rewritten.

- A trace answered from the trace cache labels each match with the
  pattern it matched, whatever order the patterns come in. Searches
  that list the same patterns in another order (`-e WARN -e NEEDLE`
  and `-e NEEDLE -e WARN`, or `regexp=` in another order over HTTP)
  share one cache entry, and a hit used to read each stored match's
  pattern position against the reader's order: every match of a
  two-pattern search came back under the other pattern's ID, with the
  submatches of that other pattern (often none). A hit now translates
  each stored position through the patterns the entry lists to the
  reader's ID, and an entry that does not list the reader's patterns is
  treated as a miss. The cache format is unchanged.

- `rx trace` and `GET /v1/trace` answer a line whose ripgrep JSON event
  is larger than 16 MiB: a matched line that long, or a shorter one on
  which the pattern matches very often (a 1 MB line of `x` searched
  for `x` makes a 50 MB event). Such an event used to stop the parser.
  On a plain file and on a gzip, bzip2, xz or plain zstd stream, rx then
  waited for ever on an `rg` that still had output to write — the CLI
  never returned and an HTTP request held its `rg` until the client gave
  up; on a seekable zstd file the whole file was listed in
  `skipped_files` with 0 matches. Events now have no size limit, and
  whenever rx stops reading ripgrep's output early it kills `rg` before
  waiting for it, so a failure ends with an error or a skipped file,
  never a hang. A seekable-zstd scan that meets a line of ripgrep output
  it cannot parse now fails like the other paths, rather than dropping
  that line's match.

- `rx trace` and `GET /v1/trace` read a NUL byte after the first 8 KiB
  of a file as part of its line. ripgrep, left to its own binary
  detection, counted each such NUL as a line break: in the chunk holding
  it, every later match was numbered one line too high per NUL, and a
  match on the NUL line came back at an offset inside the line with only
  the text after the NUL. Plain files, chunked files, gzip, bzip2, xz,
  plain and seekable zstd copies of one log answered differently, and a
  trace-cache hit kept the wrong offset while it fixed the number. Every
  ripgrep search now runs with `--text`, so each line keeps its number,
  offset and whole text. A NUL in the first 8 KiB still makes a plain
  file binary and skipped. The trace cache format is now version 6: an
  entry written by an earlier scan may hold a split line's offset, and
  is discarded and rescanned.

- `rx trace` and `GET /v1/trace` report the offsets a file has when it
  starts with a UTF-8 byte-order mark. ripgrep, left to its default
  encoding detection, dropped the mark and counted offsets without its
  three bytes: every match and context line of the input that started
  with it (the file, or its first chunk, for a plain file; the whole
  stream of a gzip, bzip2, xz or zstd copy; the first batch of up to 100
  frames of a seekable copy) came back 3 bytes short, so
  `samples --offsets=` answered the line before, and a trace-cache hit
  read that line's text. Every ripgrep search now runs with
  `--encoding=none`. The mark is the first three bytes of line 1's
  `line_text`, as `rx samples` shows it, and submatch positions count
  it; a pattern anchored with `^` therefore does not match a line that
  starts with the mark, as with `rg --encoding=none` and grep. A plain
  UTF-16 file is still binary and skipped; a compressed UTF-16 copy,
  which rx searches, is now searched as its bytes rather than
  transcoded, so a pattern written as text no longer matches it.

- `rx trace` and `GET /v1/trace` give a line that is not valid UTF-8
  its own text. ripgrep sends such a line as base64, and rx returned
  that base64 as `line_text` (newline included), in `matches` and in
  `context_lines`, while `rx samples` and a trace-cache hit of the same
  request showed the line's text, so a cold and a warm answer differed.
  Pattern identification also ran on the base64, so a line could be
  credited to the wrong pattern. Each byte that is not part of a valid
  character now reads as U+FFFD in JSON, on a scan, a cache hit and in
  `samples` alike, and human output prints the line's bytes as
  `rx samples` does. Submatch `start` and `end` stay byte positions in
  the line. A long line cut by `RX_MAX_LINE_TEXT_BYTES` is cut at the
  start of a character as Go reads one, which no longer splits a valid
  character that follows stray continuation bytes.

- `rx trace` and `GET /v1/trace` with several patterns credit each line
  to exactly the patterns that match it. ripgrep runs the patterns as
  one alternation and reports only the alternation's spans, and rx used
  to credit a pattern when Go's regexp found the same text: with
  `-e NEEDLE -e NEED`, a line holding `NEEDLE` was credited to `NEEDLE`
  alone, so `NEED` lost every such line, and swapping the `-e` order
  swapped the loser (also `ERR.R` beside `ERROR`, `abc` beside `bcd`).
  When Go could not reproduce ripgrep's span, every pattern got the line,
  including ones it does not contain: Unicode `\w`, `\b` and `\d` (ASCII
  in Go), `-w` with punctuation at a word's edge, `\r$` on a CRLF line,
  bytes that are not valid UTF-8. Now ripgrep decides: each pattern runs
  alone, with the search's flags, over the lines the scan matched, and a
  line is credited to the patterns whose run reports it, `-P` patterns
  included. The lines and submatches an answer credits to a pattern are
  the ones a trace of that pattern alone gives, in any `-e` order, and
  each match's `submatches` are now that pattern's own spans rather than
  the alternation's, as a trace-cache hit already gave them. A line
  longer than `RX_MAX_LINE_TEXT_BYTES` is decided on the whole line,
  read again from the file, rather than credited to every pattern. A
  search with one pattern runs no extra ripgrep and answers as before;
  with several, the matched lines of every file of the search are
  checked together, one more ripgrep run per pattern for each 8 MiB of
  matched text (a 465 MB log searched with `-e WARN -e W`: 2.55 s to
  2.78 s; 1000 small files: 5.7 s to 5.8 s).

### Security

- A named pipe, a socket or a device is never opened. A request or a
  command that named a named pipe waited for a writer for ever (the
  readability check of `GET /v1/trace`, `GET /v1/samples`,
  `POST /v1/index`, `rx compress`, and the text check of a directory
  walk all opened it); now a named one is refused at once with
  `not a regular file` (exit code 2, HTTP `400 Not a regular file:
  <path>`), and a walk skips one with that reason. Files are opened with
  `O_NONBLOCK`, so a file swapped for a named pipe after its check is
  refused as changed instead of waited on.

- A skip reason never reveals what the sandbox keeps out of reach: a
  link the walk refuses or cannot resolve is reported with fixed wording
  (`symlink leads into a hidden entry; …` no longer names the entry,
  `cannot resolve symlink: no such file or directory` no longer carries
  the target's path), and a trace's `skip_reasons` use a fixed set of
  wordings (`permission denied`, `cannot be read`, …) instead of an
  operating-system error's text; the error goes to the log. The same
  wordings appear in `rx index` `skip_reasons`.
- The human output of `rx trace`, `rx samples`, `rx index` and
  `rx compress`, and every `Error:` line, writes out the control
  characters of a path or a reason (`\x1b[31m`) instead of sending them
  to the terminal, so a file named with escape sequences cannot recolor,
  clear or retitle the terminal of whoever lists its directory. The text
  of matched and sampled lines is printed as it is, as ripgrep prints
  it.

- A directory search no longer follows a symbolic link out of
  `--search-root` or into a hidden entry. `rx trace` on a directory
  (recursive or `--no-recursive`, CLI and `GET /v1/trace`) and
  `rx index` on a directory checked only the directory itself, so a
  link inside it that led to `/etc/passwd`, or to `.private/` without
  `--hidden`, was searched and its lines returned, while naming the same
  link was refused with 403 or exit code 4. Every link a walk meets is
  now resolved and checked as a named path is: one that leads inside a
  root and is not hidden is followed (a file under the link's own path,
  a directory descended into); any other is skipped and listed in
  `skipped_files` (trace) or `skipped` and `skip_reasons` (index, with
  the reason), and no index is stored for it. A link back to a
  directory the walk is inside, a link to nothing and a cycle of links
  are skipped the same way, so no loop can hang a walk. A link to a
  directory is never searched or listed as a file; with
  `--no-recursive` it is passed over like any directory. `GET /v1/tree`
  applies the same rule: it leaves out the links a caller could not
  open, lists a link to a directory as a `directory`, and no longer
  reports the size and type of a file outside the roots. Without a
  sandbox (the CLI without `--search-root`) links are followed wherever
  they lead. See `docs/concepts/security.md`.
- A directory search enters each directory once. Links that lead
  sideways to other directories were followed every time they were met,
  so six levels of six links to the next level made one `rx trace`,
  `rx index --recursive` or `GET /v1/trace` search 46,656 paths to one
  file and run for minutes. A second way into a directory already
  searched is now skipped, with the reason `directory already searched
  through '<path>'` in `skip_reasons`, and links to directories are
  followed only after every real directory, so a directory reached both
  directly and through a link is searched under its own path. A link to
  a directory that is also reached directly is therefore no longer
  searched a second time under the link's path.
- A file is read only while it is the file that was checked. A link
  checked while it led inside the root, and retargeted before the read
  (or a checked file replaced by a link), was read at its new target:
  outside the roots or hidden. The check now records the file's device
  and inode, and every read compares the file it opened with them:
  every read of a trace (chunks, compressed streams, seekable frames,
  cache hits, line numbering), `samples` (CLI and `GET /v1/samples`),
  an index build and the input of `rx compress` / `POST /v1/compress`.
  A file that changed is skipped (`skipped_files`) or refused (`file
  changed after it was checked`), never read. A directory walk lists
  each directory through a handle checked the same way.
- The check of a path records the identity of the checked file only.
  When a directory on the path was swapped, while the check ran, for a
  link into a hidden directory of the root (or to another directory),
  the check recorded the file the link led to, and later reads accepted
  it: a hidden file could be read without `--hidden`. The check now
  walks down from the search root one directory at a time and fails
  with `file changed after it was checked` on any link it meets there.
- An index build reads only the file it checked. A build of a seekable
  zstd file read the file again by its path after the check, so a link
  retargeted during `rx index` or `POST /v1/index` (with or without
  `--analyze`) could index a file outside the roots or a hidden one and
  report its line count, line lengths and anomalies. Every index build
  now takes the format, the fingerprint, the text, and a seekable
  file's seek table and frames from the one handle it opened and
  checked.
- A line index is used only for the file it was built from. It is
  looked up by path, so after a link was retargeted and put back it
  could describe another file than the one read and give wrong line
  numbers in `samples` and in a trace's line numbering. It is now used
  only when the inode it recorded is the read file's, and is treated as
  absent otherwise.
- One line can no longer make a trace hold gigabytes in memory.
  ripgrep reports a matched line whole, plus about 50 bytes for every
  submatch on it, and rx read each report whole: a search for `x` over a
  file with one 100 MB line of `x` cost about 5 GB per worker, so one
  `GET /v1/trace` could exhaust a server. rx now reads ripgrep's output
  without holding it and keeps, per matched or context line, at most
  `RX_MAX_LINE_TEXT_BYTES` of its text (default 1 MiB, cut at the start
  of a UTF-8 character) and at most `RX_MAX_SUBMATCHES_PER_LINE`
  submatches (default 10,000), only those that start inside the text
  kept. New response fields say what was left out:
  `line_text_truncated` on a match and on a `context_lines` entry, and
  `submatches_truncated` on a match (always `true` on a cut line). The
  offset, both line numbers and every other match are exact, on plain,
  compressed and seekable files, from the CLI and over HTTP alike; a
  line within the bounds is answered as before. Which patterns a cut
  line is credited to is decided on the whole line, read again from the
  file. A trace whose answer cuts a line is not written to the trace
  cache. The seekable-zstd path no longer buffers ripgrep's whole output
  for a batch of frames, and a trace-cache hit and the line numbering of
  a capped trace read a long line without holding it. The human output
  prints a cut line with ` [line truncated]` after it. Output from
  ripgrep that is not a JSON event is still an error that stops the
  scan. Part of contract 1.4.

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
