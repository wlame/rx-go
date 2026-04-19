# `GET /v1/detectors`

List registered anomaly detectors and the severity-scale legend used
by `--analyze` mode.

## Purpose

`rx` ships with a pluggable file-analyzer registry. Third-party
analyzers can be registered at build time to contribute anomaly
detection to `rx index --analyze`. This endpoint reports which
analyzers are currently compiled in and exposes the 4-level severity
legend that UIs render alongside reported anomalies.

## Request

```text
GET /v1/detectors
```

No parameters.

## Response — 200 OK

```json
{
  "detectors": [],
  "categories": [],
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
| `detectors` | array | Metadata for every registered detector (empty in v1) |
| `categories` | array | Category metadata (empty in v1) |
| `severity_scale` | array | Always 4 entries; never changes |

### `severity_scale[]` fields

| Field | Type | Description |
|---|---|---|
| `min` | number | Lower bound (inclusive) |
| `max` | number | Upper bound (exclusive except `critical`) |
| `label` | string | `"low"`, `"medium"`, `"high"`, `"critical"` |
| `description` | string | Human-readable description |

### `detectors[]` fields (when non-empty)

Reserved for future analyzer registration. Would include:

| Field | Type |
|---|---|
| `name` | string |
| `version` | string |
| `description` | string |
| `categories` | string array |
| `severity_range` | object |

## Status codes

| Code | When |
|---:|---|
| `200 OK` | Always |

## Current state

The v1 release ships with an **empty analyzer registry**. Both
`detectors` and `categories` are empty arrays. The `severity_scale` is
still returned so UI code doesn't need to branch on the field.

When called via `/v1/index?analyze=true` against a file, the resulting
`anomalies` array will also be empty — the analysis pipeline runs, but
no detectors are registered to contribute findings.

See [concepts/analyzers](../../concepts/analyzers.md) for how to add a
detector (requires modifying `rx` source and recompiling).

## Examples

### Check registered detector list

```bash
curl -s 'http://127.0.0.1:7777/v1/detectors' | jq '.detectors | length'
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

- [concepts/analyzers](../../concepts/analyzers.md) — analyzer architecture
- [`POST /v1/index`](line-index.md) — triggers analyzers with `analyze=true`
