# Quick start

From install to your first merged branch in about ten minutes.

## 1. Install

```bash
brew install argon-lab/tap/argonctl     # macOS
npm install -g argonctl                  # cross-platform

# or from source
git clone https://github.com/argon-lab/argon
cd argon/cli && go build -o argon .
```

## 2. MongoDB (replica set required)

Argon captures writes through change streams, which MongoDB only serves on
replica sets. A single-node replica set is fine for development:

```bash
docker run -d --name argon-mongo -p 27017:27017 mongo:7 --replSet rs0
docker exec argon-mongo mongosh --quiet --eval \
  'rs.initiate({_id:"rs0", members:[{_id:0, host:"localhost:27017"}]})'
```

Argon connects to `MONGODB_URI` (default `mongodb://localhost:27017`) and
keeps its metadata in the `argon_wal` database.

## 3. Import an existing database ("git clone")

```bash
argon import preview  --uri mongodb://localhost:27017 --database myapp
argon import database --uri mongodb://localhost:27017 --database myapp --project myapp
```

Every document becomes versioned history on the project's `main` branch,
and a snapshot is taken automatically so reads never replay the whole
import. (Starting fresh instead: `argon projects create myapp`.)

## 4. Branch and work with any driver

```bash
argon branches create experiment -p myapp     # a metadata write — instant
argon checkout -p myapp -b experiment         # materialize into a real MongoDB db
argon watch    -p myapp -b experiment         # capture writes as history (keep running)
```

`checkout` prints a connection string. Point pymongo, mongoose, mongosh —
anything — at it and work normally: real indexes, aggregation,
transactions, all evaluated by mongod itself. While `watch` runs, every
write becomes a WAL entry attributed to its actor.

Prefer stable connection strings? `argon proxy` serves
`mongodb://<host>:27018/<project>~<branch>?directConnection=true` and
routes to the branch's physical database (see
[AGENTS.md](AGENTS.md#the-wire-proxy)).

## 5. Review and merge — a data pull request

```bash
argon diff          -p myapp -b experiment    # what would change
argon merge preview -p myapp -b experiment    # persist a reviewable plan
argon merge apply <plan-id>                   # exactly-once, refuses stale heads
```

Conflicts are never resolved silently: `apply` fails and names them;
resolve with `--strategy theirs|ours` or fix the data and re-preview.

## 6. Time travel, undo, restore

```bash
argon time-travel info  -p myapp -b main
argon time-travel query -p myapp -b main --lsn 1000

# Revert a range (or one actor's writes) with append-only compensations
argon undo -p myapp -b main --from-lsn 990 --to-lsn 1000 --dry-run

# Rewind the whole branch — preview first, keep a backup fork
argon restore preview -p myapp -b main --time 2026-07-07T09:00:00Z
argon restore reset   -p myapp -b main --time 2026-07-07T09:00:00Z --backup pre-incident
```

Resets are recorded, not destructive: discarded entries stay in the WAL
for audit, and the backup branch (or any pin) keeps the old state
readable.

## 7. For agents

```bash
claude mcp add argon -- argon mcp        # sandboxes, diff/merge, undo, pins as MCP tools
argon sandbox create -p myapp --ttl 1h   # disposable branch + connection string
argon pin create -p myapp --name eval-v1 # immutable dataset for reproducible evals
argon pin sandbox -p myapp --name eval-v1
```

The full agent workflow — sandboxes, pins, the REST control plane and the
Python adapters — is in [AGENTS.md](AGENTS.md).

## Where next

- [CLI.md](CLI.md) — every command
- [ARCHITECTURE.md](ARCHITECTURE.md) — what the engine guarantees, honestly
- [OPERATIONS.md](OPERATIONS.md) — snapshot stores (S3/filesystem), GC and
  retention, v1→v2 migration
