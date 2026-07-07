# Quick start

Install → first merged branch, step by step.

## 1 · Install + MongoDB

```bash
brew install argon-lab/tap/argonctl      # or: npm install -g argonctl

# MongoDB must be a replica set (change streams); one node is fine:
docker run -d --name argon-mongo -p 27017:27017 mongo:7 --replSet rs0
docker exec argon-mongo mongosh --quiet --eval 'rs.initiate()'
```

Argon connects to `MONGODB_URI` (default `mongodb://localhost:27017`) and
keeps its metadata in the `argon_wal` database.

## 2 · Bring your data in

```bash
argon import database --uri mongodb://localhost:27017 --database myapp --project myapp
# starting fresh instead: argon projects create myapp
```

Every document becomes versioned history on `main`; a snapshot is taken
automatically so reads never replay the whole import.

## 3 · Branch and work with any driver

```bash
argon branches create experiment -p myapp   # instant — a pointer, no copy
argon checkout -p myapp -b experiment       # → prints a MongoDB URI
argon watch    -p myapp -b experiment       # captures writes (keep running)
```

Point pymongo, mongoose, mongosh — anything — at the URI and work
normally: real indexes, aggregation, transactions. Every write becomes
history, attributed to its actor.

Want stable URIs instead of per-checkout ones? `argon proxy` serves
`mongodb://host:27018/<project>~<branch>?directConnection=true`.
Prefer a UI? `argon console` opens a local web console.

## 4 · Review and merge — a data pull request

```bash
argon diff          -p myapp -b experiment   # what would change
argon merge preview -p myapp -b experiment   # persist a reviewable plan
argon merge apply <plan-id>                  # exactly-once; stale heads refused
```

Conflicts fail loudly; resolve with `--strategy theirs|ours` or fix the
data and re-preview.

## 5 · Time travel, undo, rewind

```bash
argon time-travel query -p myapp -b main --lsn 1000

argon undo -p myapp -b main --from-lsn 990 --dry-run          # revert a range
argon undo -p myapp -b main --from-lsn 990 --actor agent-7    # …or one writer

argon restore preview -p myapp -b main --time 2026-07-07T09:00:00Z
argon restore reset   -p myapp -b main --time 2026-07-07T09:00:00Z --backup pre-incident
```

Resets are recorded, not destructive: discarded entries stay for audit,
and the backup branch (or any pin) keeps the old state readable.

## 6 · For agents

```bash
claude mcp add argon -- argon mcp           # sandboxes/diff/merge/undo/pins as MCP tools
argon sandbox create -p myapp --ttl 1h      # disposable branch + URI, one step
argon pin create  -p myapp --name eval-v1   # immutable dataset state
argon pin sandbox -p myapp --name eval-v1   # identical input, every eval run
```

Full agent workflow (MCP, REST, Python): [AGENTS.md](AGENTS.md).

## Where next

[CLI.md](CLI.md) — every command ·
[ARCHITECTURE.md](ARCHITECTURE.md) — guarantees, honestly ·
[OPERATIONS.md](OPERATIONS.md) — S3 snapshots, GC, migration
