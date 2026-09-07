#!/usr/bin/env bash
# Manual recovery path; normal publication is the tag-triggered release workflow.
set -euo pipefail

cd "$(dirname "$0")/.."
if ! git diff --quiet HEAD --; then
  echo 'Publish from a clean tagged checkout so the package matches its release binaries.' >&2
  exit 1
fi
release_tag=$(git describe --tags --exact-match HEAD)
case "$release_tag" in
  v*) ;;
  *) echo 'HEAD must have an exact v-prefixed release tag.' >&2; exit 1 ;;
esac
release_version=$(node scripts/release-version.js "$release_tag")
node scripts/release-version.js --check
npm whoami >/dev/null
(
  cd npm
  npm pack --dry-run
)
printf 'Publish argonctl@%s? [y/N] ' "$release_version"
read -r reply
case "$reply" in
  y|Y|yes|YES)
    dist_tag=latest
    if [[ "$release_version" == *-* ]]; then dist_tag=next; fi
    (cd npm && npm publish --access public --tag "$dist_tag")
    ;;
  *) echo 'Publication cancelled.' ;;
esac
