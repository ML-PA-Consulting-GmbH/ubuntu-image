#!/bin/bash

# Cut a release: pick the next tag, push it, let GitHub Actions build
# and publish the binary (.github/workflows/release.yml).

set -euo pipefail

cd "$(dirname "$0")"

DRY_RUN=0
ASSUME_YES=0
NEW_TAG=""

usage() {
    cat <<EOF
Usage: ./release.sh [options] [tag]

Without a tag, prints the latest one and proposes a patch bump.
A tag may also be given as 'patch', 'minor' or 'major'.

Options:
  --dry-run   run every check, tag and push nothing
  -y, --yes   skip the confirmation prompt
  -h, --help  this text
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        -y|--yes) ASSUME_YES=1 ;;
        -h|--help) usage; exit 0 ;;
        -*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
        *) NEW_TAG="$1" ;;
    esac
    shift
done

TAG_PATTERN='^v([0-9]+)\.([0-9]+)\.([0-9]+)(\+up(.+))?$'

BRANCH=$(git rev-parse --abbrev-ref HEAD)
UPSTREAM_VERSION=$(sed -n 's/^version: *"\?\([^"]*\)"\?/\1/p' snapcraft.yaml | head -1)
LATEST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")

# bump <tag> <part> -> the next tag, carrying snapcraft.yaml's upstream
# version so an upstream bump reaches the tag by itself.
bump() {
    local tag=$1 part=$2 major minor patch
    if [[ ! $tag =~ $TAG_PATTERN ]]; then
        echo "cannot bump $tag: it does not read as vMAJOR.MINOR.PATCH+upUPSTREAM" >&2
        return 1
    fi
    major=${BASH_REMATCH[1]}
    minor=${BASH_REMATCH[2]}
    patch=${BASH_REMATCH[3]}
    case "$part" in
        major) major=$((major + 1)); minor=0; patch=0 ;;
        minor) minor=$((minor + 1)); patch=0 ;;
        patch) patch=$((patch + 1)) ;;
    esac
    echo "v${major}.${minor}.${patch}+up${UPSTREAM_VERSION:-${BASH_REMATCH[5]}}"
}

echo "L-IoT Image Builder release"
echo
printf '  branch:    %s\n' "$BRANCH"
printf '  upstream:  %s (snapcraft.yaml)\n' "${UPSTREAM_VERSION:-unknown}"
if [ -n "$LATEST_TAG" ]; then
    printf '  last tag:  %s (%s)\n' "$LATEST_TAG" "$(git log -1 --format=%cs "$LATEST_TAG")"
else
    printf '  last tag:  none\n'
fi
echo

RANGE=${LATEST_TAG:+$LATEST_TAG..}HEAD
COMMIT_COUNT=$(git rev-list --count "$RANGE")
if [ "$COMMIT_COUNT" -eq 0 ]; then
    echo "HEAD is already tagged $LATEST_TAG; nothing to release." >&2
    exit 1
fi
echo "$COMMIT_COUNT commit(s) to release:"
git log --oneline --no-decorate "$RANGE" | sed 's/^/  /'
echo

# A dirty tree would stamp the binary -dirty, and the release would
# carry a version nobody can check out.
if [ -n "$(git status --porcelain)" ]; then
    echo "working tree has uncommitted changes; commit or stash them first:" >&2
    git status --short >&2
    exit 1
fi

PROPOSED=$(bump "${LATEST_TAG:-v0.0.0}" patch)
if [ -z "$NEW_TAG" ]; then
    if [ ! -t 0 ]; then
        echo "no tag given and no terminal to ask at; pass one as an argument" >&2
        exit 2
    fi
    read -r -p "New tag [$PROPOSED]: " NEW_TAG
fi
case "${NEW_TAG:-}" in
    "") NEW_TAG=$PROPOSED ;;
    patch|minor|major) NEW_TAG=$(bump "${LATEST_TAG:-v0.0.0}" "$NEW_TAG") ;;
esac

if [[ ! $NEW_TAG =~ $TAG_PATTERN ]]; then
    echo "\"$NEW_TAG\" does not read as vMAJOR.MINOR.PATCH+upUPSTREAM (e.g. $PROPOSED)" >&2
    exit 2
fi
if [ -z "${BASH_REMATCH[4]}" ] && [ -n "${UPSTREAM_VERSION:-}" ]; then
    echo "note: $NEW_TAG carries no +up${UPSTREAM_VERSION} suffix, unlike the recent tags"
fi
if git rev-parse -q --verify "refs/tags/$NEW_TAG" >/dev/null; then
    echo "tag $NEW_TAG exists here already" >&2
    exit 1
fi
if git ls-remote --exit-code --tags origin "$NEW_TAG" >/dev/null 2>&1; then
    echo "tag $NEW_TAG exists on origin already" >&2
    exit 1
fi

echo
echo "=> go build ./..."
go build ./...
echo "   ok (the test suite is not run: parts of it reach the real snap store)"

# A branch with no upstream has everything to push; one with an
# upstream has whatever sits beyond it.
if git rev-parse --abbrev-ref "@{upstream}" >/dev/null 2>&1; then
    UNPUSHED=$(git rev-list --count "@{upstream}..HEAD")
else
    UNPUSHED=$COMMIT_COUNT
fi
echo
echo "About to release $NEW_TAG from $BRANCH at $(git rev-parse --short HEAD):"
[ "$UNPUSHED" -gt 0 ] && echo "  - push $UNPUSHED commit(s) to origin/$BRANCH"
echo "  - tag $NEW_TAG and push it, which builds and publishes the binary"
if [ "$DRY_RUN" -eq 1 ]; then
    echo
    echo "dry run: stopping here."
    exit 0
fi
if [ "$ASSUME_YES" -eq 0 ]; then
    read -r -p "Go ahead? [y/N] " answer
    case "$answer" in
        y|Y|yes|YES) ;;
        *) echo "nothing done."; exit 1 ;;
    esac
fi

if [ "$UNPUSHED" -gt 0 ]; then
    git push origin "$BRANCH"
fi
git tag "$NEW_TAG"
git push origin "$NEW_TAG"

REPO_URL=$(git remote get-url origin | sed -e 's#^git@github.com:#https://github.com/#' -e 's#\.git$##')
echo
echo "Released $NEW_TAG"
echo "  build:   $REPO_URL/actions"
echo "  release: $REPO_URL/releases/tag/$NEW_TAG"
