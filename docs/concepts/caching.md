# Caching

`rx` caches two kinds of artifacts on disk:

- **Line indexes** — produced by `rx index`, `rx compress`, and by the
  first `rx samples` lookup in a large or compressed file; consumed by
  `rx trace`, `rx samples`, and the HTTP endpoints
- **Trace results** — produced by `rx trace`, consumed by subsequent
  `rx trace` calls with the same file, patterns and matching flags

Caches live under `~/.cache/rx/` (or `$RX_CACHE_DIR/rx/`, or
`$XDG_CACHE_HOME/rx/`), named after the source path and, for a trace,
the patterns. They are invalidated when the source file changes (its
size, mtime, inode, ctime or fingerprint), not by TTL.

## Cache location

Resolution order:

1. `$RX_CACHE_DIR/rx` — explicit override (note: `rx` is always
   appended, so `RX_CACHE_DIR=/tmp` yields `/tmp/rx`)
2. `$XDG_CACHE_HOME/rx` — follows the freedesktop spec
3. `~/.cache/rx` — ultimate fallback

Verify at runtime:

```bash
rx serve &
curl -s http://127.0.0.1:7777/health | jq '.constants.CACHE_DIR'
# "/home/you/.cache/rx"
```

The cache base directory is **not created automatically** — callers
that write to it do their own `mkdir` as needed. Read attempts against
a missing cache directory gracefully return "no cache".

## Layout

```text
~/.cache/rx/
├── indexes/                                — line-offset indexes
│   └── <basename>_<path_hash16>.json
├── trace_cache/                            — trace-result caches
│   └── <patterns_hash16>/
│       └── <path_hash16>_<basename>.json
└── frontend/                               — rx-viewer SPA (downloaded once)
    ├── index.html
    ├── assets/
    └── .metadata.json
```

For example, after a trace and an index of one log:

```text
rx/indexes/app.log-2025121008_7d075e46bc77f062.json
rx/trace_cache/65744821eb30e352/7d075e46bc77f062_app.log-2025121008.json
```

### `indexes/`

Each file is named `<basename>_<hash16>.json`:

- `<basename>` is the source file's base name, sanitized for
  filesystem safety
- `<hash16>` is a 16-character hex hash of the absolute source path,
  uniquing entries when multiple files share a basename

Contents: the full `UnifiedFileIndex` struct. See
[line indexes](line-indexes.md).

### `trace_cache/`

A trace cache entry's directory is named after a hash of the pattern
set (sorted) and the ripgrep flags that change which lines match (`-i`,
`-w`, `-x`, `-F`, `-P`); its file name carries a hash of the absolute
source path. `--max-results` is not part of the key.

When `rx trace` is invoked on a plain file of `RX_LARGE_FILE_MB` (50 MB)
or more, or on a seekable zstd file, the engine looks for an entry whose
source identity still matches the file (see below). Hit → load and
reconstruct the response; a request with `--max-results` is answered
from a full entry too, and then returns the first matches in file order
(a capped scan returns whichever chunks finished first). Miss → run the scan. Only a complete scan without
`--max-results` is written: of a plain file of `RX_LARGE_FILE_MB` or
more, or of a seekable zstd file of 1 MB or more. Gzip, bzip2, xz and
plain zstd files are never cached.

`--no-cache` bypasses both the read and write steps.

A hit costs two things. It parses the whole entry, which grows with
the match count. Then it reads the file once, on one core, from the
first match it returns to the last, because the entry stores offsets
and the line text and line numbers come from the file again. A scan
reads the file with one ripgrep per chunk in parallel. On a 465 MB log
in the page cache, a rare pattern took 65 ms to scan and 15 ms from the
cache, but `WARN` with 51,817 matches (a 4.9 MB entry) took 331 ms to
scan and 429 ms from the cache. On a 6.3 GB log the rare pattern took
481 ms to scan and 12 ms from the cache.

Under `--max-results=N` the hit rebuilds only the first N matches and
stops reading the file after them (and after the lines their context
can reach); the entry is still parsed whole. With `--max-results=100`
on the 465 MB log (the `time` field, warm page cache): `WARN` took
30 ms from its 4.6 MB entry and 15 ms to scan; `INFO`, 1,327,224
matching lines, took 0.69 s from its 119 MB entry and 16 ms to scan.
The cached answer numbers every line it returns, which a capped scan of
a plain file may leave as `-1`.

Anomaly detection writes no files of its own. An analysis is part of
the file's line index, which records the window and the detectors (each
with its version) it ran with; a later `--analyze` request with another
window or another detector set rebuilds the index. See
[analyzers](analyzers.md).

### `frontend/`

The `rx-viewer` SPA is extracted here on first `rx serve` start. The
manager writes `.metadata.json` with the SPA version and download
timestamp so subsequent starts can reuse the cached copy. See
[`rx serve`](../cli/serve.md) for the fetch behavior.

## Cache invalidation

### Source identity (default)

An index or trace cache entry is valid only while its source file is
still the file the entry was built from. Each entry records, and each
load compares:

- the file size (`source_size_bytes`)
- the mtime (`source_modified_at`)
- the inode (`source_inode`)
- the inode-change time, ctime (`source_changed_at`)
- a fingerprint: a digest of the size plus the first and last 64 KiB
  (`source_fingerprint`)

Any difference → the entry is **stale**, and the caller rebuilds the
index or scans the file again. The index and the trace cache use the
same check.

