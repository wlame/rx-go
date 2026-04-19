# Migrating from rx-python to rx-go

This document captures intentional behavioural differences between the
Python v2.2.1 implementation and the Go port (v2.2.1-go). Most
differences are invisible (same JSON shape, same exit codes, same
command-line flags). The ones below are worth knowing if you have
scripts or dashboards that consumed rx-python output.

## Philosophy

Stage 9 parity testing (Round 1 and Round 2) exercised every
user-visible surface — CLI output, JSON schemas, HTTP bodies, Prometheus
metrics, cache file contents. Differences are grouped into three
categories:

1. **Go fixes Python bugs.** rx-go preserves the correct behaviour; we
   don't replicate the bug. These are listed below under "Python bugs
   Go doesn't replicate".
2. **Go adds extras.** Python's schema is the minimum contract; rx-go
   may add extra JSON keys (e.g. `index_path` in the index JSON entries).
   The addition must not break Python's existing keys.
3. **Design divergences.** Intentional design decisions documented in
   the original spec (user decisions 5.1–5.15 and 6.9.1–6.9.6).

## Python bugs Go doesn't replicate

### P1 — `rx index --threshold=N` silently ignored (Python)

In Python v2.2.1, `rx index file --threshold=1` does NOT actually lower
the threshold. `FileIndexer.__init__` never receives the flag; the
Python CLI option is dead. Only the environment variable
`RX_LARGE_FILE_MB` changes behaviour.

rx-go plumbs `--threshold` correctly — passing it on the command line
overrides the environment default.

### P2 — `rx trace <dir> --recursive` returns 0 matches (Python)

In Python v2.2.1, the long form `--recursive` triggers a click flag /
glob interaction bug that causes recursive scans to report 0 matches
even when matches exist. The short form `-r` works. Inconsistency was
verified during Round 1 parity testing on the `dir1` fixture.

rx-go's `--recursive` (long form) and `-r` (short form) are wired to
the same boolean, so both work identically.

### P3 — `absolute_line_number: -1` on multi-chunk scans (Python)

Python's parallel trace path sets `absolute_line_number: -1` for every
match when scanning files that trigger chunking (large log files).
The single-chunk path correctly computes line numbers.

rx-go's indexer always computes `absolute_line_number` from the cached
line index, regardless of the chunking path. Multi-chunk runs in Go
emit real line numbers.

## Go adds extras (JSON)

### `rx index --json`

Python: `{indexed: [...], skipped: [...], errors: [...], total_time: N}`.
Go adds these keys to each `indexed[]` entry (Python does not emit them):

- `index_path` — absolute path to the `.json` cache file the entry was
  written to. Useful for scripts that want to inspect or delete the cache
  without recomputing the hash-based filename.

### `rx compress --json`

Matches Python's `{files: [{input, action, success, ...}]}` shape exactly
at v1 of the Go port. If future features require new telemetry, they
will be added under keys prefixed with `go_` or nested inside a
`go_extras` object to avoid collisions with potential Python additions.

## JSON schema null conventions

Stage 9 Round 2 user rule: documented schema fields must emit an
explicit null when unset — omitempty is only acceptable for fields that
are "extensions" NOT part of the advertised schema.

Affected fields that changed from Round 1 to Round 2:

- `TraceResponse.file_chunks, context_lines, before_context,
  after_context, cli_command` — now always present (null when unset).
- `SamplesResponse.compression_format, cli_command` — always present.
- `UnifiedFileIndex.frames, anomalies, anomaly_summary` — always present.
- `TreeResponse.total_size, total_size_human` — always present.
- `CompressRequest.output_path, force` — always present.

## CLI flag differences

### `rx samples`

Python uses `-b / -l / -c / -B / -A / -r / --no-color` and `--byte-offset
/ --line-offset` long forms. rx-go v1 originally shipped with only
long-form flags `--offsets / --lines / --context / --before / --after /
--regex / --color` to avoid naming collisions on `-r` (`--recursive`
elsewhere).

