#!/usr/bin/env bash
# Writes to <file> the notes of a version for the release job of ci.yml: its section in CHANGELOG.md, what follows
# its `## vX.Y.Z` title up to the next one. Fails when there is none.
#
#   .github/scripts/release-notes.sh <version> <file>
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
version="$1"
notes="$2"

awk -v version="$version" '/^## / { found = ($2 == version); next } found' "$root/CHANGELOG.md" >"$notes"
if ! grep -q '[^[:space:]]' "$notes"; then
  echo "::error file=CHANGELOG.md::No section for $version (expected a '## $version' title followed by what the version changes)"
  exit 1
fi
