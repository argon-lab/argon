# Argon Architecture

## Overview

Argon is a Git-like version control layer for MongoDB: branch, time-travel,
merge and restore data through reviewable changes. It is built on a
write-ahead log (WAL) whose replay is **deterministic by construction**, with
branches implemented as pointers into that log.

This document describes the system as implemented today. Where a capability
is planned but not built, it says so explicitly.

## Design principles

1. **Determinism first.** Replaying the same WAL prefix any number of times
   yields the same state. Every consumer (time travel, branching, restore,
   diff) is built on this property, so it is enforced structurally, not by
   convention.
2. **Physical log, not logical log.** The WAL records outcomes (full
   document images), never expressions (filters / update operators).
   Expressions are resolved exactly once, at write time.
3. **Branches are pointers.** Creating a branch writes one metadata document
   — O(1), no data copying. State is reconstructed on demand by replaying
   the branch's ancestry chain.
4. **Ordering, never density.** LSN sequences may contain gaps (failed
   writes, migrated no-ops). Consumers rely only on order.

## System components

```
┌────────────────────────────────────────────────────────────┐
│  Clients: CLI (argonctl) · Go SDK · Python SDK · REST API  │
└──────────────────────────────┬─────────────────────────────┘
                               │
┌──────────────────────────────▼─────────────────────────────┐
│  Core services (Go, internal/)                             │
│  ├─ wal.Service          append/query the log              │
│  ├─ wal.Sequencer        distributed LSN allocation        │
│  ├─ branch.Service       branch metadata & ancestry        │
│  ├─ materializer         deterministic replay              │
│  ├─ timetravel           historical state queries          │
│  ├─ restore              reset / branch-from-history       │
│  ├─ driver (interceptor) write-time filter resolution      │
│  ├─ snapshot             image layer: bounded replay depth │
│  ├─ importer             bulk import into put entries      │
│  └─ migrate              v1 → v2 WAL migration             │
└──────────────────────────────┬─────────────────────────────┘
                               │
┌──────────────────────────────▼─────────────────────────────┐
│  MongoDB (storage)                                         │
│  wal_log · wal_branches · wal_projects · wal_counters      │
└────────────────────────────────────────────────────────────┘
```

## The WAL

### Entry format (schema v2)

Every data entry is a self-contained physical record about exactly one
document:

| Field | Meaning |
|---|---|
| `lsn` | Position in the project's sequence (unique per project) |
| `project_id`, `branch_id` | Ownership; branch IDs everywhere, including control entries |
| `operation` | `put` \| `delete` \| control ops (`create_branch`, ...) |
| `collection`, `document_id` | The document the entry is about |
| `post` | Compressed full post-image (required on every put) |
| `pre` | Compressed pre-image (puts over existing docs, deletes) — powers diff, undo, audit |
| `txn_id` | Capture/merge transaction grouping; published heads cover complete batches |
| `actor` | Explicit programmatic writer or persisted native branch label |
| `v` | Schema version (2) |

Replay is a pure fold: `put` ⇒ `state[document_id] = post`, `delete` ⇒
`delete(state, document_id)`. No filter or update operator is ever
re-executed during replay — that is what makes materialization
deterministic. `Append` validates these invariants at the write boundary.

Post/pre-images are compressed per entry (zstd by default, with gzip/snappy
variants and a "don't compress if it doesn't help" floor).

### LSN allocation

LSNs are reserved through an atomic `findOneAndUpdate($inc)` on a per-project
counter document (`wal_counters`), so any number of Argon processes can write
concurrently without collisions. Failed writes leave gaps; reservations are
never rolled back (a rollback under concurrency could hand a used LSN to a
later writer). Batches reserve one contiguous range and must be
single-project.

## Branches and ancestry

A branch is `(id, parent_id, base_lsn, head_lsn, discarded_ranges)`:

- `base_lsn` — the fork point: the parent-chain LSN this branch started from.
- `head_lsn` — the newest entry belonging to this branch. Writers advance it
  with transactional compare-and-set against the observed head; restore may lower it.
- A branch's own entries live in `(base_lsn, head_lsn]`. Everything at or
  below `base_lsn` is inherited from the ancestry chain via `parent_id`.

**Materialization** walks the chain root-first: each ancestor contributes
only its own entries up to the next fork point. Sibling branches never
appear in each other's chains — isolation falls out of the traversal.
Deleted parents still anchor their descendants' history (branch metadata is
soft-deleted).

### Reset and discarded ranges

`argon restore reset --lsn T` records the abandoned window `[T+1, old_head]` in the
branch's `discarded_ranges` and lowers the head. The entries stay in the WAL
for audit; materialization skips them.

The skip rule is `segment upper bound > range end`: because the sequencer
never rewinds, any post-reset write or fork lands strictly above the
discarded window, while a branch forked *before* the reset has its fork
point at or below it. Pre-reset forks therefore keep the history they
legitimately captured, and post-reset readers never resurrect it.

