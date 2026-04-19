# Analyzers

`rx` includes a pluggable file-analyzer registry. Analyzers contribute
**anomaly detection** to `rx index --analyze` and `POST /v1/index`
with `analyze=true`. At v1 the registry is empty — the infrastructure
is in place but no detectors are registered.

## What an analyzer is

An analyzer is a Go implementation of the `FileAnalyzer` interface
(in `internal/analyzer/registry.go`). It gets handed a file path and
returns a list of anomalies — regions of the file that stand out
statistically or semantically.

Examples of what analyzers could do (none implemented in v1):

- **Log-pattern miner**: cluster lines by structure, detect outlier
  lines that don't match any common pattern
- **JSON schema inference**: detect JSON lines with unusual structure
- **CSV column stats**: detect columns with missing values or type
  violations
- **Secret scanner**: detect embedded API keys, certificates, private
  keys

## The wire contract

Every file analyzer produces `AnomalyRangeResult` entries:

```json
{
  "start_line":   12345,
  "end_line":     12345,
  "start_offset": 1024000,
  "end_offset":   1024050,
  "severity":     0.7,
  "category":     "error",
  "description":  "Unusual stack-trace pattern",
  "detector":     "log-pattern-miner"
}
```

Fields:

| Field | Type | Meaning |
|---|---|---|
| `start_line`, `end_line` | int64 | Inclusive line range (1-based) |
| `start_offset`, `end_offset` | int64 | Byte range of the anomaly |
| `severity` | float64 | 0.0 (minor) to 1.0 (critical) |
| `category` | string | Free-form taxonomy tag |
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
findings alongside a legend without knowing which analyzers are
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
2. Iterates the analyzer registry
3. For each analyzer that claims `Supports(path, mime) == true`, calls
   `Analyze(path)` and collects its anomaly results
4. Merges results from every analyzer into the `anomalies` slice of
   the resulting index

With an empty registry (the v1 state), step 3 yields zero anomalies
and the final `anomalies` slice is empty. The contract-level response
is unchanged — clients that consumed an empty `anomalies` array
before will continue to work.

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

1. Implement `FileAnalyzer` in a new package under
   `internal/analyzer/<your-analyzer>/`
2. Register it in `analyzer.NewRegistry()` so every `rx` instance
   knows about it
3. Rebuild `rx`
4. The analyzer's output now appears in `GET /v1/detectors` and runs
   when `analyze=true`

The `FileAnalyzer` interface (simplified):

```go
type FileAnalyzer interface {
    Name() string                        // stable identifier, used in cache paths
    Version() string                     // bump to invalidate cached output
    Supports(path, mime string) bool     // runs for this file?
    Analyze(path string) ([]AnomalyRangeResult, error)
}
```

Cache key scheme and the registry's concurrency contract are
documented in `internal/analyzer/` source.

## Future work

The v1 release ships without any analyzers. Reasonable first targets:

- **CSV column stats** (low effort, high value for CSV users)
- **JSON schema inference** (medium effort)
- **Log-pattern miner** (high effort, highest value for log users)

Until then, `GET /v1/detectors` returns an empty detectors list and
`analyze=true` is essentially the same as `analyze=false` plus a
promise that the infrastructure is ready for future additions.

## Implications for users

- **Don't rely on `analyze=true` producing findings at v1** — it
  currently produces none
- **Don't build tooling on specific detector names** — none are
  guaranteed to exist. Inspect the registry via `GET /v1/detectors`
  before assuming
- **The `severity_scale` is stable** — you can build UI around the 4
  levels today

## Related concepts

- [Caching](caching.md) — analyzer cache layout
- [Line indexes](line-indexes.md) — `--analyze` extends the index
  with statistics
- [API: detectors endpoint](../api/endpoints/detectors.md)
