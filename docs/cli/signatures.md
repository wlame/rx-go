# `rx signatures`

List, show and check the [signatures](../concepts/signatures.md) rx
knows: the built-in ones and those of your signature files, and print a
template file to start from.

## Synopsis

```text
rx signatures [--group=GROUP] [--tag=TAG] [--json]
rx signatures show ID [--json]
rx signatures check [FILE...]
rx signatures template
```

## Description

rx reads the built-in signatures from its own binary and your
signature files from the first of `RX_SIGNATURES_PATH`,
`$XDG_CONFIG_HOME/rx/signatures/` and `~/.config/rx/signatures/`
([where rx finds signature files](../concepts/signatures.md#where-rx-finds-signature-files)).
Each subcommand loads them when it runs; nothing is kept between runs,
so an edited file counts at the next command.

A problem in a file never stops the command: the file or the signature
it refuses is left out, and the problem is reported with its file, line
and signature id.

## `rx signatures`

Lists the signatures group by group, in group order. Each group's line
gives the number of its signatures that run, and `opt-in` when none of
them is in the default set. Each row gives the id, the level, the kind,
where the signature is defined (`built-in` or `file:line`) and its
marks:

| Mark | Meaning |
|---|---|
| `overrides` | a user signature that replaces the built-in of the same id |
| `masked` | `redact: true`: with masking on (`--mask-secrets` on `rx trace` and `rx serve`, or `RX_MASK_SECRETS=1`), what it matches is masked in the text of every finding and in the findings cache ([Masking](../concepts/signatures.md#masking)); masking is off by default |
| `opt-in` | `default: false`: it runs only when a selection names it |
| `disabled by file:line` | a built-in a user file switched off with `disable` |
| `replaced by a refused definition (file:line)` | a built-in whose id a user definition took and was refused: neither runs |

A group's line counts its active signatures and, after them, how many
of the rows under it are disabled or replaced built-ins (`structure  8
signatures, 1 disabled`), so the number reads with the rows below it.
Then come the errors, the search path with the number of files found
on it, and a `Secrets:` line: whether `RX_MASK_SECRETS` turns masking
on for `rx trace` and `rx serve` run without `--mask-secrets`.

```bash
rx signatures
```

```text
traceback  8 signatures
  python-traceback         critical  block    /home/me/.config/rx/signatures/team.yml:27  overrides
  java-exception           high      block    built-in
  …
secret  16 signatures
  aws-access-key           high      regex    built-in                                    masked
  …
structure  8 signatures, 1 disabled
  long-line                low       measure  built-in
  …
  blank-run                info      run      built-in                                    disabled by /home/me/.config/rx/signatures/team.yml:1
billing  2 signatures
  invoice-failed           high      regex    /home/me/.config/rx/signatures/team.yml:8
  slow-invoice             medium    regex    /home/me/.config/rx/signatures/team.yml:19  opt-in

Errors (1)
  /home/me/.config/rx/signatures/draft.yml:6: refund-spike: regex: error parsing regexp: missing closing ): `(refund`

Search path: /home/me/.config/rx/signatures (2 files)
Secrets: shown; --mask-secrets or RX_MASK_SECRETS=1 masks what the signatures marked masked find
```

A group without a signature to list is left out. The example is
shortened at each `…`: the built-in signatures are 70, listed with their
definitions in [Built-in signatures](../reference/signatures.md).

### Flags

| Flag | Default | Description |
|---|---|---|
| `--group` | — | List only this group's signatures; an unknown group exits 2 with close names |
| `--tag` | — | List only the signatures with this tag; an unknown tag exits 2 with close names |
| `--json` | `false` | One JSON document: `signatures`, `groups`, `levels`, `errors`, `search_path` and `files` |

In `--json`, each signature carries every field of its definition with
the defaults applied, `kind`, `keywords` (the keyword clauses, a list of
lists), `keywords_declared`, `line_pass`, `source` (`{"builtin": true}`
or `{"file": …, "line": …}`), `overrides`, `disabled_by` (`null` or
`{"file": …, "line": …}`), and one of `regex`, `block`, `run` and
`measure` (the others `null`). A block always lists its rules under
`states`, the simple form compiled. Each group carries `name`,
`description`, `order`, `count` (its signatures that run) and
`builtin`; each error `file`, `line`, `id` and `message`.

```bash
rx signatures --group=billing --json
```

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
      "source": {"file": "/home/me/.config/rx/signatures/team.yml", "line": 8},
      "overrides": false,
      "disabled_by": null,
      "examples": {"match": ["2026-01-02 10:00:00 ERROR invoice 812 failed: card expired"], "no_match": ["2026-01-02 10:00:01 INFO invoice 813 sent"]},
      "regex": {"regex": "\\binvoice \\d+ failed\\b", "not": null, "capture": 0, "entropy": null, "validate": null},
      "block": null,
      "run": null,
      "measure": null
    }
  ],
  "groups": [{"name": "billing", "description": "Events of the billing service.", "order": 12, "count": 2, "builtin": false}],
  "levels": ["info", "low", "medium", "high", "critical"],
  "errors": [{"file": "/home/me/.config/rx/signatures/draft.yml", "line": 6, "id": "refund-spike", "message": "regex: error parsing regexp: missing closing ): `(refund`"}],
  "search_path": ["/home/me/.config/rx/signatures"],
  "files": ["/home/me/.config/rx/signatures/draft.yml", "/home/me/.config/rx/signatures/team.yml"]
}
```

(The example is shortened: the real output lists `slow-invoice` too and
puts each list item on its own line.)

## `rx signatures show`

Prints one signature: comment lines with its source, its marks, its
keyword clauses (derived from the anchor or declared), whether it runs
in the [line pass](../concepts/signatures.md#keywords-and-the-gate), the
key its findings are cached under, and whether its examples ran; then
its definition as YAML with every
default filled in. The output is a valid definition: placed under
`signatures:` in a file of your own, it loads into the same signature.

```bash
rx signatures show invoice-failed
```

```text
# source: /home/me/.config/rx/signatures/team.yml:8
# keywords (derived): [invoice ] AND [ failed]
# line pass: no
# cache key: 3b8e5f0c9a1d47e2b6c0f9a8d7e6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e9d8
# examples: 1 match, 1 no_match, all pass
id: invoice-failed
name: Invoice failed
group: billing
description: An invoice the billing service could not create.
tags: [invoices]
level: high
links: []
metadata: {}
default: true
redact: false
regex: '\binvoice \d+ failed\b'
capture: 0
examples:
  match:
    - '2026-01-02 10:00:00 ERROR invoice 812 failed: card expired'
  no_match:
    - 2026-01-02 10:00:01 INFO invoice 813 sent
```

The members of one clause are separated by commas; a line must hold a
member of every clause (`AND`) before the anchor is tried on it. A
disabled built-in is shown with `# disabled by file:line`.

The cache key is the one a scan with the current settings stores the
signature's findings under in the
[findings cache](../concepts/caching.md#findings). It changes when the
signature's matching part changes, when `RX_SIGNATURES_LINE_BYTES`,
`RX_FINDINGS_MAX_PER_SIGNATURE`, `RX_TIMESTAMP_LOOKBACK_KB` or
`RX_LOG_TZ` changes, when `RX_MASK_SECRETS` turns masking on or off,
and, with masking on, when any redacting signature changes; its name,
description, group and the other descriptive fields do not change it.

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | The signature as one JSON object, as `rx signatures --json` lists it |

An id no signature has exits 2 and lists close names; an id whose every
definition was refused exits 2 and repeats the errors.

## `rx signatures check`

Loads signature files and reports every problem: one line per error
and per warning, in the form `file:line: id: error: message`, then a
summary. With `FILE` arguments (files or directories) it loads only
those, plus the built-ins (for overrides and groups); without, it loads
the search path. A `FILE` that does not exist is an error.

```bash
rx signatures check ~/.config/rx/signatures/
```

```text
/home/me/.config/rx/signatures/draft.yml:6: refund-spike: error: regex: error parsing regexp: missing closing ): `(refund`
/home/me/.config/rx/signatures/draft.yml:21: refund-late: error: examples: match example 1 gives no finding
examples: checked
2 files, 3 signatures: 2 errors, 0 warnings
Error: The signature files have 2 errors
```

The one warning is `no keywords: runs in the line pass`, for a
signature with an anchor from which no keyword clause could be derived:
it works, and is tested on every line. Every signature's examples run
through the scan engine: a `match` text that gives no finding, or a
`no_match` text that gives one, is an error of that signature
(`examples: match example 1 gives no finding`), which is then refused.
The output says `examples: checked`.

Use it in CI before you ship a signature file:

```bash
rx signatures check team-signatures.yml
```

## `rx signatures template`

Prints a commented signature file with one signature of each kind and
every field explained. It loads without errors:

```bash
rx signatures template > ~/.config/rx/signatures/mine.yml
rx signatures check ~/.config/rx/signatures/mine.yml
```

## Exit codes

| Code | When |
|---:|---|
| 0 | Success; for `check`, no error (warnings allowed) |
| 1 | `check` found an error |
| 2 | Usage error: an unknown flag or subcommand, `show` without an id, an unknown id, group or tag |

The [global flags](index.md#global-flags) are accepted;
`--search-root` does not apply to signature files, which come from the
environment.

## See also

- [Signatures](../concepts/signatures.md) — the model and the file format
- [Configuration](../configuration.md#signatures) — `RX_SIGNATURES_PATH`
