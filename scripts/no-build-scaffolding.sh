#!/usr/bin/env bash
# Fail when a comment cites the process that produced the code.
#
# A comment explains the code as it stands, not the journey that produced
# it. The plan documents these citations point at do not exist in any
# repository, so a reader six months from now has nowhere to look — and
# "See Stage 8 Reviewer 2 High #6." reads as if there were somewhere.
#
# Run from the repository root. `just ci` calls it.
set -euo pipefail

pattern='Stage [0-9]|Round [0-9]|R[0-9]-[A-Z][0-9]|Reviewer [0-9]|Finding [0-9]|user decision [0-9.]'

if hits=$(grep -rnE "$pattern" --include='*.go' internal/ cmd/ pkg/); then
    echo "build-process citations found in comments:" >&2
    echo "$hits" >&2
    echo >&2
    echo "Describe what the code guarantees instead. See AGENTS.md." >&2
    exit 1
fi

echo "no build-process citations"