Starting in Stage 9 Round 2 rx-go restored the Python-compatible short
aliases on `samples`: `-b` / `-l` / `-c` / `-B` / `-A` / `--no-color` /
`-r` (for regex, matching Python's usage on the samples subcommand).

### `rx trace` directory behaviour

Python's `rx "error" dir/` scans `dir/` recursively by default. rx-go
matches this default starting Stage 9 Round 2. Use `--no-recursive` to
opt out (Go-only flag; Python has no equivalent because it had no way
to disable recursion short of listing files explicitly).

## Exit codes

- Missing file on `rx trace`: Python exits 1 with "Path not found" on
  stderr. rx-go matches starting Stage 9 Round 2.
- Below-threshold on `rx index`: Python exits 0 with the file in
  `skipped[]`. rx-go matches starting Stage 9 Round 2 (previously
  exited 1 with a hard error).

## Cross-cache compatibility

### Whole-second mtime

Both implementations write identical cache files when the source file's
modification time has no fractional seconds. This is verified by the
round-trip tests in `internal/index/store_test.go`.

### Fractional-second mtime

Python's `datetime.fromtimestamp(stat.st_mtime).isoformat()` rounds the
timestamp to microseconds; Go's `time.Format("2006-01-02T15:04:05.000000")`
truncates. For any mtime whose 7th decimal digit is >= 5 the two
implementations produce strings differing by exactly 1 microsecond, and
a byte-for-byte mtime comparison in `is_valid_for_source` fails.

Per user decision in Round 2 triage: this is IGNORED. rx-go's truncation
behaviour is arguably more correct (it preserves the exact on-disk
timestamp) and the 1-microsecond drift has no practical impact on cache
reuse because index rebuilds are cheap. Users who need byte-identical
caches across implementations should rely on the on-disk `source_size_bytes`
+ `source_modified_at` pair being internally consistent per-writer, not
across-writer.

## Prometheus metrics

rx-go's `/metrics` endpoint emits a subset of Python's metric families
at v1, excluding:

- `rx_complexity_*` — regex complexity analysis feature is NOT ported
  (user decision, out-of-scope for v1).
- `rx_analyze_*` — analyzer plugin registry ships empty at v1; the
  analyze endpoint returns success with 0 findings, so analyze-specific
  metrics are deferred.

Stage 9 Round 2 ports the remaining gap (rx_errors_total,
rx_hook_call_duration_seconds, rx_file_size_bytes, rx_context_lines_*,
rx_offsets_per_samples_request, rx_max_results_limited_total,
rx_patterns_per_request, rx_matches_per_request,
rx_trace_cache_skip_total, rx_trace_cache_load_duration_seconds,
rx_trace_cache_reconstruction_seconds, rx_index_load_duration_seconds)
under a MetricsSink interface that is a no-op when `serve` is not active
— CLI-mode invocations pay no overhead for metrics collection.

## Stage 9 Round 4 fixes (post-performance-benchmark)

Two parity bugs surfaced only under large real-world fixtures
(1.3 GB Twitter dump, 260 MB Telegram export) during the Round 3
performance benchmark. Both are fixed; these notes preserve the history.

### R3-B1 — `rx compress --output-dir=DIR` now supported

Before fix: `rx compress file.log --output-dir=/tmp/out/` exited with an
"unknown flag" error. Only `-o / --output=PATH` (single-file target)
worked.

After fix: matches Python's behaviour — directory is auto-created
(`os.MkdirAll 0755`, equivalent to Python's `os.makedirs(exist_ok=True)`);
each input file is written as `{output-dir}/{basename}.zst`. Passing
both `--output` and `--output-dir` is a usage error (Python parity).
This is the flag that enables multi-file compress into a single target
directory — `rx compress a.log b.log c.log --output-dir=/out/`.

### R3-B2 — `rx index` honors cache when `--force` is not set

Before fix: `rx index file.log` always rewrote the cache JSON, even when
a valid cached index already existed. The `--force` flag was declared
but never consulted by `runIndexBuild`. Visible as the only scenario in
Round 3 where Go was slower than Python (warm-cache 687 ms Go vs 438 ms
Python on the 260 MB Telegram fixture).

After fix: `runIndexBuild` now calls `index.LoadForSource(path)` before
invoking the builder. If the cached index is valid (size + mtime match),
it is reused and no rebuild happens; the same JSON `indexed[]` entry is
emitted but without the CPU cost. `--force` still triggers a rebuild.
`--analyze` forces a rebuild when the cached index was built without
analysis (matching Python's `needs_rebuild` contract).