## Snapshots (the image layer)

A snapshot captures the fully materialized state of one collection on one
branch at one LSN — including everything inherited through the ancestry
chain. Materialization then starts from the nearest usable snapshot and
replays only the delta above it, which bounds read cost regardless of how
long the branch's history grows. Reads below a snapshot, and branches
without one, fall back to full replay unchanged.

- **Storage**: content-addressed chunks (hex SHA-256 of the compressed
  bytes, ~4MB pre-compression, zstd); manifests always in `wal_snapshots`.
  Identical content stores once, so snapshots of slowly-changing
  collections share almost all their chunks. Documents serialize in
  canonical sorted-key form to keep the bytes deterministic.
- **Chunk store backends** (`ARGON_SNAPSHOT_STORE`):
  - `mongodb` (default) — chunks in `wal_snapshot_chunks`; zero
    configuration, works everywhere.
  - `s3` — recommended for cloud. `ARGON_S3_BUCKET` (required),
    `ARGON_S3_PREFIX`, and `ARGON_S3_ENDPOINT` for S3-compatible stores
    (MinIO, R2, Ceph; switches to path-style addressing). Credentials and
    region resolve through the standard AWS chain. Uploads of
    already-present content are skipped (HeadObject on the content
    address).
  - `filesystem` — `ARGON_SNAPSHOT_DIR`; sharded directories, atomic
    write-then-rename. For self-hosted deployments without an object
    store.
  All three pass the same contract and end-to-end snapshot tests (S3 runs
  against MinIO in CI); GC reclaims orphaned chunks through the same
  interface, so no backend leaks storage.
- **Lookup**: materialization searches the leaf-most ancestry hop first — a
  leaf snapshot covers the entire inherited chain beneath it. Within a hop,
  the newest usable snapshot inside the segment's LSN window wins.
- **Reset interaction**: reset never touches snapshots. Each manifest
  records how many discarded ranges existed when it was built
  (`RangesApplied`); ranges are append-only, so a reader detects
  invalidation by checking only ranges recorded afterwards, with the same
  visibility rule replay applies to entries. Post-reset snapshots are
  automatically valid again.
- **Incremental**: `CreateSnapshot` materializes through the snapshot-aware
  path itself, so each snapshot builds from the previous one plus the delta.
- **Automatic**: both programmatic writes and committed native capture batches notify the snapshot service;
  once a branch head advances a threshold past its newest snapshot (default
  1000 LSNs, checked at most every 64 writes per branch), a snapshot is
  taken off the write path. `argon snapshot create/list` does it manually.
- **Reclamation**: deleting a branch (which requires it to have no
  children) drops its WAL entries, its manifests and any chunks no other
  manifest references.

## Agent sandboxes and the MCP server

A sandbox is a branch forked from a parent, checked out into its own
physical database and stamped with a TTL (`argon sandbox create -p proj
--ttl 1h`): hand an agent the connection string, merge what you like,
undo what you don't, and let `argon sandbox sweep` reclaim whatever is
left when the TTL passes (deletion reclaims WAL entries, snapshots and
chunks). `keep` removes the TTL; `extend` pushes it out.

`argon mcp` serves this workflow to AI agents over the Model Context
Protocol (stdio): `argon_sandbox_create` returns a connection string,
`argon_diff` / `argon_merge_preview` / `argon_merge_apply` bring the work
back, `argon_undo` reverts it, `argon_sandbox_discard` throws it away.
The MCP server supervises a change-stream ingester for every sandbox it
hands out, so agent writes become versioned history without anyone
running `argon watch` by hand. Register with an MCP client, e.g.
`claude mcp add argon -- argon mcp`.

### Dataset pins (reproducible evals)

A pin is a named, immutable reference to a branch state — Argon's tag,
with one addition Git doesn't need: pinned history survives garbage
collection and resets forever. `argon pin create -p proj --name eval-v1`
pins the current head (or `--lsn` / `--time`); `argon pin sandbox` forks
a TTL sandbox that starts at exactly the pinned state; `argon pin branch`
makes a durable branch instead. Pin an eval dataset once, fork a fresh
sandbox from the pin for every run, and the input state is identical no
matter what happened to the branch since — resets included, because
discarded ranges only apply to readers whose bound lies beyond them, the
same rule that keeps pre-reset backup branches intact. Deleting a branch
with pins is refused (like deleting a branch with live children); deleting
the pin releases its history to the next GC run. The MCP server exposes
`argon_pin_create` / `argon_pin_list` / `argon_pin_sandbox`, the REST API
`/projects/:p/pins`.

## Garbage collection (retention window)

`argon gc` (and `Services.RunGC`) deletes WAL entries that no reader can
ever need again. Per (branch, collection), the reclaim cutoff is

