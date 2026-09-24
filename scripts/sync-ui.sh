#!/usr/bin/env bash
# Rebuild the public console source embedded by go:embed. --check verifies
# committed assets without changing them. An explicit source path supports
# development mirrors; no private repository is required.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHECK=false
if [ "${1:-}" = --check ]; then CHECK=true; shift; fi
SRC="${1:-${ARGON_CONSOLE_SRC:-$ROOT/web}}"
DEST="$ROOT/api/server/ui/dist"
STAGING="$(mktemp -d)"
trap 'rm -rf "$STAGING"' EXIT

if [ ! -f "$SRC/package-lock.json" ]; then
    echo "console source requires package-lock.json at $SRC" >&2
    exit 1
fi

(cd "$SRC" && npm ci --no-audit --no-fund && npm run build)
test -f "$SRC/dist/index.html"
cp -R "$SRC/dist/." "$STAGING/"
node "$ROOT/scripts/ui-provenance.mjs" "$SRC" "$STAGING"
if "$CHECK"; then
    diff -rq "$DEST" "$STAGING"
    echo "embedded console matches public source and lockfile"
else
    rm -rf "$DEST"
    mkdir -p "$DEST"
    cp -R "$STAGING/." "$DEST/"
    echo "vendored console from $SRC into $DEST"
fi
