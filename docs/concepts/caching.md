# Caching

`rx` caches two kinds of artifacts on disk:

- **Line indexes** — produced by `rx index`, consumed by `rx trace`,
  `rx samples`, and the HTTP endpoints
- **Trace results** — produced by `rx trace`, consumed by subsequent
  `rx trace` calls with identical inputs

Caches are content-addressed under `~/.cache/rx/` (or
`$RX_CACHE_DIR/rx/`, or `$XDG_CACHE_HOME/rx/`). They are invalidated
when the source file changes (its size, mtime, inode, ctime or
fingerprint), not by TTL.

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
├── indexes/                        — line-offset indexes
│   └── <filename>_<hash16>.json
├── trace_cache/                    — trace-result caches
│   └── <hash>.json
├── frontend/                       — rx-viewer SPA (downloaded once)
│   ├── index.html
│   ├── assets/
│   └── .metadata.json
└── (ephemeral temporaries as needed)
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

Each trace cache entry is keyed by a hash of:

- Source file path
- Pattern set
- The ripgrep flags that change which lines match (`-i`, `-w`, `-x`,
  `-F`, `-P`)

When `rx trace` is invoked, the engine computes the key and looks for
an existing cache file whose source identity still matches the file
(see below). Hit → load and reconstruct the response. Miss → run the
scan, write the response to cache. Only a complete scan of a file at or
above the large-file threshold (`RX_LARGE_FILE_MB`), without
`--max-results`, is written.

`--no-cache` bypasses both the read and write steps.

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
| `--no-index` (on `rx trace`) | Don't consult the line-index cache for this invocation |

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
- **Fractional-second mtimes** are preserved at microsecond precision
  in the cache. Most filesystems provide this; a few network mounts
  and FAT32 do not, which means whole-second mtimes may be compared
  with microsecond-precision cached values. `rx` detects this case
  and treats it as a match.

## Atomic writes

Cache files are written via a temp-file-plus-rename pattern:

1. Write the full content to `<target>.<pid>.tmp`
2. `fsync` the temp file
3. `rename` to the final name (atomic on POSIX)

This guarantees:

- No partially-written cache files on disk (a crash mid-write leaves
  a `.tmp` file that's ignored)
- No torn reads — a concurrent reader either sees the old file or the
  new file, never a half-written state
- Safe to run multiple `rx` instances against the same cache
  concurrently

## Cache size

No hard cap. Caches grow as you index more files. Typical sizes:

- **Index cache**: ~100-500 KB per multi-GB source file
- **Trace cache**: 1 KB - 10 MB per entry, scaling with match count
- **Analyzer cache**: 0 (no analyzers at v1)
- **Frontend cache**: ~15 MB (one-time download)

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

- `rx index` on a 260 MB file: ~1-2 seconds cold, ~10 ms warm
- `rx trace` with a complex pattern: seconds cold, milliseconds warm
  (cache hit)

Pre-warming helps. Indexing is idempotent — running `rx index` once
per file at deploy time amortizes the cost.

### Cache-hit determinism

Repeated calls with identical inputs (and unchanged sources) produce
identical outputs — the cache stores the full response. This is
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
- [Analyzers](analyzers.md) — analyzer cache namespacing
