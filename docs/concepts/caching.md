# Caching

`rx` caches three kinds of artifacts on disk:

- **Line indexes** — produced by `rx index`, `rx compress`, and by the
  first `rx samples` lookup in a large or compressed file; consumed by
  `rx trace`, `rx samples`, and the HTTP endpoints
- **Trace results** — produced by `rx trace`, consumed by subsequent
  `rx trace` calls with the same file, patterns and matching flags
- **Findings** — produced by signature scans, one entry per file and
  [signature](signatures.md); a later scan of the file reads the
  signatures it holds and scans only the others

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

A relative `RX_CACHE_DIR` or `XDG_CACHE_HOME` is taken against the
directory `rx` starts in and used as that absolute path for the whole
run: `cd /srv && RX_CACHE_DIR=cache rx serve` keeps its cache in
`/srv/cache/rx`, and `/health` reports that path.

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
├── findings/                               — findings of signature scans
│   └── <basename>_<path_hash16>/
│       ├── meta.json
│       ├── meta.ranges.json
│       └── <signature-id>.json
└── frontend/                               — rx-viewer SPA (checked daily)
    ├── index.html
    ├── assets/
    └── .metadata.json
```

For example, after a trace and an index of one log:

```text
rx/indexes/app.log-2025121008_3f9c1e07a2b45d18.json
rx/trace_cache/c41a8e2f90d7b356/3f9c1e07a2b45d18_app.log-2025121008.json
```

### `indexes/`

Each file is named `<basename>_<hash16>.json`:

- `<basename>` is the source file's base name, sanitized for
  filesystem safety
- `<hash16>` is a 16-character hex hash of the absolute source path,
  uniquing entries when multiple files share a basename

A file name is at most 255 bytes, the longest name ext4, xfs, btrfs and
APFS accept. A base name longer than 233 bytes is cut to 233 (never
inside a UTF-8 character), so a log with a very long name still gets an
index; the hash of the whole path keeps two such names apart. A trace
cache entry's name, `<path_hash16>_<basename>.json`, is cut the same
way. A filesystem with a shorter limit (eCryptfs allows about 143
bytes) still refuses the longest names.

Contents: the full `UnifiedFileIndex` struct. See
[line indexes](line-indexes.md).

### `trace_cache/`

A trace cache entry's directory is named after a hash of the pattern
set (sorted) and the ripgrep flags that change which lines match (`-i`,
`-w`, `-x`, `-F`, `-P`); its file name carries a hash of the absolute
source path. `--max-results` is not part of the key.

Because the pattern set is sorted, `-e WARN -e NEEDLE` and
`-e NEEDLE -e WARN` share one entry. The entry lists the patterns in
the order of the search that wrote it, and each stored match names its
pattern by its place in that list; a hit translates it to the reader's
own pattern IDs, so every match carries the ID of the pattern it
matched in the reader's order. A pattern given twice is paired copy by
copy. An entry whose list is not the reader's patterns, or whose match
names a place the list does not have, is a miss: the trace scans and
writes a new entry.

When `rx trace` is invoked on a plain file of `RX_LARGE_FILE_MB` (50 MB)
or more, or on a seekable zstd file, the engine looks for an entry whose
source identity still matches the file (see below). Hit → load and
reconstruct the response; a request with `--max-results` is answered
from a full entry too, and then returns the first matches in file order
(a capped scan returns whichever chunks finished first). Miss → run the scan. Only a complete scan without
`--max-results` is written: of a plain file of `RX_LARGE_FILE_MB` or
more, or of a seekable zstd file of 1 MB or more. Gzip, bzip2, xz and
plain zstd files are never cached. A scan whose answer cuts a line
(`RX_MAX_LINE_TEXT_BYTES`) or leaves submatches out
(`RX_MAX_SUBMATCHES_PER_LINE`) is not written either, so a dense
pattern on very long lines is scanned every time. Nor is a scan of a
seekable zstd file with a damaged frame: its answer lacks the lines the
damage touches, and a stored copy would outlive the damage.

`--no-cache` bypasses both the read and write steps.

A hit costs two things. It parses the whole entry, which grows with
the match count. Then it reads the file once, on one core, from the
first match it returns to the last, because the entry stores each
match's offset and the start and end of its submatches, and the line
text, the submatch text and the line numbers come from the file again.
The submatches are the spans ripgrep reported in the scan, so a hit
gives the same ones for every engine and flag, `-P` included; a hit
under lower `RX_MAX_LINE_TEXT_BYTES` or `RX_MAX_SUBMATCHES_PER_LINE`
bounds cuts them as a scan under those bounds does. An entry written
before the spans were stored is a miss. A scan
reads the file with one ripgrep per chunk in parallel. On a 465 MB log
in the page cache, a rare pattern took 65 ms to scan and 15 ms from the
cache, but `WARN` with 51,817 matches (a 4.3 MB entry) took 96 ms to
scan and 183 ms from the cache (the `time` field). On a 6.3 GB log the rare pattern took
481 ms to scan and 12 ms from the cache.

Under `--max-results=N` the hit rebuilds only the first N matches and
stops reading the file after them (and after the lines their context
can reach); the entry is still parsed whole. With `--max-results=100`
on the 465 MB log (the `time` field, warm page cache): `WARN` took
39 ms from its 4.3 MB entry and 15 ms to scan; `INFO`, 1,327,224
matching lines, took 1.0 s from its 110 MB entry and 16 ms to scan.
The cached answer numbers every line it returns, which a capped scan of
a plain file may leave as `-1`.

That is the only difference a cache or a line index may make to an
answer. With them or without them, and with `--no-index`, every field
of an answer is equal, except that a line number which is `-1` in one
answer may be the true line number in the other. `-1` means "not
computed", never "no such line", and a number rx fills in is always the
line that holds the match's byte offset. The fields that describe how
the answer was produced are outside this rule: `request_id`, `time`,
`cli_command` and `file_chunks`. The chunk settings
(`RX_MIN_CHUNK_SIZE_MB`, `RX_MAX_SUBPROCESSES`) are not part of the
cache key, so a cache hit reports the chunk count of the scan that
wrote the entry, while a scan with other settings reports its own. (A
capped search of a chunked plain file may keep different matches on
each run whatever the cache holds; see [the matches a capped search
returns are
arbitrary](../cli/trace.md#the-matches-a-capped-search-returns-are-arbitrary).)

### `findings/`

Each scanned file has a directory, named as its line index is
(`<basename>_<hash16>`, the base name sanitized and cut the same way).
It holds:

- `meta.json` — the file's absolute path, `source_path`, and its
  identity (the fields the line index records: `source_size_bytes`, `source_mtime_ns`, `source_inode`,
  `source_device`, `source_ctime_ns`, `source_fingerprint`), the cache
  format `version` (1) and, per signature, its `key`, its `count` (the
  findings in the file, past the cap too), how many are `stored`, and
  when it was scanned (`scanned_at`, UTC); and `masked_ranges`, the
  `key` and `count` of the file's masked ranges;
- `meta.ranges.json` — the file's masked ranges: its `key` and the
  `ranges`, `[first, last]` line numbers of the body lines of every
  redacting block and run (see [Masking](signatures.md#masking)),
  sorted, the same for every chunking and storage; and the end of the
  text the scan read, `text_lines` and `text_bytes` (the decompressed
  text of a compressed file), past which no text of a finding is shown.
  Every scan writes it, since every scan walks every loaded redacting
  block and run;
- `<signature-id>.json` — one entry: its `key`, its `findings` (lines,
  text offsets, `time_ms` and `time_offset_minutes`, the zone offset
  the stamped line of that time writes, the excerpt with every secret
  masked, the value span, `masked`) and, for a `density` measure, its
  bucket `series`; a signature whose id is `meta` gets
  `meta.signature.json`;
- `.lock` — the file a writing process locks.

A signature checked with no findings is an entry with `count: 0`. No
unmasked value of a redacting signature is ever written ([Masking](signatures.md#masking)): every excerpt
is masked by every loaded redacting signature before it is stored, and
an excerpt on a body line of a redacting block or run is stored as
`[masked, N bytes]`. The text of a finding, read from the file when an
answer is made, shows every line of a masked range as
`[masked, N bytes]`, so cached entries are used only together with a
current `meta.ranges.json`.

**Current entries.** An entry is current when `meta.json` still
describes the file (the identity check below) and the entry's key is
the one the signature has now. The key is a hash of what decides the
signature's findings and nothing else; `rx signatures show ID` prints
it.

| Change | Effect |
|---|---|
| the file (its size, mtime, inode, device, ctime or fingerprint) | every entry of the file is stale |
| a signature's matching part: kind, anchor and every regex, keywords, `not`, `capture`, `entropy`, `validate`, the block, run or measure parameters, `redact` | its entry is stale in every file |
| any redacting signature (one with `redact: true`, selected or not) added, removed or changed | every entry is stale (excerpts are masked by all of them) |
| the engine's version, or a measure's version | the affected entries are stale |
| `RX_SIGNATURES_LINE_BYTES`, `RX_FINDINGS_MAX_PER_SIGNATURE`, `RX_TIMESTAMP_LOOKBACK_KB`, `RX_LOG_TZ` | every entry is stale |
| a signature's name, description, group, tags, level, links, metadata, examples, `default` | nothing: read from the catalogue when the answer is made |
| `RX_WORKERS`, the chunk sizes | nothing: they never change a finding |
| the cache format version | the directory counts as absent |

The masked ranges have a key of their own: the engine's version,
`RX_SIGNATURES_LINE_BYTES` (the line view decides whether a line opens
or continues a block) and the redacting signatures. A file whose
`meta.ranges.json` is missing, unreadable, of another key or count
than `meta.json` records, not sorted as a scan writes it, or whose end
of the text lies before a range or a cached finding, has no current
entry: its signatures are scanned again, and `meta.ranges.json` is
written anew.

A stale entry is overwritten the next time its signature is scanned;
nothing prunes the directories of deleted files, as for `indexes/`. The
findings cache does not depend on the line index: deleting an index
keeps the findings.

**Full, partial, none.** A file is *full* when every signature of the
default set (every loaded signature without `default: false`) has a
current entry; *partial* when it has at least one current entry, opt-in
signatures included, and is not full; *none* when it has no current
entry. A missing opt-in signature never keeps a file from full, but a
file scanned only with opt-in signatures is partial: it was scanned,
not with the defaults. The count shown for a file is the sum of `count`
over all its current entries, opt-in ones included. Reading the state reads `meta.json`
only: it compares the identity by the file's stat (size, mtime, inode,
device, ctime) and leaves the fingerprint, which needs a read of the
file, to the scan.

**A scan.** A scan splits the signatures asked for into those with a
current entry, read from the cache, and the others, which it scans in
one pass over the file. It takes the file's identity before it reads
the cache and again after the scan: when the file changed meanwhile the
scan answers and writes nothing, and when the file got shorter (a log
truncated in place) or its path leads to another file the scan fails
with an error that names it, since its answer could point past the end
of the file. A forced scan scans every signature asked for and replaces
their entries; a scan without the cache reads and writes nothing, and
piped input is never cached. A canceled or failed scan writes nothing.

In one process one scan of a file runs at a time: a second request for
the file waits for the first, then scans only what is still missing.
At most `RX_MAX_SIGNATURE_SCANS` scans read files at once; the others
wait their turn, and a scan answered wholly from the cache does not
wait.

### `frontend/`

The `rx-viewer` SPA is extracted here on first `rx serve` start. The
manager writes `.metadata.json` with the SPA version, the download time
and `last_check`, the last time GitHub was asked for a newer release.
A later start serves the cached copy and, once `last_check` is a day
old, asks again: a newer release inside the range this `rx` supports
replaces the cache. A cached viewer outside that range counts as no
cache. See [`rx serve`](../cli/serve.md#viewer-updates) for the
details.

## Cache invalidation

### Source identity (default)

An index, trace cache or findings entry is valid only while its source
file is still the file the entry was built from. Each entry records, and
each load compares:

- the file size (`source_size_bytes`)
- the mtime, in nanoseconds since the Unix epoch (`source_mtime_ns`)
- the inode (`source_inode`) and the device that holds it
  (`source_device`)
- the inode-change time, ctime, in nanoseconds since the Unix epoch
  (`source_ctime_ns`)
- a fingerprint: a digest of the size plus the first and last 64 KiB
  (`source_fingerprint`)

Each entry also records the mtime and the ctime as local wall-clock
text (`source_modified_at`, `source_changed_at`) for a person reading
the file. That text is never compared.

Any difference → the entry is **stale**, and the caller rebuilds the
index or scans the file again. The index, the trace cache and the
findings cache use the same check.

A trace cache entry records the file as it was when the scan was
planned, not when it finished. A log that grows during a scan is read
up to the size it had at the start, and the entry says so; if the file
already changed by the time the scan ends, the entry is not written at
all. Either way the next trace scans again and finds the new lines.

**There is no TTL.** A cache entry from a year ago is still valid if
the source file hasn't been touched.

The mtime and the ctime are compared as nanosecond counts, exactly;
there is no tolerance. Both sides come from the same filesystem, so a
filesystem with whole-second times compares whole-second times.

**The time zone does not matter.** A nanosecond count names one instant
whatever `TZ` is, so an `rx serve` started with `TZ=UTC` (as containers
and systemd units often are) and a CLI in the user's zone share one
cache and reuse each other's entries. A daylight-saving change does not
make an entry stale either, and an mtime moved by an hour inside the
hour the change repeats is still seen as a change. Indexes written by
rx-go before version 7 compared the local-time text, and are rebuilt
once.

**The device number can change** when a filesystem is mounted again
(network filesystems, removable media, some container overlays). An
entry recorded under the old number is then stale and is rebuilt; the
answer is never different.

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
| `--no-cache` (on `rx trace`), `no_cache=true` (on `GET /v1/trace`) | Disable trace cache for this invocation — no read, no write; with `--signatures` (`signatures=`), the findings cache too |
| `--no-index` (on `rx trace`), `no_index=true` (on `GET /v1/trace`) | Neither read nor write a line index; count lines from the start of the file instead (same answer) |
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
- **Fractional-second mtimes** are compared at the precision the
  filesystem reports, down to the nanosecond.
- **Piped input** (`… | rx trace -e PATTERN -`) is copied to a
  temporary file that is deleted when the command ends. No trace-cache
  entry is written for it; a file named beside `-` is cached as usual.

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

A line index that cannot be read (its permissions, an I/O error) or
parsed (cut short) is treated as absent too, and logged at Warn level
as `index_unreadable` with its path. A lookup never fails because of
it: `rx samples` and `GET /v1/samples` rebuild it over the damaged one,
whatever the file's size, and read the file without an index when it
cannot be rebuilt; `GET /v1/index` answers `404` for it as for a
missing one; a trace leaves a match it cannot number cheaply at `-1`.

### The findings cache

A findings cache directory is written in a fixed order, under an
advisory lock (`flock` on its `.lock` file) that keeps other processes
from writing it at the same time:

1. each new `<signature-id>.json`, then `meta.ranges.json`, by
   temporary file and rename;
2. `meta.json` read again: an entry another process added since this
   scan read the cache is kept, when `meta.json` still describes the same
   file;
3. `meta.json`, by temporary file and rename: the commit point;
4. every entry file `meta.json` does not name is removed, and every
   temporary file left by a crash.

Every operation on the cache goes through the per-file directory opened
as a root that cannot be left, and nothing below `findings/` is
followed as a symbolic link. A per-file directory that is a link (to
anywhere, inside the cache or out), or a `meta.json`, a
`meta.ranges.json`, an entry file or a `.lock` that is a link or a
special file, is never read, written or
deleted through: that file's findings are scanned and not cached, an
entry or `meta.ranges.json` that is a link counts as missing (a later write replaces the link
with a file of the cache), and the first such case of a process is
logged at Warn level as `findings_cache_unsafe_entry` with its path. A
removal removes a link itself, never what it points at. Another local
user who can write a shared `RX_CACHE_DIR`, but not the cache base
itself, can so make rx scan again, never make it write or delete a file
outside the cache. The `findings/` directory itself is the root of this
tree and is followed when it is a link, as `indexes/` and
`trace_cache/` are: linking it is the operator's choice of where the
findings live, and rx then creates its per-file directories there.

A reader that meets a file another writer has just renamed into place
(between its check and its open) checks again, up to three times; a
file replaced before every try is a miss, without a warning. Only a
link or a special file is logged.

A reader trusts only what `meta.json` names, and an entry file whose
key differs from the one `meta.json` names, or that is missing or cut
short, counts as missing: that signature is scanned again. So a crash
between steps 1 and 3 leaves the old answer readable, and two processes
that add different signatures to one file keep both. A lock another
process holds for more than 10 seconds is given up on: the scan answers
and writes nothing. On a filesystem that cannot lock (some network
filesystems) the writer goes on without the lock; step 2 still keeps
the other writer's entries, but two writes at the same instant may lose
one entry, which costs a scan and never a wrong answer. A `meta.json`
that cannot be read or parsed is logged at Warn level as
`findings_cache_unreadable` and treated as absent.

## When the cache cannot be written

A cache directory that is read-only, or an `RX_CACHE_DIR` that names a
regular file, makes every write fail. Lookups still answer, and answer
the same:

- `rx samples` and `GET /v1/samples` check that an index can be stored
  before they build one. When it cannot, they build nothing and read
  the file without an index, as `--no-index` does, and log one Warn
  line per process, `index_not_stored`, naming the directory and the
  cause. `rx serve` starts no index task for such a lookup.
- `rx index` and `POST /v1/index` fail before they read the file, with
  an error that starts `cannot store the line index:` and names the
  cause; `rx index` exits `1`.
- `rx trace` scans and logs one Warn line per process,
  `trace_cache_write_failed`, with the cause. A signature scan answers
  and logs one Warn line per process, `findings_cache_write_failed`. A cache directory under a
  regular file holds no entries, so looking one up is an ordinary miss;
  `trace_cache_unreadable` is only for an entry that exists and cannot
  be read or parsed.

## Cache size

No hard cap. Caches grow as you index more files. Typical sizes:

- **Index cache**: about one checkpoint per MB of source; a 465 MB log
  gave a 9.3 KB index with 429 checkpoints
- **Trace cache**: grows with the match count; 51,817 matches of the
  same log took 4.3 MB
- **Findings cache**: a few small JSON files per scanned file, one per
  signature checked; each holds at most `RX_FINDINGS_MAX_PER_SIGNATURE`
  findings with their excerpts of at most 200 bytes
- **Frontend cache**: the size of the rx-viewer bundle

For most developer workstations, the cache stays well under 1 GB.
Production servers that index many large files should monitor cache
growth.

### Manual pruning

No built-in prune command at v1. To remove the indexes, trace
entries and findings whose source file no longer exists:

```bash
# The cache base, resolved as rx resolves it.
cache=${RX_CACHE_DIR:+$RX_CACHE_DIR/rx}
cache=${cache:-${XDG_CACHE_HOME:-$HOME/.cache}/rx}
for entry in "$cache"/indexes/*.json "$cache"/trace_cache/*/*.json; do
    [ -e "$entry" ] || continue
    src=$(jq -r '.source_path' "$entry")
    [ -e "$src" ] || rm -v "$entry"
