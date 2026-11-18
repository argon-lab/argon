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
│  ├─ ingest               change streams → entries: capture │
│  │                       of checked-out branches, and      │
│  │                       following an external database    │
│  ├─ source               which database a branch follows   │
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

`argon restore reset --lsn T` records the abandoned window `[T+1, M]` in the
branch's `discarded_ranges` and lowers the head. `M` is the branch's
high-water mark: the highest LSN its history has ever reached. The entries
stay in the WAL for audit; materialization skips them.

The skip rule is `segment upper bound > range end`: because the sequencer
never rewinds, any post-reset write or fork lands strictly above `M`, while
every view captured *before* the reset — a fork, a `--backup` branch, a pin —
sits at or below it. Those views therefore keep all the history they
legitimately captured, and post-reset readers never resurrect it.

Three details make that rule hold in every sequence of operations:

- **The window ends at the high-water mark, not at the head.** After two
  resets in a row the head is already below the first window. Ending the
  second window at that head would put views captured inside the first
  window "after" the second reset, and they would lose the history between
  the two targets. Releases up to 2.1.5 recorded the head; such ranges are
  read with their corrected end (`Branch.EffectiveDiscardedRanges`), so
  existing data needs no migration.
- **A reset cannot target discarded history.** An LSN inside an earlier
  window is not on the branch's timeline: as a head it would show the
  discarded writes until the next write hid them again. The reset is
  refused, and the error names the nearest valid target.
- **Discarded history stays addressable up to `M`.** It can be read
  (`time-travel query --lsn`) and forked (`restore branch`), immediately
  after the reset and later. Forking is how discarded work is recovered. A
  `--time` selector resolves to where the branch stood at that moment, so
  it can land in a discarded window: `restore branch --time` then recovers
  that state, while `restore reset --time` is refused as above.

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
- **Reset interaction**: reset never touches snapshots. A snapshot at LSN
  `S` is itself a read with upper bound `S`, so it left out a discarded
  range only if the range was already recorded when it was built (the
  manifest stores that count as `RangesApplied`) and ended below `S`. A
  reader may start from a snapshot unless some range overlaps the
  snapshot's history, was not left out by the build, and must be skipped by
  that reader. This covers a reset that happens after the snapshot was
  built and a snapshot taken at an LSN inside an already discarded window;
  both keep serving the forks, pins and historical reads that retain the
  pre-reset view. Post-reset snapshots are valid for everyone again.
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
whose referenced captured history is protected from garbage collection and
resets while the pin exists. `argon pin create -p proj --name eval-v1`
pins the current head (or `--lsn` / `--time`); `argon pin sandbox` forks
a TTL sandbox that starts at exactly the pinned state; `argon pin branch`
makes a durable branch instead. Pin an eval dataset once, fork a fresh
sandbox from the pin for every run, and the input state is identical no
matter what happened to the branch since — resets included, because
discarded ranges only apply to readers whose bound lies beyond them, the
same rule that keeps pre-reset backup branches intact. Deleting a branch
with pins is refused (like deleting a branch with live children); deleting
the pin releases its history to the next GC run. Pins do not replace independent
backups. The MCP server exposes
`argon_pin_create` / `argon_pin_list` / `argon_pin_sandbox`, the REST API
`/projects/:p/pins`.

## Diff and merge

A merge goes from a branch into its parent, through a plan that is computed,
persisted for review, and applied once against the exact heads it was
computed for. `argon diff` shows the plan without persisting it.

The plan is a three-way comparison per document: base (the branch's
inherited state at its fork point), ours (the parent now) and theirs (the
branch now), by canonical BSON equality of whole documents. A document the
branch did not change is left alone; one only the branch changed is adopted;
one both changed the same way needs nothing; one both changed differently is
a conflict, resolved for the whole plan by `--strategy theirs` or `ours`.

**Cost follows the change.** The branch's state is its fork state with the
branch's own entries folded on top, so the two can differ only in documents
those entries are about. The plan is therefore built from the documents
named by the branch's own visible entries in `(base_lsn, head_lsn]`. For
each, theirs is the newest of those entries, and base and ours are point
reads along the ancestry chain: each segment is asked for the document's
newest visible entry through the `(branch_id, collection, document_id, lsn)`
index, and the first segment that has one answers. Ancestry bounds,
discarded ranges and reclaim floors apply exactly as in a full replay, and
the test suite holds this path to a reference that materializes all three
states, on randomized histories with resets, nested branches, snapshots,
garbage collection and look-alike `_id` values.

What it does not avoid:

