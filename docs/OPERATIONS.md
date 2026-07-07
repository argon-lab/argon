# Operations

Running Argon: requirements, configuration, storage, retention, and
migration. Everything here reflects what the code does today.

## Requirements

- **MongoDB as a replica set.** Write capture uses change streams with
  pre-images, which MongoDB serves only on replica sets. A single-node
  replica set is fine:

  ```bash
  docker run -d --name argon-mongo -p 27017:27017 mongo:7 --replSet rs0
  docker exec argon-mongo mongosh --quiet --eval \
    'rs.initiate({_id:"rs0", members:[{_id:0, host:"localhost:27017"}]})'
  ```

- **Connection.** Every Argon process (CLI, API server, MCP server, proxy)
  reads `MONGODB_URI` (default `mongodb://localhost:27017`) and keeps its
  metadata in the `argon_wal` database: the log (`wal_log`), branches,
  projects, LSN counters, snapshot manifests, merge plans, pins. Checked-out
  branches get physical databases named `argon_br_<branch-id>` on the same
  deployment.

## Processes

| Process | Run | Purpose |
|---|---|---|
| `argon watch -p P -b B` | one per checked-out branch you write to | captures direct writes into the WAL (resume tokens: it recovers writes made while it was down) |
| `go run ./api` (or the built binary) | one | REST control plane; supervises ingesters for the sandboxes it creates; `PORT` (default 8080) |
| `argon mcp` | per agent client | MCP server over stdio; supervises ingesters for its sandboxes |
| `argon proxy --listen :27018` | optional | stable `project~branch` connection strings |
| `argon sandbox sweep -p P` | cron | reap expired sandboxes (pinned ones are skipped loudly) |
| `argon gc -p P` | cron | reclaim covered, out-of-retention WAL entries |

## Snapshot chunk stores

Snapshots are content-addressed, zstd-compressed chunks (~4 MB),
deduplicated across branches and snapshots. Where chunks live is chosen by
environment:

| `ARGON_SNAPSHOT_STORE` | Additional variables | Notes |
|---|---|---|
| `mongodb` (default) | — | chunks in `argon_wal.wal_snapshot_chunks`; zero setup |
| `s3` (cloud default) | `ARGON_S3_BUCKET` (required), `ARGON_S3_PREFIX` (default `argon/chunks`), `ARGON_S3_ENDPOINT` (MinIO/R2/Ceph), plus standard `AWS_*` credentials | recommended for cloud deployments |
| `filesystem` | `ARGON_SNAPSHOT_DIR` (required) | self-hosted disks |

Snapshots happen automatically (roughly every 1,000 entries per branch,
plus immediately after imports); `argon snapshot create` forces one. GCS
is not yet a backend.

## Retention and GC

`argon gc -p P --retention 168h` deletes WAL entries that are **all** of:
covered by a snapshot every future reader can use, older than the
retention window, below every live child's fork-point coverage, and below
every pin's coverage. Consequences, stated plainly:

- No snapshot → nothing is ever deleted, no matter how old.
- Reclaiming entries ends time-travel/audit/undo below the cutoff — that
  is what a retention window means; pick it accordingly (default 7 days).
- Pins punch permanent holes: a pinned state stays materializable forever
  until the pin is deleted.
- Deleting a branch reclaims its entries, snapshots and unshared chunks
  immediately (deletion is refused while the branch has live children or
  pins).

`--dry-run` reports what would be deleted, per branch and collection.

## Migrating from WAL schema v1

v1 logged updates as expressions and re-executed them on replay, which was
not deterministic. The v2 materializer refuses v1 data entries with an
error naming the fix:

```bash
argon migrate-wal --project myapp --dry-run
argon migrate-wal --project myapp
```

Migration rewrites entries in place (parents before children, LSNs
preserved), is idempotent, and needs no downtime for readers of already-
migrated branches.

## Monitoring

`argon status` reports connectivity and system health; `argon metrics`
prints performance counters (operation rates, latencies, error rates). The
services log ingester lifecycle events and snapshot/GC warnings to stderr;
`wal.Monitor` runs periodic health checks inside every long-lived process.

## Authentication

Argon passes credentials through `MONGODB_URI` untouched. With the wire
proxy, clients must set `authSource=admin` explicitly (the URI database is
a branch alias, not a real database SCRAM can run against).