done
# A file's findings are a directory; its meta.json names the source.
for meta in "$cache"/findings/*/meta.json; do
    [ -e "$meta" ] || continue
    src=$(jq -r '.source_path' "$meta")
    [ -e "$src" ] || rm -rv "$(dirname "$meta")"
done
```

Every entry records its source as an absolute path, so the loop gives
the same result from any directory. An index written by rx-go before
format version 7 may record the path as it was typed, relative to the
directory it was built from; such an index is never used again, so the
loop removing it costs nothing.

## Tests and CI

For test environments:

- Set `RX_CACHE_DIR=$(mktemp -d)` before running tests that touch the
  cache — gives each run its own isolated cache
- Pass `--no-cache --no-index` to `rx trace` to run without any
  cache interaction — useful for benchmark fairness

## Implications

### Cold vs warm performance

The first query against a file pays the cold build cost:

- `rx index` on a 465 MB log in the page cache: 120 ms to build, 11 ms
  when a valid index exists
- `rx trace` of a rare pattern: 65 ms scanning, 15 ms from the cache
  (see the trace cache section for a dense pattern, where the cache is
  slower)

Pre-warming helps. Indexing is idempotent — running `rx index` once
per file at deploy time amortizes the cost.

### Cache-hit determinism

Repeated calls with identical inputs (and unchanged sources) produce
identical outputs, apart from `request_id` and `time` — the cache stores
the matches and the chunk count of the scan that wrote it. A hit reports
that chunk count even when the chunk settings have changed since, so
`file_chunks` is the one field besides those two that can differ from a
fresh scan's (see the rule above). This is useful for:

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
- [Signatures](signatures.md) — what the findings cache holds
