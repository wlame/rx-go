# History

`rx` was originally written in Python — see
[`wlame/rx-python`](https://github.com/wlame/rx-python) for the original
implementation.

It was later reimplemented in Go to reduce runtime overhead (no
interpreter, no process-pool startup cost, no per-decompressor
subprocesses) and to ship as a single static binary with no runtime
dependencies beyond `ripgrep`.

## Contract 2.1

The rx-go release that reads systemd journal files speaks HTTP wire
contract 2.1 and writes line index format 12. Contract 2.1 only adds, so
a client written for 2.0 keeps working. What it adds:

- `journal` in the index answers (`GET /v1/index`, the result of a
  `POST /v1/index` task, `rx index --json`): the counts and the state of
  a systemd journal, `null` for every other file. See
  [Journal files](concepts/journal-files.md#damage-and-its-report).
- `not_text_reason` in the entries of `GET /v1/tree` and
  `GET /v1/search-roots`: why a file that is not text is refused.
- `entries_overlap`, a new value of the `code` of a chain's reasons.
- `journal`, a new value of `compression_format`.

**Every stored line index is built once more.** An index of format 11 is
treated as absent, and the next command that needs one builds a format
12 index in its place.

**The viewer window widens.** `rx serve` installs viewer releases from
0.8.0 up to, but not including, 0.10.0. Viewer 0.8.x keeps working with
contract 2.1; viewer 0.9.x is the first to show journal files and the
warning for a damaged one.

## Contract 2.0

rx-go 0.8.0 speaks HTTP wire contract 2.0 and writes line index format
11. Signatures replace the index analysis: rx finds tracebacks, crashes,
secrets and other places worth a look with named signatures, built in
or read from your own YAML files, and keeps what it finds in a cache of
its own. The analyzer, its nine detectors and every surface that served
them are gone; nothing projects findings into the old shape.

| Removed in contract 2.0 | What replaces it |
|---|---|
| The analyzer's nine detectors | The built-in signatures, which find every case the detectors found: `traceback-python` → `python-traceback`; `traceback-java` → `java-exception`; `traceback-js` → `js-error`; `traceback-go` → `go-panic`; `coredump-unix` → `sanitizer-report`, `glibc-abort`, `killed-by-signal`, `kernel-oops`; `secrets-scan` → `aws-access-key`, `github-token`, `slack-token`, `jwt`, `private-key`; `long-line` → `long-line`; `repeat-identical` → `repeated-lines`; `json-blob-multiline` → `json-block`. `rx signatures` lists them all |
| `rx index --analyze PATH` (`-a`) | `rx trace --signatures PATH` |
| `rx index --analyze-window-lines=N` | Nothing: each block or run signature bounds itself (`max_lines`, `min`) |
| `RX_ANALYZE_WINDOW_LINES` | Nothing; `RX_SIGNATURES_LINE_BYTES` and `RX_FINDINGS_MAX_PER_SIGNATURE` bound a scan |
| `GET /v1/detectors` | `GET /v1/signatures` |
| `POST /v1/index {"analyze": true}` | `POST /v1/findings {"path": …}`, a background scan |
| `anomalies`, `anomaly_count`, `anomaly_summary` of an index and of `GET /v1/index` | `GET /v1/findings?path=…`: `findings`, `total` and `counts` |
| `analysis_performed`, `analysis_window_lines`, `analysis_detector_set` | `state`, `checked` and `missing` of `GET /v1/findings` |
| `rx_analyze_requests_total`, `rx_analyze_duration_seconds` | `rx_signature_scans_total`, `rx_signature_scan_duration_seconds`, `rx_findings_cache_hits_total` |
| Viewers `0.2.0 <= v < 0.9.0` | Viewers `0.8.0 <= v < 0.9.0`, the ones that speak contract 2 |

The removed flags stay as hidden ones that exit 2 with their
replacement, so an old script fails with a message rather than with
"unknown flag":

```text
$ rx index --analyze app.log
Error: --analyze was replaced by: rx trace --signatures PATH
```

A `POST /v1/index` body with `analyze` or `analyze_window_lines`
answers `422` naming the member, and `GET /v1/detectors` answers `404`,
as any route the server does not have.

**Every line index is built once more.** An index of format 10 is
treated as absent; the next command that needs one builds a format 11
index in its place, smaller by the analysis it no longer holds. The
index of a seekable zstd file holds no line-length statistics any more:
only `--analyze` computed them for such a file. `GET /v1/index` answers
`line_length`, `longest_line`, `line_ending` and `empty_line_count`
with `null`; `rx index --info --json`, which has no `line_length` or
`longest_line` member, gives `null` for its `line_length_*` members,
`line_ending` and `empty_line_count`.

**Findings have a cache of their own**, `findings/` beside `indexes/`
in the cache directory: one directory per file, one entry per
signature ([caching](concepts/caching.md#findings)). Deleting an index
keeps a file's findings, and a change to a signature rescans only that
signature (with masking on, a change to a redacting one rescans them
all, since every excerpt is masked by it; turning masking on or off
rescans them all too).

**Secrets are shown unless masking is on.** The text of a finding is
the file's text by default; `--mask-secrets` on `rx trace` and
`rx serve`, or `RX_MASK_SECRETS=1`, masks what the signatures with
`redact: true` find ([Masking](concepts/signatures.md#masking)).

**Release rx-go and rx-viewer back to back.** rx-go 0.8.0 refuses the
viewer 0.7.1 that is installed today, and viewer 0.7.1 knows only
contract major 1. Between the release of rx-go 0.8.0 and that of
rx-viewer 0.8.0, `rx serve` has no viewer to serve: it starts, answers
the API and prints `Viewer: none (/ redirects to /docs)`. Release rx-go
first, then the viewer.