```
min( S,  R,  min over live children of S_i )
```

where `S` is the newest snapshot valid for every possible future reader
(no later discarded range overlaps it), `R` is the newest LSN older than
the retention window (default 7 days), and `S_i` is the newest snapshot at
or below each live child's fork point — children and all their descendants
read the parent's segment with an upper bound pinned to the fork, so they
can only be served by snapshots at or below it. Dataset pins enter the
same minimum as permanent readers at their LSN: a pin at `P` clamps the
cutoff to the newest snapshot usable at bound `P`, and to zero — nothing
reclaimed — while no such snapshot exists. Entries at or below the
cutoff are deleted; control entries stay.

Two consequences worth stating plainly: history without snapshot coverage
is **never** deleted, no matter how old — and reclaiming entries ends
time-travel, audit and undo below the cutoff, which is exactly what a
retention window means.

Snapshots and GC bound replay and reclaim eligible history. Total storage
still grows with active branch states, permanent pins and write volume inside
the retention window; retention is not a fixed disk-space quota.

## Write paths

There are exactly two ways state enters the WAL, and neither reimplements
MongoDB:

1. **Checked-out branches (applications)** — writes go to the physical
   database through any MongoDB driver; the change stream feeds the WAL
   (see the ingest package). mongod evaluates every filter, update
   operator, index and pipeline.
2. **Programmatic writes (`walwriter`)** — imports, merges, undos, seeding
   and tests append explicit document states: `Put(collection, document)`
   and `Delete(collection, id)`. Pre-images are captured automatically via
   point lookups, puts batch into contiguous LSN ranges, and writes to
   live branches are rejected (their WAL is fed by the change stream).

The in-process Mongo emulation that once backed an SDK write path — filter
matching, update-operator application, a mongo-like Collection surface —
is gone. Expression evaluation survives only as a migration artifact
(`internal/mongoexpr`): the v1→v2 WAL migration must resolve legacy
expression entries one final time, and canonical BSON
comparison/serialization lives there for snapshots and diffs.

### The wire-protocol proxy (`argon proxy`)

Checked-out branches have machine-generated physical database names
(`argon_br_<id>`). The proxy (`internal/wireproxy`) gives them stable,
human-readable connection strings instead:

```
mongodb://proxy-host:27018/<project>~<branch>?directConnection=true
```

It is a TCP proxy that parses OP_MSG frames from the client, rewrites the
`$db` field of commands addressed to a branch alias (`project~branch`) to
the branch's physical database, and forwards everything else byte-for-byte
— responses are a raw copy in the other direction. mongod still evaluates
every query; the proxy only routes.

