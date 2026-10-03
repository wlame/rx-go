# `GET /v1/signatures`

The signature catalogue the server loaded when it started: the built-in
signatures and the user's signature files, with every field of each
definition, the groups, the levels and every load error. The same
document `rx signatures --json` prints. Since contract 2.0; `features`
on [`GET /health`](health.md) lists `signatures`.

## Purpose

- Name, describe and color each signature a findings answer refers to
- Offer the signatures and groups a scan can select
  ([`POST /v1/findings`](findings.md#post-v1findings))
- Show why a user's signature did not load

## Request

```text
GET /v1/signatures
```

No parameters. The catalogue is loaded once, when `rx serve` starts
([where rx finds signature files](../../concepts/signatures.md#where-rx-finds-signature-files)); a change to a signature file
takes a restart. Each load error is also logged at start, as a
`signature_load_error` warning with its file, line and id.

## Response — 200 OK

The built-ins beside two invented user files: `team.yml`, the example
file of [Signatures](../../concepts/signatures.md#a-signature-file),
and `bad.yml`, whose one signature has a regex that does not compile.
Shortened to one signature and four of the groups:

```json
{
  "signatures": [
    {
      "id": "invoice-failed",
      "name": "Invoice failed",
      "group": "billing",
      "description": "An invoice the billing service could not create.",
      "tags": ["invoices"],
      "level": "high",
      "links": [],
      "metadata": {},
      "default": true,
      "redact": false,
      "kind": "regex",
      "keywords": [["invoice "], [" failed"]],
      "keywords_declared": false,
      "line_pass": false,
      "source": {"file": "/home/you/.config/rx/signatures/team.yml", "line": 8},
      "overrides": false,
      "disabled_by": null,
      "examples": {
        "match": ["2026-01-02 10:00:00 ERROR invoice 812 failed: card expired"],
        "no_match": ["2026-01-02 10:00:01 INFO invoice 813 sent"]
      },
      "regex": {"regex": "\\binvoice \\d+ failed\\b", "not": null, "capture": 0, "entropy": null, "validate": null},
      "block": null,
      "run": null,
      "measure": null
    }
  ],
  "groups": [
    {"name": "traceback", "description": "Stack traces and uncaught exceptions of programs in many languages.", "order": 1, "count": 8, "builtin": true},
    {"name": "severity", "description": "Lines written at an error or warning level.", "order": 7, "count": 2, "builtin": true},
    {"name": "structure", "description": "The shape of the text, such as runs of blank or repeated lines, long lines, JSON blocks and binary bytes.", "order": 10, "count": 8, "builtin": true},
    {"name": "billing", "description": "Events of the billing service.", "order": 12, "count": 3, "builtin": false}
  ],
  "levels": ["info", "low", "medium", "high", "critical"],
  "errors": [
    {
      "file": "/home/you/.config/rx/signatures/bad.yml",
      "line": 6,
      "id": "broken",
      "message": "regex: error parsing regexp: missing closing ): `(unclosed`"
    }
  ],
  "search_path": ["/home/you/.config/rx/signatures"],
  "files": ["/home/you/.config/rx/signatures/bad.yml", "/home/you/.config/rx/signatures/team.yml"]
}
```

`structure` counts 8 of its 9 built-ins because `team.yml` disables
`blank-run`, which is still listed, with `disabled_by`.

## Response fields

| Field | Type | Description |
|---|---|---|
| `signatures` | array | Every loaded signature, group by group in group order, then in load order; a built-in a user file switched off is listed too, with `disabled_by` set, and is never selected or run |
| `groups` | array | The groups in their order: `name`, `description`, `order` (1-based; it sets a group's color), `count` (active signatures), `builtin` |
| `levels` | string[] | `info`, `low`, `medium`, `high`, `critical`, lowest first |
| `errors` | array | Every load problem, by file then line: `file`, `line`, `id` (empty for a problem of the whole file), `message`. A refused signature is not in `signatures` |
| `search_path` | string[] | Where user files were looked for (`RX_SIGNATURES_PATH`, or the configuration directory) |
| `files` | string[] | The user files found there, in load order, loaded or refused |

### `signatures[]` fields

| Field | Type | Description |
|---|---|---|
| `id`, `name`, `group`, `description` | string | The definition's fields |
| `tags`, `links` | string[] | Never `null` |
| `level` | string | `info`, `low`, `medium`, `high` or `critical` |
| `metadata` | object | String to string; `{}` when none |
| `default` | bool | In the default set; `false` for an opt-in signature |
| `redact` | bool | What it matches is masked in the text of every finding and in the cache when the server masks secrets (`rx serve --mask-secrets` or `RX_MASK_SECRETS=1`; off by default) ([Masking](../../concepts/signatures.md#masking)) |
| `kind` | string | `regex`, `block`, `run` or `measure`; the member of that name holds the definition, the other three are `null` |
| `keywords` | string[][] | The keyword clauses of the anchor, lowered; `[]` when the signature runs on every line |
| `keywords_declared` | bool | The clauses come from the signature's own `keywords` |
| `line_pass` | bool | No keyword gate: tested on every line |
| `source` | object | `{"builtin": true}`, or `{"file", "line"}` of a user file |
| `overrides` | bool | A user signature that replaced the built-in of the same id |
| `disabled_by` | object \| null | `{"file", "line"}` of the `disable` entry that switched a built-in off |
| `examples` | object | `match` and `no_match` texts |
| `regex`, `block`, `run`, `measure` | object \| null | The definition of the signature's kind, defaults applied, as [`rx signatures show`](../../cli/signatures.md) prints it |

## Status codes

| Code | When |
|---:|---|
| 200 | Always: load errors are in `errors`, not a failed request |

## See also

- [`GET /v1/findings`, `POST /v1/findings`](findings.md) — a file's findings
- [`rx signatures`](../../cli/signatures.md) — the same catalogue from a terminal
- [Signatures](../../concepts/signatures.md) — the model, loading and selection
