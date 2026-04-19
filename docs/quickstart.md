# Quickstart

In 10 minutes you'll:

- Run a first regex search across a multi-GB log
- Build a reusable line-offset index
- Pull context around a specific line number
- Launch the HTTP API and browse the auto-generated docs
- Understand where caches live and how to purge them

This guide assumes you've already [installed `rx` and `ripgrep`](installation.md).

## Step 1 — run a trace

Pick a log file you want to search. For this walkthrough we use
`/var/log/nginx/access.log`, but any large text file works.

```bash
rx "5[0-9]{2} [0-9]+$" /var/log/nginx/access.log
```

The first positional argument is the regex pattern; the rest are paths.
This prints one line per match with the file path, pattern, byte offset,
absolute line number, and matched line text:

```text
/var/log/nginx/access.log: [p1] offset=1024581 line=9812 10.0.0.4 - - ... 502 1284
/var/log/nginx/access.log: [p1] offset=1225104 line=11534 192.0.2.8 - - ... 504 873
--- 2 matches in 1 files (skipped 0) ---
```

!!! tip "Byte offsets are the default"
    `rx` reports byte offsets because they're O(1) to seek. Line numbers
    are computed best-effort. If you need line-number-first output on
    very large files, build an index first — see [Step 3](#step-3-build-an-index).

## Step 2 — get JSON output

Add `--json` to get machine-readable output. `rx` emits the same
schema the HTTP API uses:

```bash
rx "5[0-9]{2} [0-9]+$" /var/log/nginx/access.log --json > matches.json
```

The JSON includes `matches`, `files`, `patterns`, `scanned_files`,
`skipped_files`, `file_chunks`, and the equivalent CLI command under
`cli_command`. See [`cli/trace`](cli/trace.md) for the full shape.

## Step 3 — build an index

Indexing a large file once lets every subsequent line-offset lookup run in
milliseconds instead of seconds. Run:

```bash
rx index /var/log/nginx/access.log
```

Output:

```text
index built for 1 files in 0.852s
  /var/log/nginx/access.log: 5128432 lines, cache=~/.cache/rx/indexes/access.log_a1b2c3d4e5f6....json
```

By default only files ≥ 50 MB are indexed (configurable via
[`RX_LARGE_FILE_MB`](configuration.md)). Small files are reported in the
`skipped` list — not an error, just an efficiency choice.

Run the same command again. The second run returns in ~10 ms because `rx`
sees the cache is still valid (the source file's mtime and size match the
cache metadata).

See [concepts/caching](concepts/caching.md) for what the cache layout looks
like and when entries are invalidated.

## Step 4 — retrieve content by line number

With an index in place, `rx samples` can jump directly to a line:

```bash
rx samples /var/log/nginx/access.log --lines=450000-450010 --context=3
```

`rx` seeks to the starting byte offset of line 450000 via the index,
prints lines 449997-450013, and returns. On a 260 MB file with a warm
index this takes a few milliseconds.

You can request multiple ranges in one call:

```bash
rx samples /var/log/nginx/access.log --lines=100,5000,99999-100010
```

Without an index, line-offset mode still works — `rx` falls back to a
linear scan. For large files this can be slow; building an index is the
right answer when you plan to do more than a couple of line lookups.

## Step 5 — launch the HTTP API

Start the server:

```bash
rx serve --search-root=/var/log --port=7777
```

The startup banner lists the bind address, search roots, docs URL, and
metrics URL:

```text
Starting RX API server on http://127.0.0.1:7777
Search root: /var/log
API docs available at http://127.0.0.1:7777/docs
Metrics available at http://127.0.0.1:7777/metrics
```

Open <http://127.0.0.1:7777/docs> in a browser — you'll see the full
Swagger UI for every endpoint, generated from the OpenAPI 3.1 spec at
`/openapi.json`.

The `--search-root` flag is the sandbox: all path-accepting endpoints will
reject paths that resolve outside `/var/log`. Pass the flag multiple times
to allow multiple roots. See [concepts/security](concepts/security.md).

## Step 6 — call the API

In another terminal:

```bash
# Trace.
curl -s "http://127.0.0.1:7777/v1/trace?path=/var/log/nginx/access.log&regexp=5%5B0-9%5D%7B2%7D+%5B0-9%5D%2B%24" \
    | jq '.matches | length'

# Samples.
curl -s "http://127.0.0.1:7777/v1/samples?path=/var/log/nginx/access.log&lines=450000" \
    | jq '.samples'

# Health check.
curl -s "http://127.0.0.1:7777/health" | jq '.status'
# "ok"

# Prometheus metrics.
curl -s "http://127.0.0.1:7777/metrics" | head -20
```

Requests that return `403 path_outside_search_root` mean the path
resolved outside `/var/log`. Start the server with more roots or move
your test file.

## Step 7 — purge caches

Caches live under `~/.cache/rx/` (or `$RX_CACHE_DIR/rx/`).

```bash
# See what's cached.
ls -la ~/.cache/rx/

# Remove one specific index.
rx index /var/log/nginx/access.log --delete

# Nuke everything.
rm -rf ~/.cache/rx/
```

Caches are also invalidated automatically when the source file's mtime
moves past the cached timestamp. There's no TTL — stale entries only
matter if you set mtimes manually with `touch -t ...`.

## Next steps

- **Learn how chunking works** — [concepts/chunking](concepts/chunking.md)
- **Tune worker count for your hardware** — [performance](performance.md)
- **Set up webhooks for long-running scans** — [api/webhooks](api/webhooks.md)
- **Wire `rx serve` behind a reverse proxy** — [cli/serve](cli/serve.md)
- **Add a custom file analyzer** — [concepts/analyzers](concepts/analyzers.md)

## See also

- [CLI reference](cli/index.md)
- [HTTP API reference](api/index.md)
- [Configuration](configuration.md)
- [Troubleshooting](troubleshooting.md)
