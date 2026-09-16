# Quickstart

In 10 minutes you'll:

- Run a first regex search across a large log
- Build a reusable line-offset index
- Pull context around a specific line number
- Launch the HTTP API and browse the auto-generated docs
- Understand where caches live and how to purge them

This guide assumes you've already [installed `rx` and `ripgrep`](installation.md).

## Step 1 — run a trace

Pick a log file you want to search. For this walkthrough we use a
572 MB PostgreSQL log, `/var/log/postgresql/postgresql.log`, and look for
statements that took a second or more; any large text file works.

```bash
rx "duration: [0-9]{4}\.[0-9]+ ms" /var/log/postgresql/postgresql.log
```

The first positional argument is the regex pattern; the rest are paths.
`rx` prints a short header, then one line per match with the file path,
the line number, the byte offset and the pattern that matched:

```text
Request ID: 01a0ffdb-5996-7eff-aa8c-cc432806e4ef
Path: /var/log/postgresql/postgresql.log
Pattern: duration: [0-9]{4}\.[0-9]+ ms
Time: 0.067s
Parallel chunks: 20 (1 file(s) chunked)
Matches: 5

Matches (file:line:offset [pattern]):
  /var/log/postgresql/postgresql.log:5160:68901410 [duration: [0-9]{4}\.[0-9]+ ms]
  /var/log/postgresql/postgresql.log:5540:73839234 [duration: [0-9]{4}\.[0-9]+ ms]
  /var/log/postgresql/postgresql.log:6831:79068800 [duration: [0-9]{4}\.[0-9]+ ms]
  /var/log/postgresql/postgresql.log:9872:93021045 [duration: [0-9]{4}\.[0-9]+ ms]
  /var/log/postgresql/postgresql.log:42341:485678857 [duration: [0-9]{4}\.[0-9]+ ms]
```

The file was split into 20 chunks that were searched in parallel. Add
`--samples` to print the matching lines with context around them.

!!! tip "Line numbers and byte offsets"
    Every match carries both. A line number is counted from the start of
    the file, never guessed: a search cut short by `--max-results` that
    has not read the bytes before a match reports its line as `?`
    (`-1` in JSON). `rx samples --offsets=…` resolves those, and an
    index (Step 3) makes the count cheap. See
    [byte offsets vs line numbers](concepts/byte-offsets-vs-line-numbers.md).

## Step 2 — get JSON output

Add `--json` to get machine-readable output. `rx` emits the same
schema the HTTP API uses:

```bash
rx "duration: [0-9]{4}\.[0-9]+ ms" /var/log/postgresql/postgresql.log --json > matches.json
```

The JSON has `request_id`, `path`, `time`, `patterns`, `files`,
`matches`, `scanned_files`, `skipped_files`, `file_chunks`,
`context_lines`, `before_context`, `after_context`, `max_results` and
`cli_command`. `cli_command` is `null` from the CLI; over HTTP it holds
the `rx` command that gives the same answer. See
[`cli/trace`](cli/trace.md) for the full shape.

A file of 50 MB or more also gets its answer cached, so running the same
search again reads the cache instead of the file. See
[concepts/caching](concepts/caching.md).

## Step 3 — build an index

Indexing a large file once lets every later line lookup start from a
nearby checkpoint instead of from the first byte. Run:

```bash
rx index /var/log/postgresql/postgresql.log
```

Output:

```text
Indexed 1 files in 0.1s
  /var/log/postgresql/postgresql.log: 51,329 lines, 571.85 MB
```

By default only files of 50 MB or more are indexed (configurable via
[`RX_LARGE_FILE_MB`](configuration.md) or `--threshold`). A smaller file
is skipped — not an error, just an efficiency choice:

```text
No files indexed.
Skipped 1 files:
  /tmp/small.log: file size 3712 bytes is below threshold 52428800 bytes
```

Run the same command again. The second run prints only
`Indexed 1 files in 0.0s`, because `rx` sees the stored index still
describes the file: same size, modification time, inode, change time,
and the same first and last 64 KiB.

See [concepts/caching](concepts/caching.md) for what the cache layout looks
like and when entries are invalidated.

## Step 4 — retrieve content by line number

`rx samples` jumps to a line and prints it with context:

```bash
rx samples /var/log/postgresql/postgresql.log --lines=5160 --context=1
```

```text
File: /var/log/postgresql/postgresql.log
Context: 1 before, 1 after

=== /var/log/postgresql/postgresql.log:5160:68901410 ===
	                    Heap Fetches: 1
2025-12-10 07:06:25 MST [4241]: [31-1] user=app,db=orders,app=billing,client=127.0.0.1 LOG:  duration: 1520.310 ms  execute <unnamed>/C_12: w…
2025-12-10 07:06:27 MST [4243]: [13-1] user=app,db=orders,app=billing,client=127.0.0.1 LOG:  duration: 204.125 ms  plan:
```

(The long line is cut here; `rx` prints it whole.) The header of each
block names the line and its byte offset. You can request several lines
and ranges in one call:

```bash
rx samples /var/log/postgresql/postgresql.log --lines=100,5000-5002 --context=0
```

With an index in place, `rx` seeks to the nearest checkpoint before the
line and reads forward from there. Without one, `rx samples` builds and
stores the index first when the file is compressed or 50 MB or more —
the first call pays for a full read of the file, later calls are fast.
A smaller plain file is read from the start. `--no-index` (or
`RX_NO_INDEX=true`) reads the file without building or using an index;
the answer is the same.

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
`/openapi.json`. Without `--skip-frontend`, `/` serves the rx-viewer
web app.

The `--search-root` flag is the sandbox: all path-accepting endpoints will
reject paths that resolve outside `/var/log`. Pass the flag multiple times
to allow multiple roots. See [concepts/security](concepts/security.md).

## Step 6 — call the API

In another terminal:

```bash
# Trace.
curl -s "http://127.0.0.1:7777/v1/trace?path=/var/log/postgresql/postgresql.log&regexp=duration%3A+%5B0-9%5D%7B4%7D%5C.%5B0-9%5D%2B+ms" \
    | jq '.matches | length'
# 5

# Samples.
curl -s "http://127.0.0.1:7777/v1/samples?path=/var/log/postgresql/postgresql.log&lines=5160&context=0" \
    | jq '.samples'

# Health check.
curl -s "http://127.0.0.1:7777/health" | jq '.status'
# "ok"

# Prometheus metrics.
curl -s "http://127.0.0.1:7777/metrics" | head -20
```

A path outside the search roots is refused with `403` and a body whose
`error` is `path_outside_search_root`:

```json
{"detail":"path_outside_search_root","error":"path_outside_search_root","message":"path \"/etc/hosts\" is not within any configured --search-root","path":"/etc/hosts","roots":["/var/log"]}
```

Start the server with more roots or move your test file.

## Step 7 — purge caches

Caches live under `~/.cache/rx/` (or `$RX_CACHE_DIR/rx/`).

```bash
# See what's cached.
ls -la ~/.cache/rx/

# Remove one specific index.
rx index /var/log/postgresql/postgresql.log --delete
# deleted index for /var/log/postgresql/postgresql.log

# Nuke everything.
rm -rf ~/.cache/rx/
```

An index or a cached trace answer is used only while it still describes
its file: the same size, modification time, inode and change time, and
the same first and last 64 KiB. Anything else makes `rx` treat it as
absent and rebuild it. There is no TTL.

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
