# `GET /v1/detectors`

List registered anomaly detectors and the severity-scale legend used
by `--analyze` mode.

## Purpose

`rx` ships with a pluggable file-analyzer registry. This endpoint
reports which detectors are currently compiled in (the v1 catalog is
nine detectors, enumerated below) and exposes the 4-level severity
legend that UIs render alongside reported anomalies.

## Request

```text
GET /v1/detectors
```

No parameters.

## Response — 200 OK

```json
{
  "detectors": [
    { "name": "traceback-python",    "version": "0.1.0", "category": "log-traceback", "description": "Python tracebacks (Traceback (most recent call last): ...)" },
    { "name": "traceback-java",      "version": "0.1.0", "category": "log-traceback", "description": "Java stack traces (Exception in thread ... / Caused by: / Suppressed:)" },
    { "name": "traceback-go",        "version": "0.1.0", "category": "log-traceback", "description": "Go runtime tracebacks (panic: / fatal error: / goroutine N [state]:)" },
    { "name": "traceback-js",        "version": "0.1.0", "category": "log-traceback", "description": "JavaScript / Node.js stack traces (Error: ... / at ...)" },
    { "name": "coredump-unix",       "version": "0.1.0", "category": "log-crash",     "description": "Unix crash dumps (segfault / ASAN / kernel oops / stack smashing)" },
    { "name": "json-blob-multiline", "version": "0.1.0", "category": "format",        "description": "Multi-line JSON objects or arrays spanning several lines" },
    { "name": "long-line",           "version": "0.1.0", "category": "format",        "description": "Lines unusually long relative to the file's length distribution" },
    { "name": "repeat-identical",    "version": "0.1.0", "category": "repetition",    "description": "Consecutive identical lines" },
    { "name": "secrets-scan",        "version": "0.1.0", "category": "secrets",       "description": "Credential-shaped strings (AWS key, GitHub PAT, Slack token, JWT, PEM key)" }
  ],
  "categories": [
    { "name": "log-traceback", "description": "Stack traces from language runtimes" },
    { "name": "log-crash",     "description": "Native crash dumps (segfault / ASAN / kernel oops)" },
    { "name": "format",        "description": "Structural oddities (long lines, multi-line JSON)" },
    { "name": "repetition",    "description": "Consecutive repeats" },
    { "name": "secrets",       "description": "Credential-shaped strings" }
  ],
  "severity_scale": [
    { "min": 0.0, "max": 0.4, "label": "low",      "description": "Minor deviations, informational" },
    { "min": 0.4, "max": 0.6, "label": "medium",   "description": "Warnings, format issues" },
    { "min": 0.6, "max": 0.8, "label": "high",     "description": "Errors, crashes" },
    { "min": 0.8, "max": 1.0, "label": "critical", "description": "Fatal errors, exposed secrets" }
  ]
}
```

## Response fields

| Field | Type | Description |
|---|---|---|
| `detectors` | array | Metadata for every registered detector |
| `categories` | array | Category metadata (grouping taxonomy for `detectors[].category`) |
| `severity_scale` | array | Always 4 entries; never changes |

### `detectors[]` fields

| Field | Type | Description |
|---|---|---|
| `name` | string | Stable identifier; also the cache-path segment |
| `version` | string | Bumped when output format changes |
| `category` | string | One of the entries in `categories[].name` |
| `description` | string | Human-readable summary |

### `categories[]` fields

| Field | Type | Description |
|---|---|---|
| `name` | string | Category tag used in `detectors[].category` and in anomaly `category` fields |
| `description` | string | Human-readable summary |

### `severity_scale[]` fields

| Field | Type | Description |
|---|---|---|
| `min` | number | Lower bound (inclusive) |
| `max` | number | Upper bound (exclusive except `critical`) |
| `label` | string | `"low"`, `"medium"`, `"high"`, `"critical"` |
| `description` | string | Human-readable description |

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Always |

## Shipped detectors

Nine detectors are compiled into `rx` as of v1:

| Detector | Category | Severity |
|---|---|---:|
| `traceback-python` | `log-traceback` | 0.7 |
| `traceback-java` | `log-traceback` | 0.7 |
| `traceback-go` | `log-traceback` | 0.8 |
| `traceback-js` | `log-traceback` | 0.6 |
| `coredump-unix` | `log-crash` | 0.9 |
| `json-blob-multiline` | `format` | 0.3 |
| `long-line` | `format` | 0.3 |
| `repeat-identical` | `repetition` | 0.4 |
| `secrets-scan` | `secrets` | 1.0 |

Severity is a navigation hint (which band to render), not a verdict.
See [concepts/analyzers](../../concepts/analyzers.md) for each
detector's trigger rules and for how to add a new one (requires
modifying `rx` source and recompiling).

## Examples

### Check registered detector list

```bash
curl -s 'http://127.0.0.1:7777/v1/detectors' | jq '.detectors[] | .name'
```

### Filter detectors by category

```bash
curl -s 'http://127.0.0.1:7777/v1/detectors' \
    | jq '.detectors[] | select(.category == "log-traceback") | .name'
```

### Show the severity legend

```bash
curl -s 'http://127.0.0.1:7777/v1/detectors' \
    | jq '.severity_scale[] | "\(.label): \(.min)–\(.max)"'
```

Output:

```text
"low: 0–0.4"
"medium: 0.4–0.6"
"high: 0.6–0.8"
"critical: 0.8–1"
```

## See also

- [concepts/analyzers](../../concepts/analyzers.md) — analyzer architecture and shipped catalog
- [`POST /v1/index`](line-index.md) — triggers analyzers with `analyze=true`
