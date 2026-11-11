#!/usr/bin/env bash
# Manual recovery path; normal publication is the tag-triggered release workflow.
set -euo pipefail

cd "$(dirname "$0")/.."
if ! git diff --quiet HEAD --; then
  echo 'Publish from a clean tagged checkout so the package matches its release binaries.' >&2
  exit 1
fi
release_tag=$(git tag --points-at HEAD --list 'v*')
case "$release_tag" in
  v*)
    if [[ "$release_tag" == *$'\n'* ]]; then
      echo 'HEAD has multiple root release tags; use an unambiguous release checkout.' >&2
      exit 1
    fi
    ;;
  *) echo 'HEAD must have an exact v-prefixed release tag.' >&2; exit 1 ;;
esac
release_version=$(node scripts/release-version.js "$release_tag" --check)
node scripts/release-version.js --check
npm whoami >/dev/null
# The package must carry the digests of this release's binaries: its
# installer refuses to run a download it cannot verify.
checksums=$(mktemp -d)
trap 'rm -rf "$checksums"; rm -f npm/checksums.json' EXIT
gh release download "$release_tag" --repo argon-lab/argon --pattern SHA256SUMS --dir "$checksums"
node scripts/npm-checksums.mjs "$checksums/SHA256SUMS"
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
