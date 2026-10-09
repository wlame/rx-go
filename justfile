# rx (Go) — the single dev entrypoint. CI runs these same recipes.

set shell := ["bash", "-uc"]

# Tools installed with `go install` land here.
export PATH := env_var("HOME") + "/go/bin:" + env_var("PATH")

# The version a build stamps into the binary. Tags are the source of truth;
# there is no version constant in the source.
version := `git describe --tags --dirty --always 2>/dev/null || echo dev`

# The recipes read the version as "$BUILD_VERSION", never as {{version}}.
# It holds a tag name, and git allows a quote, `;`, `$(` or a backtick in
# one: pasted into a recipe line, the shell would run that part of it.
# Read from the environment, it stays one word. The name has no RX_
# prefix, so `GET /health` of an rx started by `just serve` does not list
# it among rx's settings.
export BUILD_VERSION := version

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
    CGO_ENABLED=0 go build -ldflags "-s -w -X main.appVersion=$BUILD_VERSION" -o dist/rx ./cmd/rx
    echo "built dist/rx $BUILD_VERSION"

# Cross-compile the release binaries with their sha256 sidecars
build-all:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
        os="${target%/*}"; arch="${target#*/}"
        out="dist/rx-${os}-${arch}"
        GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
            go build -ldflags "-s -w -X main.appVersion=$BUILD_VERSION" -o "$out" ./cmd/rx
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

# Run rx from source; each argument reaches rx as one word (just run trace 'a b' app.log)
[positional-arguments]
run *args:
    go run ./cmd/rx "$@"

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

# Fail when a comment cites the process that produced the code (CI gate)
scaffolding-check:
    ./scripts/no-build-scaffolding.sh

# Copy the golden OpenAPI document to docs/, where it is published
spec-sync:
    cp internal/webapi/testdata/openapi.golden.json docs/api/openapi.json

# Fail when the published spec is not the golden one (CI gate)
spec-check:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! diff -u docs/api/openapi.json internal/webapi/testdata/openapi.golden.json; then
        echo "docs/api/openapi.json is stale — run \`just spec-sync\` and commit the result" >&2
        exit 1
    fi

# Reachable-CVE scan. Needs network access to vuln.go.dev.
vuln:
    govulncheck ./...

# ── tests ────────────────────────────────────────────────────────────────

# Every go test run below goes through scripts/test-isolated-home.sh: it
# runs the tests under a throwaway HOME and fails when one of them wrote an
# rx cache file there instead of into its package's own RX_CACHE_DIR.
#
# test_timeout replaces go test's 10-minute default per package:
# internal/trace alone takes over 5 minutes under -race on a 16-core
# machine, and CI runners have 3 to 4 cores.
test_timeout := "30m"

# Run the unit tests; each argument reaches go test as one word (-run='A|B')
[positional-arguments]
test *args:
    ./scripts/test-isolated-home.sh go test -timeout={{test_timeout}} "$@" ./...

# Run the tests with the race detector — mandatory before merge
test-race:
    ./scripts/test-isolated-home.sh go test -race -count=1 -timeout={{test_timeout}} ./...

# Hunt flaky tests by repeating a package (e.g. just test-repeat ./internal/trace/)
[positional-arguments]
test-repeat pkg='./...':
    ./scripts/test-isolated-home.sh go test -race -count=10 "$1"

# Benchmarks, not a CI gate; each argument reaches go test as one word (-bench='A|B')
[positional-arguments]
bench *args:
    ./scripts/test-isolated-home.sh go test -run='^$' -bench=. -benchmem "$@" ./...

# Tests with the coverage floor. No -race here: `just ci` runs the race tests,
# and race plus atomic coverage pushes slow tests past their own time limits.
cover:
    #!/usr/bin/env bash
    set -euo pipefail
    ./scripts/test-isolated-home.sh go test -timeout={{test_timeout}} -coverprofile=cover.out -covermode=atomic ./...
    total=$(go tool cover -func=cover.out | awk '/^total:/ {gsub(/%/,"",$3); print $3}')
    echo "total coverage: ${total}%  (floor: {{coverage_min}}%)"
    awk -v t="$total" -v m="{{coverage_min}}" 'BEGIN { exit (t+0 >= m+0) ? 0 : 1 }' \
        || { echo "FAIL: coverage ${total}% is below the {{coverage_min}}% floor"; exit 1; }

# ── aggregates ───────────────────────────────────────────────────────────

# Exactly what GitHub CI enforces, in the same order
ci: fmt-check vet lint tidy-check scaffolding-check spec-check test-race docs-build

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
    @printf '%s\n' "$BUILD_VERSION"

# Cut a release (major|minor|patch): gates, changelog, commit, tag. Never pushes.
[positional-arguments]
release part='patch':
    #!/usr/bin/env bash
    set -euo pipefail
    just ci
    ./scripts/release.sh "$1"

# Show what a release would do, changing nothing
[positional-arguments]
release-dry part='patch':
    @./scripts/release.sh "$1" --dry-run

# Print one version's changelog section (e.g. just release-notes 0.1.0)
[positional-arguments]
release-notes version:
    #!/usr/bin/env bash
    set -euo pipefail
    # release.yml passes the pushed tag's version here. Only X.Y.Z gets
    # through, and awk compares it as text, never as a pattern.
    if ! [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        printf 'error: the version must be X.Y.Z, such as 0.1.0 (got %q)\n' "$1" >&2
        exit 2
    fi
    awk -v v="$1" 'index($0, "## [" v "]") == 1 {f=1; next} /^## \[/ {f=0} f' CHANGELOG.md

# ── product ──────────────────────────────────────────────────────────────

# Start the API server with the viewer; each argument reaches rx serve as one word
[positional-arguments]
serve *args:
    go run ./cmd/rx serve "$@"

# List the anomaly detectors this build registers
detectors:
    @go run ./cmd/rx index --help | sed -n '/analyze/,$p'

# Diff CLI output against rx-python for the same arguments, each passed as one word
[positional-arguments]
parity *args:
    #!/usr/bin/env bash
    set -euo pipefail
    go run ./cmd/rx "$@" --json > /tmp/rx-go.json
    ( cd ../rx-python && uv run rx "$@" --json ) > /tmp/rx-python.json
    diff /tmp/rx-go.json /tmp/rx-python.json && echo "identical"
