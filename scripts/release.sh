#!/usr/bin/env bash
set -euo pipefail

# Cut a release: promote the changelog, commit, and tag.
#
# Usage: ./scripts/release.sh [major|minor|patch] [--dry-run]
# Default: patch
#
# There is no version constant to edit — the build stamps the version from
# `git describe`, so the tag is the source of truth.
#
# Never pushes and never creates a GitHub Release. It prints those commands
# for you to run.

PART="patch"
DRY_RUN=0
for arg in "$@"; do
    case "$arg" in
        major|minor|patch) PART="$arg" ;;
        --dry-run) DRY_RUN=1 ;;
        *)
            echo "Usage: $0 [major|minor|patch] [--dry-run]"
            exit 1
            ;;
    esac
done

CHANGELOG="CHANGELOG.md"

# Resolve repo root (the script may be called from any directory)
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# ── read + compute the version ───────────────────────────────────────────

CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
CURRENT="${CURRENT_TAG#v}"
if ! [[ "$CURRENT" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: latest tag '$CURRENT_TAG' is not a vX.Y.Z semver tag"
    exit 1
fi

IFS='.' read -r MAJOR MINOR PATCH <<< "$CURRENT"
case "$PART" in
    major) MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0 ;;
    minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
    patch) PATCH=$((PATCH + 1)) ;;
esac

NEW_VERSION="${MAJOR}.${MINOR}.${PATCH}"
TAG="v${NEW_VERSION}"
TODAY=$(date +%F)

# ── preflight ────────────────────────────────────────────────────────────

BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [[ "$BRANCH" != "main" ]]; then
    echo "Error: release must run from main (currently on '$BRANCH')"
    exit 1
fi

# A dirty tree blocks a real run but not a preview — you often want to see
# the plan before committing the last change. `git status --porcelain` is
# used rather than `git diff HEAD` because the latter does not see untracked
# files, and a release cut with a stray file in the tree is a release whose
# tag does not describe what was built.
if [[ -n "$(git status --porcelain)" ]]; then
    if [[ "$DRY_RUN" -eq 1 ]]; then
        echo "Warning: working tree has uncommitted changes — a real run would refuse."
        echo
    else
        echo "Error: working tree has uncommitted changes. Commit or stash first."
        exit 1
    fi
fi

if git rev-parse "$TAG" >/dev/null 2>&1; then
    echo "Error: tag $TAG already exists"
    exit 1
fi

if ! grep -q '^## \[Unreleased\]' "$CHANGELOG"; then
    echo "Error: $CHANGELOG has no '## [Unreleased]' section to promote"
    exit 1
fi

if grep -q "^## \[${NEW_VERSION}\]" "$CHANGELOG"; then
    echo "Error: $CHANGELOG already has a '## [${NEW_VERSION}]' section"
    exit 1
fi

# Refuse to release nothing. Everything between [Unreleased] and the next
# version heading must contain at least one non-blank line.
UNRELEASED_LINES=$(awk '
    /^## \[Unreleased\]/ { inside = 1; next }
    /^## \[/            { inside = 0 }
    inside && NF        { count++ }
    END                 { print count + 0 }
' "$CHANGELOG")
if [[ "$UNRELEASED_LINES" -eq 0 ]]; then
    echo "Error: [Unreleased] is empty — nothing to release"
    exit 1
fi

echo "Release plan"
echo "  version:   $CURRENT -> $NEW_VERSION  ($PART, derived from $CURRENT_TAG)"
echo "  tag:       $TAG (lightweight)"
echo "  changelog: [Unreleased] -> [$NEW_VERSION] - $TODAY  ($UNRELEASED_LINES lines)"
echo "  constants: none — the build stamps the version from git describe"
echo

if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "--dry-run: nothing changed."
    exit 0
fi

# ── apply ────────────────────────────────────────────────────────────────

# Promote the changelog: keep a fresh empty [Unreleased] at the top and put
# the released heading directly beneath it, so everything that was pending
# now sits under the new version.
awk -v ver="$NEW_VERSION" -v dt="$TODAY" '
    !promoted && /^## \[Unreleased\]/ {
        print "## [Unreleased]"
        print ""
        print "## [" ver "] - " dt
        promoted = 1
        next
    }
    { print }
' "$CHANGELOG" > "${CHANGELOG}.tmp"
mv "${CHANGELOG}.tmp" "$CHANGELOG"

git add "$CHANGELOG"
git commit -m "Record the ${NEW_VERSION} release in the changelog."
git tag "$TAG"

echo "Created commit and tag $TAG"
echo
echo "Verify what a build will stamp:"
echo "  git describe --tags        # -> $TAG"
echo "  just version               # -> $TAG"
echo
echo "Next steps — nothing has been pushed:"
echo
echo "  git push origin main"
echo "  git push origin $TAG"
echo
echo "  # The tag push runs release.yml, which builds the binaries and"
echo "  # creates the GitHub Release with their sha256 sidecars."
echo "  gh run watch"
