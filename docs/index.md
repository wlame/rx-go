# rx

Regex search, indexing, sampling, and seekable compression for very large text files.

`rx` is a command-line tool and HTTP API server for engineers who work with
multi-gigabyte log files, data dumps, and text corpora. It layers parallel
chunking, line-offset indexes, and seekable compression on top of `ripgrep`
so that repeated searches, line lookups, and content retrieval scale to
files in the tens of gigabytes.

!!! warning "Intended use"

    `rx` is built for **internal use on a trusted network. It is not
    intended to be exposed to the internet.**

    `rx serve` has no authentication: anyone who can reach the port can
    read any file under `--search-root`. Bind to loopback, reach it over
    a VPN or an SSH tunnel, or front it with an authenticating reverse
    proxy. See [Security](concepts/security.md) for what `rx` does and
    does not take responsibility for.

## What makes `rx` different

- **Parallel chunking with newline-aligned boundaries.** A single file is
  split into byte ranges across a goroutine pool; each worker scans its
  range independently. No match is missed or duplicated at chunk seams.
  Scales near-linearly up to physical core count on literal-dense
  patterns.
- **Line-offset indexes with on-disk caching.** Once built, an index maps
  line numbers to byte offsets via sparse checkpoints. Building one for
  a 6.3 GB log took 2.7 s with the file in the page cache; after that,
  line 40,000,000 comes back in 20 ms instead of 2.5 s.
- **Seekable zstd output.** `rx compress` writes zstd streams as
  independent frames with an appended seek table, trading ~4-5% of
  compression ratio for random-access decompression. Later `rx samples`
  calls can read any line from the compressed file without sequentially
  scanning earlier bytes.

## Quick look

```bash
# Search a log; every match comes with its line number and byte offset.
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

- Version: the release tag (`rx --version` prints it); see the
  [releases](https://github.com/wlame/rx-go/releases)
- License: MIT
- Source: <https://github.com/wlame/rx-go>

## See also

- [Configuration reference](configuration.md)
- [Troubleshooting](troubleshooting.md)