- **Reclaimed history.** A point read that finds no entry for a document in
  a segment has shown the document unchanged there only while that segment's
  entries are all in the log. Where garbage collection has deleted some, the
  segment's snapshot is the record, and it is loaded once per collection and
  side — a read the size of the collection. Snapshot chunks are sorted by
  key but the manifest does not index them, so one document cannot be
  fetched alone. The reclaim floor says where that is the case. Releases up
  to 2.1.5 recorded none, so below the newest snapshot manifest of that time
  the log is treated as possibly incomplete as well. An entry at or below a
  floor is never used as evidence: a deletion may have removed a newer one.
- **A reclaimed branch log.** If garbage collection has deleted entries of
  the branch being merged, its log no longer names every document it
  changed, and the plan is computed by materializing the three states in
  full.
- **History from before typed document keys.** Entries written up to 2.0.x,
  and entries rewritten by the schema-v1 migration, store document keys in
  several forms, some shared between different `_id` values. They are not
  read one document at a time: where a branch's entries of a collection
  start with such an entry, the plan is computed in full, with the errors
  that path has always reported.
- **A very long branch log.** The branch's own entries are scanned once. Past
  200,000 of them the states are materialized instead, which reads each
  document once however often it was rewritten.
- **Applying.** Apply writes each change; that is proportional to the plan.
- **The drain before the plan.** Diff and merge preview first drain the
  capture of the branch and of its parent, so that completed native writes are
  in the log. Draining opens the change stream at its stored position, and
  MongoDB scans the oplog written since, whichever database it was written
  to. A checkout keeps a checked-out parent's position current while it
  copies, so the new branch's own copy is not in that scan. Anything else
  written to the deployment since the parent was last drained or watched
  still is: keep `argon watch` or a managed process on a parent that sits
  beside heavy unrelated writes.

## Garbage collection (retention window)

`argon gc` (and `Services.RunGC`) deletes WAL entries that no reader can
ever need again. Per (branch, collection), the reclaim cutoff is

```
min( S_R,  min over live children of S_i )
```

where `S_R` is the newest snapshot that is valid for every possible reader
and lies at or below `R`, the last LSN older than the retention window
(default 7 days), and `S_i` is the newest snapshot at
or below each live child's fork point — children and all their descendants
read the parent's segment with an upper bound pinned to the fork, so they
can only be served by snapshots at or below it. Dataset pins enter the
same minimum as readers at their LSN while they exist: a pin at `P` clamps the
cutoff to the newest snapshot usable at bound `P`, and to zero — nothing
reclaimed — while no such snapshot exists. Entries at or below the
cutoff are deleted; control entries stay.

The cutoff is a snapshot rather than `R` itself because a read inside the
window starts from the newest snapshot at or below it and replays upward:
it needs the entries between that snapshot and the window, although they
are older than the window. Every LSN inside the window therefore stays
exactly readable after GC, as do forks and pins.

**Reclaimed history is refused, never misread.** Before deleting, GC
records the cutoff as the collection's reclaim floor on that branch. A
read replays from a snapshot (or from the start of a segment) upward; if
that replay would cross entries at or below the floor that the reader does
not skip as discarded, it fails with "history reclaimed by garbage
collection" instead of returning a state without those writes. The floor is
checked after the entries are read, so a GC run that completes during a
read is caught as well. Operations that would commit a branch to such a
position — `restore reset`, `restore branch`, `pin create --lsn`, an undo
range reaching into it — are refused up front. Positions that sit exactly
on a retained snapshot stay usable. GC runs performed by releases up to
2.1.5 left no floors behind; positions those runs reclaimed are not
detected.

Two consequences worth stating plainly: history without snapshot coverage
is **never** deleted, no matter how old — and reclaiming entries ends
time-travel, audit and undo below the cutoff, which is exactly what a
retention window means. With automatic snapshots every ~1000 LSNs, up to
that many entries just older than the window stay until the next snapshot
falls out of it.

Snapshots and GC bound replay and reclaim eligible history. Total storage
still grows with active branch states, existing pins and write volume inside
the retention window; retention is not a fixed disk-space quota.

## Write paths

There are exactly three ways state enters the WAL, and none reimplements
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

