# Analyzers

`rx` includes a pluggable file-analyzer registry. Analyzers contribute
**anomaly detection** to `rx index --analyze` and `POST /v1/index`
with `analyze=true`. The v1 catalog ships nine detectors that mark
regions of interest (tracebacks, crashes, JSON blobs, long lines,
repeated runs, secret-shaped strings) so rx-viewer can surface them
as jump targets or highlights.

## What an analyzer is

An analyzer is a Go implementation of the `FileAnalyzer` interface
(in `internal/analyzer/registry.go`). It inspects a file and returns
a list of anomalies — regions of the file that stand out statistically
or semantically. Each detector's output is a navigation hint, not a
verdict: `severity` maps to a display bucket, not a production-grade
alert level.

Line-level detectors additionally implement `LineDetector`
(`internal/analyzer/linedetector.go`): they receive each line through
a shared **coordinator** that runs inside the index builder's per-chunk
loop. One file pass produces both the line-offset index and the anomaly
list.

## Shipped catalog

The nine detectors below register at process startup and run whenever
`--analyze` is set. Each is a separate package under
`internal/analyzer/detectors/`; bumping one detector's version does
not invalidate the others' caches.

| Detector | Category | Severity | What it finds | Package |
|---|---|---:|---|---|
| `traceback-python` | `log-traceback` | 0.7 | Python tracebacks (`Traceback (most recent call last):`) | [`tracebackpython`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/tracebackpython) |
| `traceback-java` | `log-traceback` | 0.7 | Java stacks (`Exception in thread …`, `Caused by:`, `Suppressed:`) | [`tracebackjava`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/tracebackjava) |
| `traceback-go` | `log-traceback` | 0.8 | Go runtime tracebacks (`panic:`, `fatal error:`, `goroutine N [state]:`) | [`tracebackgo`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/tracebackgo) |
| `traceback-js` | `log-traceback` | 0.6 | JavaScript / Node.js stacks (`Error:` + `at …`) | [`tracebackjs`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/tracebackjs) |
| `coredump-unix` | `log-crash` | 0.9 | Segfaults, ASAN reports, kernel oops, stack-smashing | [`coredumpunix`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/coredumpunix) |
| `json-blob-multiline` | `format` | 0.3 | Multi-line JSON objects / arrays | [`jsonblob`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/jsonblob) |
| `long-line` | `format` | 0.3 | Lines unusually long versus the file's length distribution | [`longline`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/longline) |
| `repeat-identical` | `repetition` | 0.4 | Consecutive identical lines (runs of 5+) | [`repeatidentical`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/repeatidentical) |
| `secrets-scan` | `secrets` | 1.0 | Credential-shaped strings (AWS key, GitHub PAT, Slack token, JWT, PEM key) | [`secretsscan`](https://github.com/wlame/rx-go/tree/main/internal/analyzer/detectors/secretsscan) |

Severity values are fixed per detector — they map to one of the four
bands exposed via `GET /v1/detectors`. Every detector on this list is
`version = "0.1.0"`; bumps are reflected in the cache path and in
`GET /v1/detectors`.

## The wire contract

Every detector produces `AnomalyRangeResult` entries:

```json
{
  "start_line":   12345,
  "end_line":     12348,
  "start_offset": 1024000,
  "end_offset":   1024512,
  "severity":     0.7,
  "category":     "log-traceback",
  "description":  "Python traceback (4 frames)",
  "detector":     "traceback-python"
}
```

Fields:

| Field | Type | Meaning |
|---|---|---|
| `start_line`, `end_line` | int64 | Inclusive line range (1-based) |
| `start_offset`, `end_offset` | int64 | Byte range of the anomaly |
| `severity` | float64 | 0.0 (minor) to 1.0 (critical) |
| `category` | string | Free-form taxonomy tag (see catalog above) |
| `description` | string | Human-readable explanation |
| `detector` | string | The analyzer that produced this entry |

## The severity scale

The 4-level scale is fixed:

| Severity | Label | Meaning |
|---|---|---|
| `0.0 - 0.4` | low | Minor deviations, informational |
| `0.4 - 0.6` | medium | Warnings, format issues |
| `0.6 - 0.8` | high | Errors, crashes |
| `0.8 - 1.0` | critical | Fatal errors, exposed secrets |

Exposed via `GET /v1/detectors` as `severity_scale`. UIs can render
findings alongside a legend without knowing which detectors are
registered.

## How analyzers engage

When a user runs:

```bash
rx index /var/log/app.log --analyze
```

Or via HTTP:

```bash
curl -sXPOST http://127.0.0.1:7777/v1/index -H 'Content-Type: application/json' \
    -d '{"path":"/var/log/app.log", "analyze":true}'
```

`rx`:

1. Builds the base line-offset index (same as non-analyze mode)
2. Spins up a per-worker `Coordinator` wrapping the full detector
   registry and a shared **sliding window** of the most recent lines
   (default 128, capped at 2048 — see
   [`--analyze-window-lines`](../cli/line-index.md))
3. Dispatches each line to every registered `LineDetector` through the
   coordinator
4. After the per-chunk pass, calls each detector's `Finalize` with a
   `FlushContext` holding the file's total line count, median, and P99
   length (so stats-driven detectors like `long-line` can pick a
   threshold)
5. Deduplicates anomalies across chunk workers by
   `(detector, start_offset, end_offset)` and attaches the result to
   `UnifiedFileIndex.Anomalies`

When `--analyze` is off, no coordinator runs and the hot loop is the
same as the current release.

## Sliding window and look-back

Detectors that need context (`traceback-python` walks back to see the
opening cue, `json-blob-multiline` counts brackets across lines) read
from a fixed-size ring buffer owned by the coordinator. The buffer is
passed by pointer, so detectors never allocate per line. The window
size is configurable per request:

- CLI: `rx index --analyze --analyze-window-lines=256`
- HTTP: `{"analyze": true, "analyze_window_lines": 256}`
- Env: `RX_ANALYZE_WINDOW_LINES=256`

Precedence is URL param > CLI flag > env > default (128). The value is
clamped to `[1, 2048]`.

## Cache namespacing

Each analyzer's output is cached under:

```text
~/.cache/rx/analyzers/<analyzer-name>/v<version>/<source-hash>.json
```

This scheme means:

- Two analyzer versions coexist without overwriting each other
- Upgrading one analyzer doesn't invalidate the others
- Removing an analyzer from the registry leaves its old cache files
  dormant (safe to delete at any time)

## Registering a new analyzer

**Analyzers are added at build time, not runtime.** End users cannot
drop a plugin into a directory and have `rx` pick it up. Adding an
analyzer is a small code change to `rx` itself.

Steps (for developers):

1. Implement `LineDetector` (or `FileAnalyzer` for whole-file
   detectors) in a new package under
   `internal/analyzer/detectors/<your-detector>/`
2. Call `analyzer.Register(...)` from the package's `init()` function
3. Add a blank import (`_ "github.com/wlame/rx-go/internal/analyzer/detectors/<your-detector>"`)
   to `cmd/rx/main.go` so the init fires at process startup
4. Rebuild `rx`
5. The detector's metadata now appears in `GET /v1/detectors` and runs
   whenever `analyze=true`

The `LineDetector` interface:

```go
type LineDetector interface {
    FileAnalyzer
    OnLine(w *Window)
    Finalize(flush *FlushContext) []Anomaly
}
```

`FileAnalyzer` (the metadata-only parent):

```go
type FileAnalyzer interface {
    Name() string         // stable identifier, used in cache paths
    Version() string      // bump to invalidate cached output
    Category() string     // grouping tag; shown in GET /v1/detectors
    Description() string  // human-readable
}
```

Cache key scheme, the registry's Freeze barrier, and the coordinator's
concurrency contract are documented in `internal/analyzer/` source.

## Implications for users

- **`analyze=true` now produces findings** — the nine detectors above
  run on every analyze pass
- **Detector names are stable** — anything shipped under a given
  `name` + `version` keeps its wire contract; breaking changes land
  under a new version string
- **The `severity_scale` is stable** — you can build UI around the 4
  levels today

## Related concepts

- [Caching](caching.md) — analyzer cache layout
- [Line indexes](line-indexes.md) — `--analyze` extends the index
  with statistics
- [API: detectors endpoint](../api/endpoints/detectors.md)
