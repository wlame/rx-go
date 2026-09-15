#!/usr/bin/env bash
# Run a test command under a throwaway HOME and fail if it wrote an rx
# cache file there.
#
# Every test package points RX_CACHE_DIR at its own temporary directory
# (internal/testutil/isolatedcache). A test that escapes it falls back to
# $XDG_CACHE_HOME/rx or $HOME/.cache/rx — on a developer's machine, the
# real cache. A result could then depend on a stale index left by an
# earlier run, and every run would leave files behind. Here both
# variables point into a fresh temporary directory, so such a leak turns
# the gate red instead of landing in the user's cache.
#
# Usage: scripts/test-isolated-home.sh go test -race -count=1 ./...
# Run from the repository root. The test recipes in the justfile call it.
set -euo pipefail

if [ "$#" -eq 0 ]; then
    echo "usage: $0 <command> [args...]" >&2
    exit 2
fi

# Go keeps its build cache, module cache and settings file under HOME by
# default. Pin them to where they are now, so a throwaway HOME neither
# rebuilds every package nor downloads every module again.
GOCACHE="$(go env GOCACHE)"
GOMODCACHE="$(go env GOMODCACHE)"
GOPATH="$(go env GOPATH)"
GOENV="$(go env GOENV)"
export GOCACHE GOMODCACHE GOPATH GOENV

scratch="$(mktemp -d "${TMPDIR:-/tmp}/rx-test-home.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
home="$scratch/home"
mkdir -p "$home/.cache"

# With RX_CACHE_DIR unset and XDG_CACHE_HOME inside the throwaway HOME,
# rx's fallback cache directory is $home/.cache/rx whichever rule applies.
status=0
env -u RX_CACHE_DIR HOME="$home" XDG_CACHE_HOME="$home/.cache" "$@" || status=$?

leaked_cache="$home/.cache/rx"
if [ -d "$leaked_cache" ]; then
    leaked_files="$(cd "$leaked_cache" && find . -type f | sort)"
    if [ -n "$leaked_files" ]; then
        echo "FAIL: the tests wrote rx cache files outside their own RX_CACHE_DIR:" >&2
        echo "$leaked_files" >&2
        echo "Give the test package a TestMain that calls isolatedcache.Main." >&2
        exit 1
    fi
fi
exit "$status"