3. **A followed source database** — a branch that follows an external
   database receives that database's change stream as entries (see
   [Following an external database](#following-an-external-database)).
   It is the branch's only writer.

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
connection. The proxy is a router, not a security boundary: it listens on
loopback by default, forwards non-alias traffic untouched, and relies on
the upstream deployment for authentication. Frames are length-checked
before parsing and a malformed one ends only its own connection. Capture
stays asynchronous through the change-stream ingester
— the proxy is deliberately only the routing layer. It is the natural
place to one day close the ingest-lag window (see the roadmap), but not
by parsing writes: the change stream already hands the ingester
before/after images, transaction resolution and mongod's commit order for
free, and re-deriving those in the proxy would mean reimplementing
MongoDB write semantics — the one thing this design refuses to do.

## Following an external database

`argon import database --online` imports a database that is in use and leaves
the new project's `main` following it; `argon merge apply --write-back`
writes a reviewed plan to it. Both rest on one decision.

### The decision: copy and follow, not adopt in place

`main` stays what every branch is, a pointer into the WAL. The source
database is copied into it and then followed through its change stream. The
source is never a branch's physical database.

The alternative was to adopt the source in place: record it as `main`'s
physical database and run ordinary capture on it. That would have reused the
capture path unchanged and made a merge into `main` a write to the source for
free. It was not chosen, for three reasons:

- **Every destructive path would have had to learn an exception.** Checkout
  refresh, release, discard and the sandbox sweep all drop "the branch's
  physical database". With the source in that role, its safety would depend
  on each of them remembering not to. With copy-and-follow no branch record
  ever names the source, and the drop function accepts only databases Argon
  created.
- **Capture is allowed to change the database it watches.** It enables pre-
  and post-images on every collection and inserts marker documents to prove
  that a transaction's events are complete. Both are right for a database
  Argon made and wrong for one it did not.
- **It only works on one deployment.** A physical database lives next to the
  metadata. A production database usually does not.

The price of the decision is one copy of the data in the WAL, and a follower
that needed its own answers to the two things capture does by writing.

### Online import

A copy made with ordinary cursors while the source is written reads different
documents at different moments. It becomes a state of the source through the
source's own change stream:

1. Take the primary's cluster time `T0`, and only then list the collections.
   A collection created before `T0` is in the list; one created later
   arrives through the stream. Start a causally consistent session at `T0`
   and read everything in it with majority read concern.
2. Copy every listed collection into put entries.
3. Replay the change stream from just after `T0` on top of the copy. Each
   entry is a whole document, so applying a change the copy already contained
   leaves the same document.
4. Stop when the stream's position has passed the primary's cluster time
   taken after the last read. The branch now holds what the source held at
   that position, and that pair (LSN, cluster time) is published.

A write made during the import is in the copy, in the replayed stream, or in
both; it cannot be in neither, because the reads cannot show less than `T0`
and the stream starts there. Majority reads matter for the opposite error: a
read must not show a write that later rolls back, since no event would ever
correct it.

The copy has to finish while `T0` is still in the source's oplog. That is the
same bound MongoDB's own initial sync has, and exceeding it fails the import
rather than publishing something unproven.

### Following

The follower (`internal/ingest/follow.go`) turns each change event into a
physical entry, as capture does, and differs where the source is not Argon's:

- **Completeness without markers.** Entries are grouped by cluster time and a
  group is published once the stream has moved past its cluster time: either
  a later event arrived, or the stream's resume token, which advances with
  the oplog even when nothing concerns this database, stands at a later time.
  The cluster time is read from the token. MongoDB documents resume tokens as
  opaque; their encoding has begun with the cluster time since 4.0.7, the
  follower verifies the decoded value against the server's clock before
  relying on it and refuses to run if it cannot, and a test pins the property
  against the server in use.
- **Exact documents without images.** An update without an attached
  post-image is read from the source with a snapshot read at the update's
  cluster time, the same read capture uses for writes that reach a new
  collection early. It is exact, and it is only available inside MongoDB's
  snapshot history window. Outside it the follower stops as degraded, unless
  the operator allowed an inexact catch-up, which is the import's replay
  again: store the document's current state, and declare the branch
  consistent once the stream has passed every such read. The range in between
  is recorded in the capture status as inexact.
- **Two deployments.** Entries, head and resume token commit together on the
  metadata deployment, so exactly-once does not depend on where the source
  is.

A follower and a resync take the same per-branch lease in `wal_ingest_state`.
Acquisition and heartbeat timestamps use the metadata primary's clock. The
owner releases the lease on exit. If the heartbeat expires, another run can
take over. Only the current owner can update capture status or advance the
stream position. Transactions that publish the log and branch head also
check the lease, the branch's source and its checkout state. Detach removes
the saved state and a previous owner cannot write into a recreated record.
A head that moved without the follower, or a saved position another process
replaced, ends following as degraded and requires a manual resync. Automatic
resync handles gaps in the source stream and unfinished resyncs.

Which deployment a connection leads to is decided by the replica set's ID
when it can be read and by the set name plus member hosts otherwise, never by
the name alone.

A snapshot read at a cluster time shows a document after everything that
committed at that time. Within a multi-document transaction an earlier change
to a document may therefore be stored with the document's state at the end of
the transaction. Published heads are unaffected, because a head never falls
inside a cluster time.

Collection drops and renames stop the follower as degraded, as they stop
capture: the WAL has no operation for them. They are repaired by a resync
rather than handled in the stream, for three reasons. A drop or rename
happens at one cluster time, so its effect on the branch (a delete per
document, and for a rename a put per document as well) would have to be
published as one group, whose size is the collection's; publishing it in
pieces would be an inexact range nobody asked for. Derived from the branch's
own state it is only right while that state is exact: during a catch-up the
branch may hold a later state of a document than the one the rename moved,
and nothing would correct it afterwards. And the same machinery is needed
anyway for a position the oplog no longer holds, where there is no event to
derive anything from.

### Resync

`internal/ingest/resync.go` brings a followed branch whose follower ended as
degraded back to the source, in place. It is the import's copy-then-replay
with the copy replaced by a comparison, because the branch already has
documents:

1. Acquire the branch's lease, take the source primary's cluster time `T0`,
   list its collections and check that the change stream can open.
2. Record mode `catch_up`, the LSN the inexact range starts at, the resync
   marker and the stream's start position before writing any branch entries.
   Clear the previous resume token in the same transaction. If a preflight
   check fails, the previous capture status is preserved.
3. Source to branch: read every source document with majority reads in a
   causally consistent session that starts at `T0`; append a put unless the
   branch's newest entry for the document is a put of the same bytes.
   Flush each batch at the document-count or raw BSON byte limit.
4. Branch to source: walk the branch's document keys, look each batch up on
   the source by `_id` in the same session, and append a delete for every
   document the source does not have. A dropped collection has none of its
   documents on the source; a renamed one is that plus step 3's puts under
   the new name. Collection-level changes thus become the document-level
   operations the WAL has.
5. Replay the change stream from the `T0` boundary held in memory until it
   has passed every read. The transaction that stores the first head proven
   equal to the source also clears the marker and records the range.

The deletes are covered by the import's argument. A document found missing in
step 4 was either removed before `T0` (the delete is the only record of it)
or after (its event is replayed on top, harmlessly), and one that is inserted
after being found missing is put back by its insert event, which is stored
after the delete.

Step 4 does not hold a collection in memory. The keys come from the WAL's
`(branch, collection, document, lsn)` index, read in key order with one key
at a time; each batch is resolved to the branch's live documents with the
materializer's point read and checked against the source with one `$in`
query. Where garbage collection has reclaimed part of a collection's history
the log no longer names every document, and the collection is listed from its
snapshot instead, loaded whole as every reader of reclaimed history does.
Step 3 treats a document as unchanged only on evidence from the log itself
(its newest entry, above the reclaim floor); anything else is stored again,
which costs an entry and changes nothing.

The branch is visible throughout, and each batch moves its head. What keeps a
half-compared branch from passing for a healthy one is the status written in
step 2: the follower refuses to open a stream while the marker is set, and
the saved position is only meaningful on top of a finished comparison. A
resync that dies is run again by `source resync` or `source follow --auto-resync`.
It takes a new `T0` and compares whatever the branch holds.

A drop or rename that arrives during step 5 fails the resync as it fails the
follower, with the marker left in place.

### Write-back

A plan is written to the source and nowhere else; `main` learns of it through
the follower. One writer per branch is kept, and there is nothing to
reconcile between two deployments.

The write is one source transaction that compares each document with the
plan's before-image and then inserts, replaces or deletes it, in a collection
that already exists: it never upserts, so it cannot create one. The
comparison is on the bytes the followed branch stored for the document when
it has them, which makes a reordered document a changed one. A replacement is
sent with its fields in the order the source's document has them, so that
only what the plan changed changes. Because the source and
the metadata may be different deployments, the plan's status cannot commit
with the documents. Instead the plan is claimed first, with the ID of a
session used for nothing else; the follower recognises change events from
that session, attributes their entries to the merge, appends the `merge`
record and marks the plan applied in the same transaction that stores them.
A crash between the source commit and its bookkeeping therefore leaves a plan
that cannot be applied again and that the follower closes on its own.

### Limits

- Replica-set sources only. Sharded clusters are refused.
- Indexes, validators and collection options are not followed. Collection
  drops and renames end following until `argon source resync` is run; a
  source that drops or renames collections as a matter of routine (`$out`,
  temporary collections) needs a resync each time.
- Following runs through `argon source follow`. The MCP server and the REST
  control plane do not supervise it, and neither offers write-back.
- A resync restores the source's current state, not its history: the
  changes between the last applied one and the resync are recorded as their
  outcome only, inside a range marked inexact. A write-back that committed in
  that gap is not attributed to its merge and its plan stays `writing_back`.
- A resync reads every source document once and needs the oplog to cover its
  duration. It is not incremental.
- A resync that is interrupted leaves `main` between two states, marked as
  unfinished, until it is run again; `main` is not held at its old state
  while a resync runs.
- An update event that MongoDB cannot return because it exceeds 16 MB with
  its images ends following as degraded at that position, as does any error
  the source answers with repeatedly while nothing can be stored. A resync
  gets past it.
- A source with a time-series collection cannot be followed: its
  measurements never reach a change stream as documents. Importing one is
  refused, and creating one on a followed source ends following as degraded.
  Views are neither imported nor followed.
- A collection MongoDB does not serve at a past cluster time (a capped one
  up to 7.0; on 6.0 also any collection altered after the update) can only
  be followed exactly through attached images.
- A saved stream position that is a high-water mark is not verified by the
  server on resume. A source restored to an earlier state under the same
  replica-set ID, with an oplog that still covers the position, is followed
  on from there without the rewind being noticed.
- Processes older than this feature do not know a followed branch is
  read-only. The follower detects that the head moved without it and stops
  as degraded; it cannot stop the older process from writing.
- Write-back never creates a collection. A replaced document keeps the field
  order production gave it; fields the plan adds follow in the sandbox's
  order.

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
  Write-back to a followed source makes the same comparison inside the source
  transaction.
- **Followed branches:** a branch that follows an external database is
  written only by its follower. Its published heads are the source at one
  cluster time, except inside ranges its capture status lists as inexact.
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
7.0.14. `argon doctor` checks real image/transaction permissions. Checkout
prepares materialized collections itself, and capture enables images on a
collection the application creates as soon as it sees it.

Writes that reach a new collection before then arrive without images. Capture
does not reconstruct them from the update description, which cannot convey
field order or tell a moved field from an updated one. It asks MongoDB for the
document as of the event's cluster time, and as of the cluster time just
before it, with a snapshot read. A recovered image is therefore the stored
document, byte for byte, and the entry records `capture_images: snapshot`.
This has two limits. MongoDB keeps snapshot history for a bounded time
(`minSnapshotHistoryWindowInSeconds`, five minutes by default), so the event
has to be captured within it. And a snapshot shows a document only before or
after everything that commits at one cluster time. Inside a transaction that
gives a change's pre-image when the change is the document's first and its
post-image when it is the last; a change that follows one whose event carried
the document takes that document as its pre-image. What cannot be read is the
state after an update that is followed by another change to the same document
in the same transaction, and that stops capture with a reported gap. Running
`argon collections prepare <collection> -p <project> -b <branch>`, or creating
the collection with `changeStreamPreAndPostImages: {enabled:true}`, before the
first write avoids both.

The same read corrects one image MongoDB attaches wrongly. On MongoDB 7.0 and
8.x, after an update that moves a field, the attached post-image names that
field twice: once where it was, with the old value, and once at the end with
the new one. Capture detects a repeated field name and stores the document read
back from the snapshot instead. Inside a transaction, or outside the snapshot
history, it keeps the attached image and records
`capture_images: attached_unverified`; materialization decodes images into
maps, where the later value wins, so the state is correct either way.

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
- The published small-scale benchmark matrix includes native capture delay,
  end-to-end sandbox readiness and divergence storage categories. Expanded
  dataset/concurrency/depth matrices and longer recovery runs remain separate
  evidence: do not generalize small runs or twenty-sample p99 values into
  production latency guarantees. The public `WriterFor` API is available.

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
| `wal_ingest_state` | Durable capture boundary, actor, resume token and last health report; for a followed branch also its source position, consistent point and inexact ranges |
| `wal_sources` | Which external database a branch follows: database, hosts, replica-set name and ID. No credentials |
| `wal_merge_plans` | Merge plans; a written-back plan also records its source session and commit |
| `wal_snapshot_locks` | Persistent publication/GC locks; see operations recovery |
| `wal_reclaim_floors` | Per (branch, collection): the LSN at or below which GC has deleted entries |

Indexes on `wal_log`: unique `(project_id, lsn)`;
`(branch_id, collection, lsn)`; `(branch_id, collection, document_id, lsn)`;
`timestamp`.
