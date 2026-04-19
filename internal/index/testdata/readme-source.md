# RX (Regex Tracer)

A high-performance tool for searching and analyzing large files, powered by ripgrep.

## Designed for large files.
RX is optimized for processing multi-GB text files efficiently through parallel chunking and streaming.
If you need to process many files repeatedly, use the API server (`rx serve`) and http calls instead of running CLI commands in a loop. The server mode avoids Python startup overhead on each invocation.

## Main idea in regex searching
RX leverages the speed of [ripgrep](https://github.com/BurntSushi/ripgrep) for fast regex searching and parallel processing of large files. It automatically splits large files into chunks and processes them in parallel, returning precise byte offsets for matches. This approach allows RX to handle very large files efficiently while providing accurate results.
In general, processing large file means calculating offsets for it's parts and running parallel `dd | rg` subprocesses outside of python process, summarizing results afterwards.

## Features
- **Parallel Processing**: Automatic chunking and parallel execution for large files
- **Byte-Offset Based**: Returns precise byte offsets for efficient large file processing (line-based indexing available)
- **Samples output**: Can show arbitrary parts of text files with context when you found interested offsets
- **REST API Server**: All CLI features available via async HTTP API
- **File Analysis**: Extract metadata, statistics, and metrics from files
- **Regex Complexity Analysis**: (Experimental) Detect ReDoS vulnerabilities before production use
- **Compressed File Support**: (Limited) Analyze and search gzip, zstd, xz, bzip2 files transparently
- **Seekable Zstd**: (Limited) Fast random access to seekable zstd compressed files with automatic indexing
- **Analysis Caching**: Cache file analysis results for instant repeated access
- **Background Tasks**: Background compression and indexing files
- **Security Sandbox**: Restrict file access to specific directories in server mode

## Prerequisites

**ripgrep must be installed:**

- **macOS**: `brew install ripgrep`
- **Ubuntu/Debian**: `apt install ripgrep`
- **Windows**: `choco install ripgrep`

## Installation

### Option 0: Download Prebuilt Binary
Download the latest release from the [Releases](https://github.com/wlame/rx-python/releases) page and add it to your PATH as `rx`.

### Option 1: Install from PyPI (in python 3.13+ environment)

```bash
# Requires Python 3.13+
pip install rx-tool

# Now use the rx command
rx --version
rx "error.*" /var/log/app.log
```

**Note:** Requires `ripgrep` to be installed separately (see Prerequisites).

### Option 2: Development with uv

```bash
uv sync
uv run rx "error.*" /var/log/app.log
```

### Option 3: Standalone Binary

```bash
./build.sh
./dist/rx "error.*" /var/log/app.log
```

### Shell Completion

Enable tab completion for `rx` commands and options:

**Zsh** (add to `~/.zshrc`):
```bash
_RX_COMPLETE=zsh_source rx > ~/.rx-complete.zsh
echo 'source ~/.rx-complete.zsh' >> ~/.zshrc
source ~/.zshrc
```

**Bash** (add to `~/.bashrc`):
```bash
_RX_COMPLETE=bash_source rx > ~/.rx-complete.bash
echo 'source ~/.rx-complete.bash' >> ~/.bashrc
source ~/.bashrc
```

**Fish**:
```bash
_RX_COMPLETE=fish_source rx > ~/.config/fish/completions/rx.fish
```

After setup, `rx <Tab>` will suggest subcommands (`index`, `trace`, etc.) and options.

## Quick Start

### Basic Examples

```bash
# Pattern search in a file

# Search a file (returns byte offsets)
rx "error.*" /var/log/app.log

# Search multipatterns in folder
rx -e "error" -e "critical"  --path=/home/log/ --path=/var/log/

# Show context lines
rx "error" /var/log/app.log --samples --context=5

# Show JSON output (for passing to other tools or filtering by jq)
# ---json works with all commands and returns same structures as web API in `serve` mode
rx "error" /var/log/app.log --json

# Search piped input. Not effective due to single-threaded approach. Use rg directly for piping scenarios.
cat /var/log/app.log | rx "error"
docker logs mycontainer | rx "error"


# Indexing and Analysis

# Index a large file (>=50MB) for line-based access
rx index /var/log/huge.log

# Index and analyze file metadata (much slower, tries to find predefined anomalies)
rx index --analyze /var/log/app.log
rx index --analyze --recursive /var/log/

# Check regex complexity (Experimental)
rx check "(a+)+"

# Start API server
rx serve --port=8000  # serves files from current directory
rx serve --search-root=/var/log --search-root=/home/user/data
```

## Why Byte Offsets?

For not-indexed files RX returns **byte offsets** instead of line numbers for efficiency. Seeking to byte position is O(1), while counting lines is O(n). For large files, this matters significantly.
Use the `rx samples` command to extract context lines around byte offsets returned by searches.
Use the `rx index` command to create a line-offset index for large files, enabling fast line-based access.

**Need line numbers?** Use the indexing feature:

```bash
# Show lines around byte offsets
rx samples -b 12345 -b 67890 --context=2 /var/log/huge.log

# Create index for a large file
rx index /var/log/huge.log

# Now you can use line-based operations
rx samples -l 1000 -l 2000 -l 3000 --context=5 /var/log/huge.log
rx samples -l 2000-2050 /var/log/huge.log
```

The index enables fast line-to-offset conversion for files >50MB.

## Server Mode (Recommended for Repeated Operations)

The CLI spawns a Python interpreter on each invocation.
On first launch it tries to fetch and unpack frontend from github [rx-viewer](https://github.com/wlame/rx-viewer/releases) release page to the local cache directory `~/.cache/rx/frontend/` (or RX_FRONTEND_PATH if set).
In case of no internet connection or failure the root `/` will redirect to swagger /docs page only.

For processing multiple files or repeated operations, use the API server:

```bash
# Start server
rx serve --port=8000 --search-root=/var/log --search-root=/home/user/data

# Use HTTP API (same endpoints as CLI)
curl "http://localhost:8000/v1/trace?path=/var/log/app.log&regexp=error"
curl "http://localhost:8000/v1/analyze?path=/var/log/"

# Or open frontend (http://localhost:8000) or swagger (http://localhost:8000/docs) in browser for interactive docs
```

**Benefits of server mode:**
- No Python startup overhead per request
- Async processing with configurable workers
- Webhook support for event notifications
- Security sandbox with `--search-root`


## CLI Commands
Use `rx --help` to see all commands and options.

```bash
%  rx --help
Usage: rx [OPTIONS] COMMAND [ARGS]...

  RX (Regex Tracer) - High-performance file tracing and analysis tool.

  Commands:
    rx <pattern> [path ...]   Trace files for patterns (default command)
    rx index <path>           Create/manage file indexes (use --analyze for full analysis)
    rx check <pattern>        Analyze regex complexity
    rx compress <path>        Create seekable zstd for optimized access
    rx samples <path>         Get file content around byte offsets
    rx serve                  Start web API server

  Examples:
    rx "error" /var/log/app.log
    rx "error.*failed" /var/log/ -i
    rx index /var/log/app.log --analyze
    rx check "(a+)+"
    rx serve --port 8000

  For more help on each command:
    rx --help
    rx index --help
    rx check --help
    rx serve --help

Options:
  --version  Show the version and exit.
  --help     Show this message and exit.

Commands:
  check     Analyze regex pattern complexity and detect ReDoS...
  compress  Create seekable zstd compressed files for optimized rx-tool...
  index     Create or manage file indexes with optional analysis.
  samples   Get file content around specified byte offsets or line numbers.
  serve     Start the RX web API server.
  trace     Trace files and directories for regex patterns using ripgrep.
```

## API Endpoints for Server Mode

Once the server is running, visit http://localhost:8000/docs for interactive API documentation.

**Main Endpoints:**
- `GET /v1/trace` - Search files for patterns
- `GET /v1/index` - File indexing with optional analysis and anomaly detection
- `GET /v1/complexity` - Regex complexity analysis
- `GET /v1/samples` - Extract context lines
- `GET /health` - Server health and configuration

**Background Task Endpoints:**
- `POST /v1/compress` - Start background compression to seekable zstd (returns task_id)
- `POST /v1/index` - Start background indexing for large files (returns task_id)
- `GET /v1/tasks/{task_id}` - Check task status (queued, running, completed, failed)

## Configuration

### Environment Variables

TBD

## Compressed File Support (Experimental, in progress)

RX can search, analyze, and extract samples from compressed files without manual decompression. Supported formats:
- **gzip** (`.gz`)
- **zstd** (`.zst`) - including seekable zstd
- **xz** (`.xz`)
- **bzip2** (`.bz2`)


### Performance Considerations

- Regular compressed files (gzip, xz, bzip2) processed sequentially
- Seekable zstd supports parallel frame access (Tier 2 - coming soon)
- All indexes cached at `~/.cache/rx/indexes/` for instant repeated access

## License

MIT
