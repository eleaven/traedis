#!/usr/bin/env bash
# Checks ./VERSION for the release job of ci.yml, and fails when it is not vMAJOR.MINOR.PATCH, or when it is a new
# version that does not follow the highest released one. Writes to $GITHUB_OUTPUT (stdout outside of the CI):
#   version=vX.Y.Z
#   new=true|false  (false: already released, nothing to do)
#
#   .github/scripts/version.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
output="${GITHUB_OUTPUT:-/dev/stdout}"

# vMAJOR.MINOR.PATCH, no leading zeros: what Go modules and the Traefik Plugin Catalog expect
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

version="$(<"$root/VERSION")"
if [[ ! "$version" =~ $semver ]]; then
  echo "::error file=VERSION::'$version' is not a valid version (expected vMAJOR.MINOR.PATCH, e.g. v1.2.3)"
  exit 1
fi
echo "version=$version" >>"$output"

tags="$(git -C "$root" ls-remote --tags --refs origin | sed 's|.*refs/tags/||')"
if grep -qxF "$version" <<<"$tags"; then
  echo "$version is already released: nothing to do"
  echo "new=false" >>"$output"
  exit 0
fi

# Incremental: the next patch, minor or major of the highest released version
latest="$(grep -E "$semver" <<<"$tags" | sort -V | tail -n 1 || true)"
if [[ "$latest" =~ $semver ]]; then
  major="${BASH_REMATCH[1]}"
  minor="${BASH_REMATCH[2]}"
  patch="${BASH_REMATCH[3]}"
  next_patch="v$major.$minor.$((patch + 1))"
  next_minor="v$major.$((minor + 1)).0"
  next_major="v$((major + 1)).0.0"
  if [[ "$version" != "$next_patch" && "$version" != "$next_minor" && "$version" != "$next_major" ]]; then
    echo "::error file=VERSION::$version does not follow $latest (expected $next_patch, $next_minor or $next_major)"
    exit 1
  fi
fi

echo "new=true" >>"$output"
