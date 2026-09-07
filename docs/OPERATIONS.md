# Operations

Running Argon: requirements, configuration, storage, retention, and
migration. Everything here reflects what the code does today.

## Requirements

- **MongoDB 6+ as a writable replica set.** Write capture uses change streams with
  pre-images, which MongoDB serves only on replica sets. A single-node
  replica set is fine:

  ```bash
  docker run -d --name argon-mongo -p 127.0.0.1:27017:27017 mongo:7 --replSet rs0
  docker exec argon-mongo mongosh --quiet --eval \
    'rs.initiate({_id:"rs0", members:[{_id:0, host:"localhost:27017"}]})'
  ```

  MongoDB 6 is the feature minimum, not a production version recommendation.
  Use a current supported MongoDB patch release; the `mongo:7` example follows
  the latest available 7.x patch.

- **Source builds require Go 1.26.6 or newer.** Root, CLI and API modules declare
  the same minimum toolchain. CI scans all three modules with official
  `govulncheck` against the current vulnerability database and fails on reachable
  vulnerabilities. Reproduce it with
  `go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...` from each module.
  A clean reachable-symbol scan does not mean every unused transitive package
  is vulnerability-free; keep dependency and Go security patches current.

- **Connection.** Every Argon process (CLI, API server, MCP server, proxy)
  reads `MONGODB_URI` (default `mongodb://localhost:27017`) and keeps its
  metadata in `ARGON_METADATA_DB` (default `argon_wal`): the log (`wal_log`), branches,
  projects, LSN counters, snapshot manifests, merge plans, pins. Checked-out
  branches get physical databases named `argon_br_<branch-id>` on the same
  deployment.

## Processes

| Process | Run | Purpose |
|---|---|---|
| `argon watch -p P -b B` | one per checked-out branch you write to | captures direct writes into the WAL (resume tokens: it recovers writes made while it was down) |
| `(cd api && go run .)` (or the built binary) | one | REST control plane; supervises ingesters for the sandboxes it creates; `HOST`/`PORT` (default 127.0.0.1:8080) |
| `argon mcp` | per agent client | MCP server over stdio; supervises ingesters for its sandboxes |
| `argon proxy --listen :27018` | optional | stable `project~branch` connection strings |
| `argon sandbox sweep -p P` | schedule for CLI-only use | reap expired sandboxes (pinned ones are skipped loudly) |
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

## Preflight and new collections

Run `argon doctor` before onboarding a deployment. It performs actual temporary
writes, a transaction across physical/metadata databases and an exact-image
change-stream probe; it removes its own probe collections afterward. Status is
read-only and reports the last persisted worker status. Neither command prints
the connection URI, which can contain credentials. Failed readiness returns a
nonzero exit code, including with `--output json`.

For an empty checkout, prepare collections before application writes:

```bash
argon collections prepare orders -p myapp -b experiment
argon watch -p myapp -b experiment --actor agent:run-42
```

Driver alternative: create the collection with
`changeStreamPreAndPostImages: {enabled: true}`. Updates made before enabling
images cannot be reconstructed exactly. A watcher retries temporary index-build
or topology errors; a missing post-image, unsupported drop/rename or expired
resume token marks capture degraded. Stop writers, inspect the reported cause
and preserve the physical data. Rebuild/import into a new branch from a trusted
state when a history gap cannot be repaired; do not clear a token to pretend
that missing history was captured.

## Managed processes and lifecycle

`argon console` and API processes supervise live capture and sweep expired
sandboxes every minute. MCP does the same for its managed branches while the
process runs. Bare CLI checkout/sandbox commands do not create a background
worker: keep `watch` running and schedule `sandbox sweep` explicitly. Automatic
snapshots are triggered by native capture and programmatic writers; retention
GC still runs through its explicit command/service.

Stop native writers before release/reset. Release waits through a durable
capture barrier; reset requires a released branch. Discard intentionally drops
physical data and history, including degraded sandboxes, after pin/child checks.
Use a single owner for destructive lifecycle operations; cross-process first
checkout is not protected by a distributed lifecycle lease.

## Snapshot/GC lock recovery

Snapshot publication and chunk garbage collection share persistent locks in
`wal_snapshot_locks`. These locks deliberately do not expire: a paused worker
must not resume uploading/publishing into chunks a second worker has deleted.
A crashed worker may therefore leave a lock behind. Stop all snapshot and GC
workers for that metadata database, verify that no former owner can resume,
inspect the lock record and remove only the confirmed abandoned lock. Restart
workers after that review. Do not attach a TTL index or automatically delete
old-looking locks; age alone cannot prove a worker is dead.

## Exposure and credentials

Console/API bind loopback by default. A non-loopback listener requires
`ARGON_API_TOKEN` unless the restricted demo mode is selected. CORS allows only
same-origin requests by default; configure trusted origins explicitly. The
hosted demo disables native checkout/sandbox credentials and is not a native
driver playground. Read-only mode is available for an inspection server.

Native URIs use the engine's MongoDB credentials. They are suitable only for
trusted clients sharing those database permissions. For mutually untrusted
agents, provision restricted users or separate deployments; API bearer auth
does not restrict a returned MongoDB credential. Changing the URI's database
preserves its original SCRAM authentication database via `authSource`.

Capture status may include `last_captured_at`, `last_event_at`, `head_lsn` and
`last_event_lag_ms`. Lag is the last event wall-clock-to-checkpoint sample, not
a continuously measured queue lag or latency guarantee.
