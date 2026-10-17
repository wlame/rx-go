# Installation

`rx` is distributed as a single statically-linked binary. It has exactly one
runtime dependency: the `ripgrep` (`rg`) executable must be on `PATH` for
regex search.

## System requirements

- **Operating system:** Linux (amd64, arm64) or macOS (arm64, amd64). Windows
  is not an officially supported target.
- **`ripgrep` (`rg`):** required for `rx trace`, `rx logs trace` and the
  HTTP `/v1/trace` and `/v1/logs/trace` endpoints. Must be reachable via
  `$PATH`. `rx trace --signatures` without a pattern, `samples`, `index`,
  `compress` and the other endpoints work without it.
- **Disk:** an index is about one checkpoint per MB of source (9.3 KB
  for a 465 MB log); a cached trace answer grows with its match count
  (4.3 MB for 51,817 matches).
  Cache location defaults to `~/.cache/rx/`; override with
  [`RX_CACHE_DIR`](configuration.md).
- **Memory:** grows with the number of matches a trace returns, not
  with the file size: a rare pattern over a 6.3 GB log peaked at 22 MB
  RSS, a pattern with 894,264 matches at 5.9 GB. See
  [performance](performance.md#memory-profile).
- **Network (`rx serve` only):** `rx serve` downloads the `rx-viewer`
  web app at run time from the GitHub releases of `wlame/rx-ui` and
  caches it under `~/.cache/rx/frontend/`. On a host that cannot reach
  GitHub, `--skip-frontend` skips the download, and `RX_FRONTEND_PATH`
  serves a viewer build you copied there. The API works either way. See
  [`rx serve`](cli/serve.md#air-gapped-offline).

`rx` itself has no shared-library dependencies and no bundled
`libzstd`. The statically-linked binary is about 18 MB (17.5 MB for
darwin/arm64, 18.5 MB for linux/amd64).

### What `rx` does *not* need

`ripgrep` is the only external tool. In particular there is no need for
`zstd`, `t2sz`, `gzip`, `xz` or `bzip2` on `PATH`: every compressed
format rx reads or writes is handled inside the binary. A machine with
only `rg` can create a seekable `.zst`, index it, search it and read
lines out of it.

rx-python is the same in this respect — it decompresses through the
`zstandard` package, which installs with it — so the two backends have
the same one dependency.

## Install `ripgrep` first

=== "Linux (apt)"

    ```bash
    sudo apt install ripgrep
    ```

=== "Linux (dnf)"

    ```bash
    sudo dnf install ripgrep
    ```

=== "Linux (pacman)"

    ```bash
    sudo pacman -S ripgrep
    ```

=== "macOS (Homebrew)"

    ```bash
    brew install ripgrep
    ```

=== "From source"

    ```bash
    # Requires Rust toolchain
    cargo install ripgrep
    ```

Verify:

```bash
rg --version
```

`rx serve` looks for `rg` once, when it starts. If `rg` isn't reachable
then, it will report `ripgrep_available: false` on `GET /health` and
return `503 Service Unavailable` on `/v1/trace` and `/v1/logs/trace`
until it is restarted with `rg` installed.

## Install `rx`

### Prebuilt binary

Every `vX.Y.Z` tag publishes one static binary per platform —
`rx-linux-amd64`, `rx-linux-arm64`, `rx-darwin-arm64`, `rx-darwin-amd64` —
each with a `.sha256` sidecar. Download the one for your platform, check
it, and move it into `$PATH`:

```bash
# The latest release; for another one use download/vX.Y.Z in place of
# latest/download.
base="https://github.com/wlame/rx-go/releases/latest/download"
curl -LO "$base/rx-linux-amd64"
curl -LO "$base/rx-linux-amd64.sha256"
sha256sum -c rx-linux-amd64.sha256      # on macOS: shasum -a 256 -c
sudo install -m 0755 rx-linux-amd64 /usr/local/bin/rx

rx --version
# rx version v0.8.0  (the release tag)
```

### Build from source

Requires Go 1.25 or newer. The build is fully reproducible with CGO disabled.
The version comes from the git tag; `just build` stamps it the same way.

```bash
git clone https://github.com/wlame/rx-go.git
cd rx-go

CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.appVersion=$(git describe --tags --dirty --always)" \
    -o /usr/local/bin/rx \
    ./cmd/rx

rx --version
```

The `-s -w` flags strip debug symbols and the DWARF table, producing a
~18 MB binary (17.5 MB for darwin/arm64, 18.5 MB for linux/amd64).
Omit them if you need a symbolized build for debugging.

### Verify installation

```bash
# Should print the version string.
rx --version

# Should print the top-level help with eleven commands: compress, index,
# logs, samples, scan, serve, signatures, time-range and trace, plus
# completion and help.
rx --help
```

If `rx` prints the help page but `rx trace` fails with
"Ripgrep (rg) is not installed or not on PATH", complete the
[ripgrep install](#install-ripgrep-first) step above.

## Shell completion

`rx` uses cobra's built-in completion generator:

=== "Bash"

    ```bash
    rx completion bash > /etc/bash_completion.d/rx
    ```

=== "Zsh"

    ```bash
    rx completion zsh > "${fpath[1]}/_rx"
    ```

=== "Fish"

    ```bash
    rx completion fish > ~/.config/fish/completions/rx.fish
    ```

## Uninstall

```bash
# Remove the binary.
sudo rm /usr/local/bin/rx

# Optional: remove cached indexes, trace caches, frontend SPA.
rm -rf ~/.cache/rx/
```

## See also

- [Quickstart](quickstart.md) — your first 10 minutes with `rx`
- [Configuration](configuration.md) — environment variables and global flags
- [Troubleshooting](troubleshooting.md) — common install-time errors
