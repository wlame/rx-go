# Installation

`rx` is distributed as a single statically-linked binary. It has exactly one
runtime dependency: the `ripgrep` (`rg`) executable must be on `PATH` for
any command that performs regex search.

## System requirements

- **Operating system:** Linux (amd64, arm64) or macOS (arm64, amd64). Windows
  is not an officially supported target.
- **`ripgrep` (`rg`):** required for `rx trace`, `rx samples` against
  uncompressed files, and the HTTP `/v1/trace` and `/v1/samples` endpoints.
  Must be reachable via `$PATH`.
- **Disk:** roughly 2-5% of your largest indexed file for on-disk caches.
  Cache location defaults to `~/.cache/rx/`; override with
  [`RX_CACHE_DIR`](configuration.md).
- **Memory:** depends on workload. A single worker scanning a dense
  literal pattern on a multi-GB file typically peaks at a few hundred MB
  RSS per worker. See [performance](performance.md) for profile numbers.

`rx` itself has no shared-library dependencies and no bundled
`libzstd`. The statically-linked binary is ~13 MB.

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

`rx serve` will report `ripgrep_available: false` on `GET /health` if `rg`
isn't reachable, and will return `503 Service Unavailable` on search
endpoints until one is installed.

## Install `rx`

### Prebuilt binary

Download the archive for your platform from the releases page, extract, and
move the binary into `$PATH`:

```bash
# Example — adjust tag and arch to match your target.
curl -LO https://github.com/wlame/rx-go/releases/download/v2.2.1-go/rx-linux-amd64.tar.gz
tar -xzf rx-linux-amd64.tar.gz
sudo install -m 0755 rx /usr/local/bin/rx

rx --version
# rx version 2.2.1-go
```

### Build from source

Requires Go 1.25 or newer. The build is fully reproducible with CGO disabled.

```bash
git clone https://github.com/wlame/rx-go.git
cd rx-go

CGO_ENABLED=0 go build \
    -ldflags='-s -w -X main.appVersion=2.2.1-go' \
    -o /usr/local/bin/rx \
    ./cmd/rx

rx --version
```

The `-s -w` flags strip debug symbols and the DWARF table, producing a
~13 MB binary. Omit them if you need a symbolized build for debugging.

### Verify installation

```bash
# Should print the version string.
rx --version

# Should print the top-level help with five subcommands.
rx --help
```

If `rx` prints the help page but `rx trace` fails with
"ripgrep (rg) is not installed or not on PATH", complete the
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
