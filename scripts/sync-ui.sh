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

(cd "$SRC" && npm install --no-audit --no-fund && npm run build)
if [ ! -f "$SRC/dist/index.html" ]; then
    echo "build produced no dist/index.html" >&2
    exit 1
fi

rm -rf "$DEST"
mkdir -p "$DEST"
cp -R "$SRC/dist/." "$DEST/"
sha="$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo "$sha" >"$DEST/.source"
echo "vendored console UI from $SRC ($sha) into $DEST"
