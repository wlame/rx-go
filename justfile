# rx (Go) — the single dev entrypoint. CI runs these same recipes.

set shell := ["bash", "-uc"]

# Tools installed with `go install` land here.
export PATH := env_var("HOME") + "/go/bin:" + env_var("PATH")

# The version a build stamps into the binary. Tags are the source of truth;
# there is no version constant in the source.
version := `git describe --tags --dirty --always 2>/dev/null || echo dev`

# Coverage floor. 82.4% today; raise it as coverage improves, never lower it
# to make a red build green. Recorded in AGENTS.md.
coverage_min := "80"

# List all recipes
default:
    @just --list --unsorted

# ── build ────────────────────────────────────────────────────────────────

# Build the static rx binary into dist/
build:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    CGO_ENABLED=0 go build -ldflags '-s -w -X main.appVersion={{version}}' -o dist/rx ./cmd/rx
    echo "built dist/rx {{version}}"

# Cross-compile the release binaries with their sha256 sidecars
build-all:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
        os="${target%/*}"; arch="${target#*/}"
        out="dist/rx-${os}-${arch}"
        GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
            go build -ldflags '-s -w -X main.appVersion={{version}}' -o "$out" ./cmd/rx
        ( cd dist && sha256sum "$(basename "$out")" > "$(basename "$out").sha256" )
        echo "built $out"
    done

# Install rx into $GOBIN (or ~/go/bin) with shell completions
install: build
    #!/usr/bin/env bash
    set -euo pipefail
    gobin="${GOBIN:-$HOME/go/bin}"
    mkdir -p "$gobin"
    install -m 0755 dist/rx "$gobin/rx"
    echo "installed $gobin/rx"
    # Completions are best-effort: a missing or unwritable directory must
    # not fail the install.
    for shell in bash zsh fish; do
        case "$shell" in
            bash) dir="$HOME/.local/share/bash-completion/completions"; name="rx" ;;
            zsh)  dir="$HOME/.local/share/zsh/site-functions";          name="_rx" ;;
            fish) dir="$HOME/.config/fish/completions";                 name="rx.fish" ;;
        esac
        if mkdir -p "$dir" 2>/dev/null && "$gobin/rx" completion "$shell" > "$dir/$name" 2>/dev/null; then
            echo "installed $shell completions into $dir"
        else
            echo "warning: could not install $shell completions" >&2
        fi
    done

# Run rx from source (e.g. just run trace error app.log)
run *args:
    go run ./cmd/rx {{args}}

# Remove build and coverage artifacts
clean:
    rm -rf dist/ bin/ cover.out coverage.out coverage.html site/

# ── quality gates ────────────────────────────────────────────────────────

# Format every Go file
fmt:
    gofmt -s -w .

# Fail when a file is not gofmt-clean (CI gate)
fmt-check:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(gofmt -s -l .)
    if [ -n "$out" ]; then echo "not gofmt-clean:"; echo "$out"; exit 1; fi

# go vet every package
vet:
    go vet ./...

# golangci-lint (config in .golangci.yml)
lint:
    golangci-lint run ./...

# Fail when go.mod or go.sum is not tidy (CI gate)
tidy-check:
    go mod tidy -diff

# Reachable-CVE scan. Needs network access to vuln.go.dev.
vuln:
    govulncheck ./...

# ── tests ────────────────────────────────────────────────────────────────

# Run the unit tests (e.g. just test -run TestTrace ./internal/trace/)
test *args:
    go test {{args}} ./...

# Run the tests with the race detector — mandatory before merge
test-race:
    go test -race -count=1 ./...

# Hunt flaky tests by repeating a package (e.g. just test-repeat ./internal/trace/)
test-repeat pkg='./...':
    go test -race -count=10 {{pkg}}

# Benchmarks. Not a CI gate; for local before/after comparison.
bench *args:
    go test -run='^$' -bench=. -benchmem {{args}} ./...

# Tests with the coverage floor
cover:
    #!/usr/bin/env bash
    set -euo pipefail
    go test -race -coverprofile=cover.out -covermode=atomic ./...
    total=$(go tool cover -func=cover.out | awk '/^total:/ {gsub(/%/,"",$3); print $3}')
    echo "total coverage: ${total}%  (floor: {{coverage_min}}%)"
    awk -v t="$total" -v m="{{coverage_min}}" 'BEGIN { exit (t+0 >= m+0) ? 0 : 1 }' \
        || { echo "FAIL: coverage ${total}% is below the {{coverage_min}}% floor"; exit 1; }

# ── aggregates ───────────────────────────────────────────────────────────

# Exactly what GitHub CI enforces, in the same order
ci: fmt-check vet lint tidy-check test-race docs-build

# The full pre-push battery
check: ci cover build vuln

# ── docs ─────────────────────────────────────────────────────────────────

# Build the mkdocs site into site/ (strict: warnings fail the build)
docs-build:
    uv run --no-project --with-requirements=docs/requirements.txt mkdocs build --strict

# Serve the docs with live reload
docs-serve:
    uv run --no-project --with-requirements=docs/requirements.txt mkdocs serve

# ── release ──────────────────────────────────────────────────────────────

# Print the version a build would stamp
version:
    @echo {{version}}

# Cut a release (major|minor|patch): gates, changelog, commit, tag. Never pushes.
release part='patch':
    #!/usr/bin/env bash
    set -euo pipefail
    just ci
    ./scripts/release.sh {{part}}

# Show what a release would do, changing nothing
release-dry part='patch':
    @./scripts/release.sh {{part}} --dry-run

# Print one version's changelog section (e.g. just release-notes 0.1.0)
release-notes version:
    @awk '/^## \[{{version}}\]/{f=1;next} /^## \[/{f=0} f' CHANGELOG.md

# ── product ──────────────────────────────────────────────────────────────

# Start the API server with the viewer (e.g. just serve --search-root=/var/log)
serve *args:
    go run ./cmd/rx serve {{args}}

# List the anomaly detectors this build registers
detectors:
    @go run ./cmd/rx index --help | sed -n '/analyze/,$p'

# Diff CLI output against rx-python for the same arguments
parity *args:
    #!/usr/bin/env bash
    set -euo pipefail
    go run ./cmd/rx {{args}} --json > /tmp/rx-go.json
    ( cd ../rx-python && uv run rx {{args}} --json ) > /tmp/rx-python.json
    diff /tmp/rx-go.json /tmp/rx-python.json && echo "identical"
