# `rx scan`

Scan files for [signatures](../concepts/signatures.md) and keep their
findings in the findings cache, so that `rx trace --signatures`,
`GET /v1/findings` and the viewer answer from it without reading the
files again.

## Synopsis

```text
rx scan PATH [PATH ...] [--signatures=SEL] [--force] [--json]
rx scan --recursive DIR [--signatures=SEL] [--force] [--json]
```

## Description

`rx scan` is the command `POST /v1/findings` is the equivalent of. It
scans each file for the selected signatures (the default set unless
`--signatures=` names others) and caches what it finds, one cache entry
per file and signature. A signature whose entry is current for the file
is not scanned again: a file whose every selected signature is current
is reported `current` and is not read. `--force` scans every selected
signature again.

A directory needs `--recursive`: without it the command is refused with
exit 2 and nothing is scanned. With it, every file the
[file tree](../api/endpoints/tree.md) lists under the directory is
scanned: entries whose name starts with a dot only with `--hidden`, and
a symbolic link only where naming its target would be allowed (see
[Symlinks inside a directory search](../concepts/security.md#symlinks-inside-a-directory-search)).
A file whose text is not text (a binary file, a `.tar.gz`) is skipped
with the reason. The files are scanned one at a time, in the order the
paths are given and, under a directory, in the order of the walk: real
directories depth first and by name, then the directories reached
through links. `rx index --recursive` lists the same files in the same
order.

The scan takes the settings `rx trace --signatures` takes:
`RX_WORKERS`, `RX_SIGNATURES_LINE_BYTES`,
`RX_FINDINGS_MAX_PER_SIGNATURE`, `RX_TIMESTAMP_LOOKBACK_KB`,
`RX_LOG_TZ`, `RX_MAX_SIGNATURE_SCANS` and the cache directory
(`RX_CACHE_DIR`), and the masking mode. With masking on the findings are
cached as a masking run caches them, and a run without masking does not
take them for its own (the two modes never share an entry; see
[Masking](../concepts/signatures.md#masking)). `rx serve` caches in the
same directory, so a server started over the same cache and in the same
masking mode answers from what `rx scan` cached.

## Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--signatures` | `string` | the default set | The signatures to scan for: ids, groups, tags, `all`, `default`, `-word` to remove (`--signatures=traceback,sql`); a bare `--signatures` is the default set |
| `-f`, `--force` | `bool` | `false` | Scan every selected signature again, those with current findings too |
| `-r`, `--recursive` | `bool` | `false` | Scan every file under a directory; a directory is refused without it |
| `--json` | `bool` | `false` | Emit machine-readable JSON |
| `--mask-secrets` | `bool` | `RX_MASK_SECRETS` or `false` | Mask what the signatures with `redact: true` find, in the findings cache |

The global flags `--hidden` and `--search-root` apply as on every
command (see the [CLI reference](index.md#global-flags)).

## Exit codes

The codes are those of [`rx index`](line-index.md): every path given is
processed and each failure is reported; the command then exits 3 when
every failure was a file that does not exist, 4 when every failure was
a path outside the roots or a file the process may not read, and 1 when
the failures were of different kinds or a scan failed. A path outside
every `--search-root` exits 4 before anything is read. A directory
without `--recursive`, and a selection that names an unknown signature,
exit 2 before anything is scanned. An interrupt exits 5; the file being
scanned then caches nothing.

A file that a walk of a directory meets and may not read is not a
failure: it is listed under `skipped` with the reason, and the rest of
the tree is scanned.

## Output

One line per file whose findings are known, with its state against the
default set (`full`, `partial` or `none`), its findings and their count
by signature, and `current` when every selected signature was current
and the file was not read; then the skipped files with their reasons,
and a total line. Errors go to stderr.

```text
  logs/app.log: full, 3 findings (error-line 2, python-traceback 1)
  logs/old/worker.log: full, 0 findings
Skipped 1 files:
  logs/core.bin: not a text file: a NUL byte in its first 8 KiB
Total: 3 files, 2 scanned, 1 skipped (0 current), 0 failed, 3 findings in 0.0s
```

The same run a second time reads no file:

```text
  logs/app.log: full, 3 findings (error-line 2, python-traceback 1), current
  logs/old/worker.log: full, 0 findings, current
Skipped 1 files:
  logs/core.bin: not a text file: a NUL byte in its first 8 KiB
Total: 3 files, 0 scanned, 3 skipped (2 current), 0 failed, 3 findings in 0.0s
```

In the totals, a file whose findings were current counts as skipped
(it was not scanned) and as current.

### JSON

```bash
rx scan --recursive logs --json
```

```json
{
  "signatures": "default",
  "files": [
    {
      "path": "logs/app.log",
      "outcome": "scanned",
      "state": "full",
      "counts": {"error-line": 2, "python-traceback": 1},
      "findings": 3
    },
    {
      "path": "logs/old/worker.log",
      "outcome": "scanned",
      "state": "full",
      "counts": {},
      "findings": 0
    }
  ],
  "skipped": [
    {"path": "logs/core.bin", "reason": "not a text file: a NUL byte in its first 8 KiB"}
  ],
  "errors": [],
  "totals": {"files_total": 3, "done": 2, "skipped": 1, "current": 0, "failed": 0, "findings": 3},
  "total_time": 0.042
}
```

| Key | Meaning |
|-----|---------|
| `signatures` | The selection as the run resolved it (`default` for none) |
| `files` | Each file whose findings the run knows, in the order reached: `outcome` is `scanned` or `current`, `state` is the file's state against the default set, `counts` holds the selected signatures that found something (every selected signature was checked), `findings` their sum |
| `skipped` | Each file passed over, with the reason |
| `errors` | Each file that failed, with its error |
| `totals` | `files_total` = `done` + `skipped` + `failed`; `current` is the part of `skipped` whose findings were current |
| `total_time` | Seconds the run took |

## Examples

Scan one file for the default set:

```bash
rx scan /var/log/app.log
```

Scan every file under a directory for tracebacks and SQL only, again
where already current:

```bash
rx scan --recursive /var/log/app --signatures=traceback,sql --force
```

Scan with masking on, so a server started with `--mask-secrets` answers
from the cache:

```bash
rx scan -r /var/log/app --mask-secrets
```

## See also

- [`rx trace --signatures`](trace.md) — scan and show the findings with
  their lines
- [`rx signatures`](signatures.md) — the signatures and the selections
- [`POST /v1/findings`](../api/endpoints/findings.md) — the same scan
  over HTTP
- [Signatures](../concepts/signatures.md) — the model, the cache and
  masking
