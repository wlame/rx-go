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

# Either case, with or without "#": "Finding 6", "finding #7", "stage 3".
pattern='[Ss]tage #?[0-9]|[Rr]ound #?[0-9]|R[0-9]-[A-Z][0-9]|Reviewer [0-9]|[Ff]inding #?[0-9]|user decision [0-9.]'
# Design documents, task lists and a separate prototype the product does
# not contain: "Decision 5.1", "spec §7", "(Task 4)", "Task 6:",
# "plan-mandated", "another-rx-go/internal/…", "Post-Stage-8", "per
# user-instructions", ".go-rewriter/stage-5-decisions.md".
pattern+='|another-rx-go|[Dd]ecisions? [0-9]+\.[0-9]|plan-mandated|\(Task [0-9]+\)|Task [0-9]+:|§[0-9]'
pattern+='|Stage-[0-9]|user[- ]instructions|user design|\.go-rewriter'
# Milestones: "stub for M3", "lands in M4", "At M2", "M3's builder".
# Welford's "M2" (the running sum of squared deviations in
# internal/index/linestats.go) is never preceded by these words.
pattern+="|(at|At|in|for|until|of) M[0-9]\\b|M[0-9]'s"
# Work items live in a tracker outside this repository: "(ticket 28)".
ticket='[Tt]icket [0-9]+'
pattern+="|$ticket"

if hits=$(grep -rnE "$pattern" --include='*.go' internal/ cmd/ pkg/); then
    echo "build-process citations found in comments:" >&2
    echo "$hits" >&2
    echo >&2
    echo "Describe what the code guarantees instead. See AGENTS.md." >&2
    exit 1
fi

# The changelog, the docs and the test data ship too. Only the ticket
# pattern applies to them: the others match ordinary prose ("§", "Stage").
if hits=$(grep -rnE "$ticket" CHANGELOG.md docs/ testdata/); then
    echo "ticket numbers found in shipped files:" >&2
    echo "$hits" >&2
    echo >&2
    echo "Say what changed and why; the tracker is not part of the repository." >&2
    exit 1
fi

echo "no build-process citations"
