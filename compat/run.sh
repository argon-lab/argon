#!/usr/bin/env bash
# Driver-compatibility harness: real, unmodified drivers (PyMongo and
# Mongoose) run workloads against checked-out Argon branches; afterwards
# compat-verify requires the WAL to reproduce the physical state exactly.
#
# Prerequisites: a MongoDB replica set on MONGODB_URI (default
# mongodb://localhost:27017), Go, Python 3 with pymongo+pytest, Node.
set -euo pipefail
cd "$(dirname "$0")/.."

STAMP=$(date +%s)
echo "=== building tools ==="
go build -o /tmp/argonctl ./cli 2>/dev/null || (cd cli && go build -o /tmp/argonctl .)
go build -o /tmp/compat-verify ./cmd/compat-verify

run_driver() {
  local name="$1" project="compat-$1-$STAMP"; shift
  echo "=== $name: project $project ==="
  /tmp/argonctl projects create "$project" > /dev/null
  /tmp/argonctl checkout -p "$project" -b main > /dev/null
  local uri
  uri=$(/tmp/argonctl connect -p "$project" -b main)
  echo "    branch uri: $uri"

  /tmp/argonctl watch -p "$project" -b main &
  local watch_pid=$!
  trap "kill $watch_pid 2>/dev/null || true" RETURN
  sleep 2  # let the change stream open

  ARGON_BRANCH_URI="$uri" "$@"

  echo "=== $name: verifying convergence ==="
  /tmp/compat-verify --project "$project" --branch main --timeout 90s
  kill "$watch_pid" 2>/dev/null || true
  wait "$watch_pid" 2>/dev/null || true
}

run_driver pymongo python3 -m pytest compat/pymongo/test_compat.py -q
(cd compat/mongoose && npm install --no-audit --no-fund > /dev/null)
run_driver mongoose node compat/mongoose/compat.test.mjs

echo "=== driver-compat: ALL GREEN ==="
