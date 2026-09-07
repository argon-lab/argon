# Quick start

This path runs a real MongoDB replica set, checks prerequisites, and uses a
managed API/console process for write capture and sandbox expiry.

## Install and check MongoDB

```bash
npm install -g argonctl
# macOS alternative: brew install argon-lab/tap/argonctl

docker run -d --name argon-mongo -p 127.0.0.1:27017:27017 mongo:7 --replSet rs0
docker exec argon-mongo mongosh --quiet --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"localhost:27017"}]})'
export MONGODB_URI='mongodb://localhost:27017/?replicaSet=rs0'
argon doctor
```

Wait for primary election if doctor reports that the replica set is not ready.
The `mongo:7` tag follows current 7.x patches; production deployments should
use a current supported patch release and keep it updated.
`doctor` verifies MongoDB 6+, a writable replica-set primary, metadata writes,
cross-database transactions and exact change-stream images. It creates and
removes uniquely named temporary probe collections. `argon status` provides
read-only readiness checks. Both return nonzero on failure.

## Run the managed workflow

In terminal A:

```bash
argon console --no-browser
```

Open http://127.0.0.1:1818. The process supervises capture for live branches,
waits for capture before returning native URIs, and sweeps expired sandboxes
every minute. Keep it running while agents work. MCP clients can instead
launch `argon mcp`, which manages its sandbox capture and expiry too.

In terminal B, from a repository checkout:

```bash
python3 -m venv .venv
. .venv/bin/activate
python3 -m pip install pymongo
python3 examples/pinned_agents.py --api http://127.0.0.1:1818
```

The runnable example creates business accounts, pins the baseline, gives two
independent agent proposals identical databases, reviews both changes, merges
the approved 10% discount, and discards the rejected 50% proposal. Assertions
check isolation and the final database state. No LLM account is needed; the
two deterministic proposals make the data workflow reproducible. The resulting
project and pin remain available in the console for inspection.

## CLI-only workflow

CLI commands connect directly to MongoDB. A one-shot checkout/sandbox command
does not leave a background watcher or TTL worker behind.

```bash
argon projects create myapp --output json
argon branches create experiment -p myapp --output json
argon checkout -p myapp -b experiment --output json
argon collections prepare orders -p myapp -b experiment
# Keep this running in terminal A; use the printed URI from terminal B:
argon watch -p myapp -b experiment --actor agent:experiment
```

After writing through that URI, use another terminal:

```bash
argon diff -p myapp -b experiment --output json
argon merge preview -p myapp -b experiment --output json
argon merge apply <plan-id> --output json
```

Use the `plan.id` returned by preview. Versioned operations synchronize
completed native writes first; stale plans fail and require a new preview.
For bare CLI sandboxes, schedule `argon sandbox sweep -p myapp` yourself.

## History and cleanup

```bash
argon time-travel info -p myapp -b main --output json
argon time-travel query -p myapp -b main --lsn <retained-lsn> --output json
argon undo -p myapp -b experiment --from-lsn <first-change-lsn> --dry-run --output json
# Stop native writers, then drain and release before a reset:
argon release -p myapp -b experiment
argon restore preview -p myapp -b experiment --lsn <retained-lsn>
```

Read retained LSNs from `time-travel info`; sample numbers are not valid for
every project. Missing images and later changes are skipped by undo and
reported explicitly. Retention GC can remove old audit/undo history; pins
preserve named states. Drop/rename DDL is not captured and marks capture
degraded. Native capture actor labels apply to the whole branch, not each
MongoDB client. See [architecture](ARCHITECTURE.md) and
[operations](OPERATIONS.md) before production use.

## Build from source

```bash
# From the repository root:
(cd cli && go build -o ../bin/argon .)
(cd api && go run .)
```

`ARGON_METADATA_DB` changes the metadata database (default `argon_wal`), which
is useful for isolated tests. Use Go 1.26.6 or newer, as declared in `go.mod`;
CLI and API are separate Go modules.
The API defaults to 127.0.0.1:8080. Set `ARGON_API_TOKEN` before exposing a
non-loopback listener, and send that bearer token from REST clients.