A trace cache entry records the file as it was when the scan was
planned, not when it finished. A log that grows during a scan is read
up to the size it had at the start, and the entry says so; if the file
already changed by the time the scan ends, the entry is not written at
all. Either way the next trace scans again and finds the new lines.

**There is no TTL.** A cache entry from a year ago is still valid if
the source file hasn't been touched.

The mtime and the ctime are compared as text at microsecond precision,
exactly; there is no tolerance. Both sides come from the same
filesystem, so a filesystem with whole-second times compares
whole-second times.

### Manual invalidation

```bash
# Remove a specific index.
rx index /var/log/audit-2026-03.log --delete

# Force a rebuild (overwrites any existing cache).
rx index /var/log/audit-2026-03.log --force

# Disable caches for one invocation.
rx trace "error" /var/log/audit-2026-03.log --no-cache --no-index

# Nuke all caches.
rm -rf ~/.cache/rx/
```

Per-invocation flags:

| Flag | Effect |
|---|---|
| `--no-cache` (on `rx trace`) | Disable trace cache for this invocation — no read, no write |
| `--no-index` (on `rx trace`) | Neither read nor write a line index; count lines from the start of the file instead (same answer) |
| `--no-index` (on `rx samples`), `RX_NO_INDEX` | Neither build nor read a line index for this lookup |

### Edge cases

- **Manual mtime changes** (`touch -t ...`) invalidate the cache.
  This is usually what you want.
- **In-place edits that preserve size and mtime** (rare, but possible
  with some rsync configurations) are detected by the ctime, which
  every write moves, and by the fingerprint when the edit touches the
  first or last 64 KiB. A file replaced by rename has a new inode. The
  one case left is an edit confined to the middle of a file that keeps
  its size and mtime, on a filesystem whose ctime does not move; use
  `--force` (index) or `--no-cache` (trace) there.
- **Fractional-second mtimes** are recorded at microsecond precision,
  in the layout rx-python writes, and compared exactly.

## Atomic writes

Cache files are written via a temp-file-plus-rename pattern:

1. Write the full content to a temporary file of the writer's own in
   the same directory, named `.tmp-<random>`
2. `rename` it to the final name (atomic on POSIX)

There is no `fsync`. This guarantees:

- No torn reads — a concurrent reader either sees the old file or the
  new file, never a half-written state
- A failed write leaves no entry under the final name and removes its
  temporary file; a `.tmp-*` left by a crash is never read
- Two writers of the same entry at once (two traces of one file and
  pattern set finishing together) each rename a whole entry; the last
  rename wins

It does not guarantee the entry survives a power loss: the rename can
reach the disk before the content. A truncated entry fails to parse and
is treated as absent, so the cost is a rebuild. A trace entry that
fails to parse is also logged at Warn level as `trace_cache_unreadable`
with its path, and the next complete scan of that file replaces it.

## Cache size

No hard cap. Caches grow as you index more files. Typical sizes:

- **Index cache**: about one checkpoint per MB of source; a 465 MB log
  gave a 9.3 KB index with 429 checkpoints (an analysis adds its
  anomalies)
- **Trace cache**: grows with the match count; 51,817 matches of the
  same log took 4.9 MB
- **Frontend cache**: the size of the rx-viewer bundle

For most developer workstations, the cache stays well under 1 GB.
Production servers that index many large files should monitor cache
growth.

### Manual pruning

No built-in prune command at v1. To remove stale entries:

```bash
# Find stale indexes (source no longer exists).
for entry in ~/.cache/rx/indexes/*.json; do
    src=$(jq -r '.source_path' "$entry")
    [ -e "$src" ] || rm -v "$entry"
done
```

## Tests and CI

For test environments:

- Set `RX_CACHE_DIR=$(mktemp -d)` before running tests that touch the
  cache — gives each run its own isolated cache
- Pass `--no-cache --no-index` to `rx trace` to run without any
  cache interaction — useful for benchmark fairness

## Implications

### Cold vs warm performance

The first query against a file pays the cold build cost:

- `rx index` on a 465 MB log in the page cache: 142 ms to build, 11 ms
  when a valid index exists
- `rx trace` of a rare pattern: 65 ms scanning, 15 ms from the cache
  (see the trace cache section for a dense pattern, where the cache is
  slower)

Pre-warming helps. Indexing is idempotent — running `rx index` once
per file at deploy time amortizes the cost.

### Cache-hit determinism

Repeated calls with identical inputs (and unchanged sources) produce
identical outputs, apart from `request_id` and `time` — the cache stores
the matches and the chunk count of the scan that wrote it. This is
useful for:

- Reproducible CI runs
- Comparing between tool versions (run once, keep the cache, swap
  binaries, re-run)

### Multi-tenant caches

Multiple users sharing a single cache directory works, but each user
sees the other's cache entries. If isolation matters, set per-user
`RX_CACHE_DIR`:

```bash
# In /etc/profile.d/rx.sh
export RX_CACHE_DIR="/var/cache/rx-users/$USER"
```

## Related concepts

- [Line indexes](line-indexes.md) — structure of the indexes stored here
- [Byte offsets vs line numbers](byte-offsets-vs-line-numbers.md) — why
  indexes matter
- [Chunking](chunking.md) — when the trace cache is written
- [Analyzers](analyzers.md) — the analysis stored in an index
