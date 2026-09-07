#!/usr/bin/env bash
# Vendor the console SPA build into api/server/ui/dist, where go:embed
# picks it up — one binary then serves both the API and the UI
# (`argon console`, or the standalone api server).
#
# The SPA source lives in the argon-cloud repo (web/). Point at a local
# checkout with $1 or ARGON_CONSOLE_SRC; a placeholder page ships in the
# repo for builds without the UI.
set -euo pipefail

SRC="${1:-${ARGON_CONSOLE_SRC:-$HOME/dev/argon-cloud/web}}"
DEST="$(cd "$(dirname "$0")/.." && pwd)/api/server/ui/dist"

if [ ! -f "$SRC/package.json" ]; then
    echo "no console SPA source at $SRC (clone argon-cloud or set ARGON_CONSOLE_SRC)" >&2
    exit 1
fi

(cd "$SRC" && npm ci --no-audit --no-fund && npm run build)
if [ ! -f "$SRC/dist/index.html" ]; then
    echo "build produced no dist/index.html" >&2
    exit 1
fi

rm -rf "$DEST"
mkdir -p "$DEST"
cp -R "$SRC/dist/." "$DEST/"
if sha="$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null)"; then
    source_tree="$(git -C "$SRC" diff HEAD -- . ':!dist' | shasum -a 256 | awk '{print $1}')"
    if [ -n "$(git -C "$SRC" status --porcelain -- . ':!dist')" ]; then sha="$sha+working-tree"; fi
else
    sha="unknown"
    source_tree="unknown"
fi
printf '%s\nsource-diff-sha256=%s\n' "$sha" "$source_tree" >"$DEST/.source"
echo "vendored console UI from $SRC ($sha) into $DEST"
