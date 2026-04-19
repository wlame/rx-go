# rx

Regex search, indexing, sampling, and seekable compression for very large text files.

`rx` is a command-line tool and HTTP API server for engineers who work with
multi-gigabyte log files, data dumps, and text corpora. It layers parallel
chunking, line-offset indexes, and seekable compression on top of `ripgrep`
so that repeated searches, line lookups, and content retrieval scale to
files in the tens of gigabytes.

## What makes `rx` different

- **Parallel chunking with newline-aligned boundaries.** A single file is
  split into byte ranges across a goroutine pool; each worker scans its
  range independently. No match is missed or duplicated at chunk seams.
  Scales near-linearly up to physical core count on literal-dense
  patterns.
- **Line-offset indexes with on-disk caching.** Once built, an index maps
  line numbers to byte offsets via sparse checkpoints. Warm-cache reads
  complete in ~10 ms on a 260 MB file regardless of its line count; cold
  builds run at roughly real-time-per-GB.
- **Seekable zstd output.** `rx compress` writes zstd streams as
  independent frames with an appended seek table, trading ~4-5% of
  compression ratio for random-access decompression. Later `rx samples`
  calls can read any line from the compressed file without sequentially
  scanning earlier bytes.

## Quick look

```bash
# Search a 1.3 GB log with 4 workers, show byte offsets of matches.
rx "timeout.*ms" /var/log/app-2026-03.log

# Build a line index so future line-number lookups are instant.
rx index /var/log/app-2026-03.log

# Pull lines 450000-450010 with 3 lines of context on each side.
rx samples /var/log/app-2026-03.log --lines=450000-450010 --context=3

# Turn the log into a seekable archive — random-access line lookups
# remain O(1).
rx compress /var/log/app-2026-03.log

# Start the HTTP API + rx-viewer SPA on port 7777.
rx serve --search-root=/var/log
```

## Where to go next

<div class="grid cards" markdown>

- **[Install `rx`](installation.md)**  
  Binary download, build from source, runtime dependencies.

- **[10-minute Quickstart](quickstart.md)**  
  Install, run your first trace, build an index, launch the API.

- **[CLI reference](cli/index.md)**  
  All five subcommands with flags, defaults, and realistic examples.

- **[HTTP API reference](api/index.md)**  
  Endpoints, request/response schemas, OpenAPI, webhooks.

- **[Concepts](concepts/index.md)**  
  Chunking, byte offsets vs line numbers, caching, analyzers, security.

- **[Performance & tuning](performance.md)**  
  Benchmarks, worker-count advice, when to build an index.

</div>

## Version and license

- Current version: **2.2.1-go**
- License: MIT
- Source: <https://github.com/wlame/rx-go>

## See also

- [Configuration reference](configuration.md)
- [Troubleshooting](troubleshooting.md)