Honest constraints: compression is negotiated away (the proxy blanks the
handshake's `compression` field so OP_COMPRESSED never appears);
`directConnection=true` is required (topology discovery would hand clients
the upstream's own address); with auth, `authSource=admin` must be
explicit because the URI database is the alias. Aliases that don't resolve
(unknown project/branch, or a branch that isn't checked out) get a
synthesized `{ok: 0}` command error naming the proxy, not a dropped
connection. Capture stays asynchronous through the change-stream ingester
— the proxy is deliberately only the routing layer. It is the natural
place to one day close the ingest-lag window (see the roadmap), but not
by parsing writes: the change stream already hands the ingester
before/after images, transaction resolution and mongod's commit order for
free, and re-deriving those in the proxy would mean reimplementing
MongoDB write semantics — the one thing this design refuses to do.

## Migration from schema v1

v1 logged updates/deletes as expressions and re-executed them on replay —
which was not deterministic (Go map iteration order). The materializer
refuses v1 data entries with an error; `argon migrate-wal --project <name>`
rewrites them in place (parents before children, LSNs preserved, no-op
entries removed). Migration is idempotent.

## Consistency model

- **Deterministic replay:** full BSON outcomes are logged. Typed IDs remain
  distinct, including ObjectID versus a same-looking string and ordered
  composite IDs. Legacy image-backed IDs are normalized when read.
- **Atomic publication:** metadata writer batches, merge data/audit/plan state,
  capture WAL/head/resume token and reset metadata are transactionally
  published. Concurrent writers compare the observed branch head/incarnation.
- **Live conflict checks:** physical merge/undo compare each current document
  with the previewed before-image inside the write transaction. A stale
  document aborts the entire batch even if capture has not advanced the head.
- **Asynchronous native capture:** completed driver writes initially exist in
  mongod. Managed API/MCP URI creation waits for readiness and a durable barrier;
  versioned CLI/API/MCP operations drain completed native writes before reading.
  Direct Go service consumers must call `Services.SyncBranch` when they need
  that synchronization. The wire proxy does not delay native acknowledgements.
- **Recovery:** checkout persists the first stream boundary, later capture
  commits resume tokens with WAL batches, and transient failures retry. This
  requires MongoDB to retain the oplog/change-stream history and exact images.
  Expired history or missing post-images becomes degraded capture, not success.
- **Transaction bounds:** native transaction events are published as complete
  batches at the branch head, including groups larger than a server cursor batch.
  An empty cursor batch alone does not prove a transaction ended: capture waits
  for a subsequent database marker or a different transaction. Cancellation
  leaves an unproven trailing group behind the saved resume boundary for replay.
  An arbitrary historical LSN inside a transaction
  is still a document-history prefix, not a transaction-consistent snapshot.
- **Undo:** compensations contain their own before-images. Missing required
  images and any later unselected document writes are reported and skipped.
  Actor filtering uses recorded labels; MongoDB change streams do not identify
  each application or user. A native branch keeps one persisted actor label.

## Capture and lifecycle prerequisites

Use MongoDB 6+ as a writable replica set; the regression environment is MongoDB
7.0.14. `argon doctor` checks real image/transaction permissions. Before the
first write to every new native collection, run
`argon collections prepare <collection> -p <project> -b <branch>` or create it
through the MongoDB driver with `changeStreamPreAndPostImages: {enabled:true}`.
Auto-enabling after a collection appears cannot recover images for updates
that already happened. Checkout prepares materialized collections itself.

Document insert/update/replace/delete operations are versioned. Collection
rename/drop/database-drop is unsupported and produces persisted `degraded`
health. Indexes, validators and collection options are executed by mongod but
are not versioned or reproduced by a later checkout. Missing delete pre-images
make undo unrecoverable for those documents. Observe the capture health rather
than assuming a running process implies complete history.

Repeated checkout returns an existing live database without rebuilding it.
Before release, stop application writers; release drains through a durable
marker before dropping the physical database. Writes after that marker are
outside the guarantee. Explicit discard intentionally removes the branch and
its history, after pin/child preflight checks. Coordinate destructive lifecycle
operations through one control-plane owner: checkout serialization is local
to a Service, not a distributed lease for simultaneous first checkout across
processes. Native URI credentials are deployment credentials, not per-sandbox
security isolation.

## Known limitations and roadmap

Current limitations (deliberate scope):

- Time-travel and metadata-only branch reads materialize in memory (no
  indexes or aggregation on that path); live branches checked out to a
  physical database get real mongod reads with everything that implies.
- WAL entries live in MongoDB and are reclaimed by retention-window GC once
  snapshots cover them; snapshot chunks can additionally live in an
  S3-compatible or filesystem chunk store (see "Chunk store backends"
  above). GCS is not yet a backend.
- Published benchmarks still need native capture overhead, end-to-end sandbox
  readiness and divergence storage growth. The public `WriterFor` write API
  exists; the old public-writer issue is no longer a blocker.

Performance characteristics are measured by the public benchmark suite at
https://github.com/argon-lab/benchmarks — reproducible with
`docker compose up`, results recorded with pinned engine refs.

Recently closed from this list: driver compatibility is now exercised in
CI on every push (pymongo and mongoose harnesses writing through a live
ingester, plus WAL-convergence verification), and the LangGraph
checkpointer / Mem0 factory ship as the `argon-agents` Python package on
top of the REST API.

Planned next:

- **GCS chunk-store backend** — a third cloud object store alongside
  S3-compatible and filesystem.
- **A read-your-writes barrier in the wire proxy** — an opt-in mode where
  the proxy holds a write's acknowledgement until the change-stream
  ingester confirms the entry has landed in the WAL, giving callers zero
  effective ingest lag (immediate `diff`/materialize after a write) and
  strict "no write exists that isn't logged" semantics. This is
  deliberately a *synchronization barrier*, not synchronous capture: the
  proxy still never parses writes or owns images — the ingester remains
  the single capture path — so it stays cheap and keeps mongod as the only
  query engine. It does not remove the replica-set requirement (change
  streams still do the capturing); dropping that would require true
  in-proxy capture, which we are not planning.

## Storage collections

| Collection | Contents |
|---|---|
| `wal_log` | The log itself (entries as above) |
| `wal_branches` | Branch metadata: pointers, ancestry, discarded ranges |
| `wal_projects` | Project metadata |
| `wal_counters` | Per-project LSN counters |
| `wal_snapshots` | Snapshot manifests |
| `wal_pins` | Dataset pins (named immutable branch states) |
| `wal_snapshot_chunks` | Content-addressed snapshot data |
| `wal_ingest_state` | Durable capture boundary, actor, resume token and last health report |
| `wal_snapshot_locks` | Persistent publication/GC locks; see operations recovery |

Indexes on `wal_log`: unique `(project_id, lsn)`;
`(branch_id, collection, lsn)`; `(branch_id, collection, document_id, lsn)`;
`timestamp`.
