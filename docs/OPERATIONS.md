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

- **Source builds require Go 1.26.11 or newer (1.27.4+ on the Go 1.27 line).** Root, CLI and API modules declare
  the same minimum toolchain. CI scans all three modules with official
  `govulncheck` against the current vulnerability database and fails on reachable
  vulnerabilities. Reproduce it with
  `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` from each module.
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
| `argon proxy` | optional | stable `project~branch` connection strings on `127.0.0.1:27018` (see [Exposure](#exposure-and-credentials) before listening elsewhere) |
| `argon source follow -p P` | one per project that follows a source | [Experimental and off by default](#following-a-source-database). Applies the source database's changes to `main` and resumes from its saved position. With `--auto-resync`, repairs a degraded end itself. |
| `argon sandbox sweep -p P` | schedule for CLI-only use | reap expired sandboxes (pinned ones are skipped loudly) |
| `argon gc -p P` | cron | reclaim covered, out-of-retention WAL entries |

## Importing existing data

`argon import database` copies ordinary documents with sequential cursor reads.
A cursor reads different documents at different moments, so a copy of a
database that is being written is not, by itself, a state that database was
ever in. There are two ways to get one that is. Either way indexes, validators
and collection options are not copied, and `--yes` only skips the
target-creation confirmation.

### Online: the source keeps running

Online import is experimental, off by default, and has known open issues.
Set `ARGON_EXPERIMENTAL_SOURCE=1` to enable it.

```sh
export ARGON_EXPERIMENTAL_SOURCE=1
export ARGON_SOURCE_URI="mongodb://…"      # used per command, never stored
argon import preview  --database myapp
argon import database --database myapp --project myapp --online --yes
argon source follow -p myapp               # keep main current from here on
```

`--online` (releases after 2.5.0) needs the source to be a replica set; a
sharded cluster behind mongos is refused, and a standalone has no change
stream. The import takes the source's cluster time, copies every collection,
and then replays the source's change stream from that time on top of the copy
until the stream has passed everything the copy read. Because entries are
whole documents, replaying a change the copy already contained is harmless.
What is published is the source at one cluster time, and the command prints
it together with the LSN it corresponds to (`consistent_lsn`,
`consistent_cluster_time`). LSNs below that are the copy in progress; they
are recorded as an inexact range and are not states the source was in.

What the import does to the source: majority reads, one change stream, and
nothing else. It creates nothing there. Writes made during the import are
neither lost nor blocked.

What can make it fail, in which case nothing is published and the reserved
project is removed:

- **The copy outlasts the source's oplog.** The replay starts at the cluster
  time taken before the copy; if the oplog no longer reaches back that far
  when the copy ends, the import stops. Give the source a larger oplog, or
  import when it is quieter.
- **A collection is dropped or renamed on the source during the import.**
  Such events carry no documents; the import does not guess.

### Quiesced: nobody is writing

```sh
argon import preview --uri "$SOURCE_MONGODB_URI" --database myapp
# Pause source writers and DDL now.
argon import database --uri "$SOURCE_MONGODB_URI" --database myapp --project myapp --source-quiesced --yes
# Source writers may resume after success.
```

Stop all source application writes, jobs and DDL before starting, and keep them
stopped until completion. Pass `--source-quiesced` to acknowledge that contract;
the command cannot prove that an external writer is paused. If writers resume
during copying, consistency is outside the contract: discard that result and
repeat. The result is a one-time copy that does not follow the source. This is
the only way to import a standalone mongod.

### Preflight, failure and recovery

Preflight verifies readable ordinary collections before creating the target;
views/time-series collections and an absent/no-collection source are rejected
(an online import skips views, which hold no documents, and rejects
time-series collections).
The reserved target is hidden from normal project lookup/listing until every
collection succeeds. A handled error or cancellation removes that unpublished
project, its branches, WAL and counter transactionally, so retrying its name
starts cleanly. Cleanup uses a separate context when the copy was canceled.

If the process is killed, or the metadata database was unavailable during
cleanup, stop the original importer and recover explicitly:

```sh
argon import cleanup --project myapp --yes
```

This command accepts only an unfinished import; it refuses ordinary or completed
projects. Do not run recovery concurrently with the original importer. Imports
restart from the beginning; a copy cannot be resumed part-way. Cleanup removes
rows in Argon's metadata database (for an online import that includes the
source binding and the saved stream position); it never touches the source.
Destination metadata needs the normal writable replica set for transactional
recovery, even when the quiesced source is standalone.

## Following a source database

The `argon source` commands and `merge apply --write-back` are experimental
and off by default. They have known open issues. Set
`ARGON_EXPERIMENTAL_SOURCE=1` to enable them. A branch already bound to a
source still refuses ordinary merges, undo, reset, checkout, and programmatic
writes while the switch is off.

A project imported with `--online` stays tied to its source: `main` follows
that database. Argon keeps a record of which deployment and database that is
(`wal_sources`: replica-set name and ID, database name, hosts) but not how to
log in to it. Every command that reaches the source takes the connection
string again, from `--uri` or `ARGON_SOURCE_URI`, and refuses a deployment
other than the recorded one.

What "the recorded one" rests on depends on what the importing user could
read, and `argon source status` shows which (`identity`):

- **The replica set's ID**, when the import could run `replSetGetConfig`.
  Every later connection then has to show the same ID. One that cannot read
  it is refused, however well the names match: grant the user
  `replSetGetConfig` (the `clusterMonitor` role has it).
- **The set name and member hosts**, when it could not. A later connection
  has to report the same set name and at least one of the hosts recorded at
  import. A matching name alone is never accepted: separate deployments
  routinely share one, and a staging set is often a restore of production.
  The first verified connection that can read the ID records it, and the
  stronger check applies from then on. This weaker check cannot tell two
  deployments apart if they share a set name and a member host name.

### What `argon source follow` guarantees

- **No change is skipped or repeated across restarts.** The entries, the
  branch head and the stream's resume token commit in one transaction on the
  metadata deployment. This holds whether or not the source is the same
  deployment as the metadata.
- **A published head is the source at one cluster time.** Changes are
  published a whole cluster time at a time, so a transaction is never half
  visible. `argon source status` shows the newest such point. On a source
  nobody else writes to, the last change appears only when the source's next
  oplog entry does; MongoDB writes one every ten seconds when idle.
- **Stored documents are exact.** An insert or replacement carries its
  document. An update's document is read from the source as of the update's
  own cluster time and stored byte for byte.
- **Detaching stops every follower.** `argon source detach` cannot reach a
  follower in another process, so the follower's own writes are conditional
  on the branch still following the source, in the same transaction. After a
  detach its next write fails, it stops, and `main` is not written again.
- **Nothing is written to the source.** No marker collections, no collection
  options. The only writes Argon ever makes to a source are `--write-back`
  transactions and, when asked for by name, `--enable-source-images`.

### What stops it, and what it then reports

Following ends as `degraded`, with the reason in `argon source status` and
`argon status` (both exit nonzero), and with `main` left at the last change
before the problem:

- **The saved position is no longer in the source's oplog.** Following was
  stopped for longer than the oplog covers. Run `argon source resync`.
- **A collection was dropped or renamed, or the database was dropped.**
  Run `argon source resync` to follow it past that point.
- **An update's exact document can no longer be read.** MongoDB serves a past
  cluster time only for its snapshot history window
  (`minSnapshotHistoryWindowInSeconds`, five minutes by default). If the
  follower is stopped or lags for longer than that, updates older than the
  window cannot be read as they were. There are two ways out, both explicit:

  - `--allow-inexact` lets following continue. Updates it can no longer read
    exactly are stored with the document's current state, `main` converges on
    the source again, and the LSN range in between is recorded as inexact:
    `argon source status` lists it, and the entries in it are marked
    `capture_images: current`. The state at the end of the range is exact;
    positions inside it are not states the source was in, and time travel
    there shows a mixture. While it lasts the state is `catching_up`, never
    `running`.
  - `--enable-source-images` (on `import database --online` or
    `source follow`) switches on `changeStreamPreAndPostImages` for the
    source's existing collections, so updates carry their own documents for
    as long as the oplog keeps them and no reads are needed. **This changes
    the source.** MongoDB then stores a pre-image of every updated or deleted
    document in `config.system.preimages` until it expires with the oplog (or
    after `changeStreamOptions.preAndPostImages.expireAfterSeconds`): more
    writes and more storage on the source for every update and delete, a
    `collMod` on each collection, and the source user needs the privilege to
    run it. Argon never does this unless the flag is given, and does not
    undo it, also when the import that asked for it then fails: the error
    names the collections that were changed. The change is made only after
    the import has reserved its project and bound the source, and for
    `source follow` only once following can actually start. Collections
    created later are not covered until the flag is given again. It helps
    with updates made after it is enabled, not with ones already waiting in
    the stream.

  On MongoDB 6.0 an update cannot be read at its own cluster time once its
  collection has been altered afterwards: an index build, a `collMod`
  (enabling images is one), a rename or a drop. Updates the follower had not
  stored by then stop it, with a message that says so; `--allow-inexact` or a
  resync gets past them. On 6.0, enable images while the follower is caught
  up, not while a backlog is waiting. MongoDB 7.0 and 8.x serve such reads
  (checked on 7.0.45 and 8.3.17).

  A collection MongoDB does not serve at a past cluster time has no exact
  reads at all. A capped collection is one on MongoDB 7.0 and earlier (8.x
  serves it). Its updates are followed
  exactly only through attached images; without them every one of them stops
  following, or with `--allow-inexact` is stored as the document's current
  state inside one inexact range whose reason names the collection.

Following also ends as `degraded`, rather than retrying for ever, when the
source keeps answering the same position with an error and nothing can be
stored in between: an update event too large to return (over 16 MB with its
images), a privilege the user lacks, a transaction too large to store in the
time allowed. An import in that situation fails with the error instead of
hanging. And it ends as `degraded` when something else has written to `main`
(a process from before following existed does not know the branch is
read-only) or has replaced its saved position: the head on record no longer
being the branch's head is checked on every start and on every write.

A time-series collection created on a followed source ends following: its
measurements never reach a change stream as documents, so `main` could not
equal such a source, and a resync refuses it for the same reason. Views are
ignored; they hold no documents. Collections named `config.*`, `admin.*` or
`local.*` inside the source database are ordinary collections and are
imported and followed (a quiesced import still skips them).

Without images, each followed update costs the source one point read. Pre-images
of followed changes are stored only when the source supplies them, so `main`
entries have none unless images are enabled.

### Repairing a degraded follower: `argon source resync`

```bash
argon source status -p myapp     # degraded: drop of scratch on the source ...
argon source resync -p myapp     # compares, repairs, prints the consistent point
argon source follow -p myapp     # continues from there
```

Resync brings `main` to the source's current state without a second project.
It takes a stream position on the source, compares the source with `main`,
and then replays the source's changes from that position until `main` equals
the source at one cluster time, which it prints with the LSN.
Before it writes anything it opens the source's change stream at that
position: a user who may not read the stream is refused there, with `main`
untouched and no unfinished resync on record.

What it does to `main`:

- Every source document `main` does not hold byte for byte is stored. A
  document `main` already holds is not stored again, so the cost in history
  is the size of the difference, not of the database.
- Every document `main` holds that the source no longer has is deleted. A
  collection dropped on the source loses all its documents on `main`; a
  renamed one ends up under its new name; a dropped database leaves `main`
  empty. Run against the right source: the deployment is checked against the
  one imported from, as for every source command.
- `main` keeps its history. The state before the resync can still be read at
  its LSN, and sandboxes forked from it are not affected.

What it guarantees, and what it cannot:

- **Nothing is written to the source.** It issues reads only: one pass over
  every collection, and one `_id` lookup per batch of `main`'s documents.
- **The end is a state of the source.** `main` at the printed LSN is the
  source at the printed cluster time, also when the source was written
  throughout.
- **The LSNs in between are not.** While it runs `main` moves batch by batch
  and is reported as `catching_up`; afterwards the range from the first LSN
  after the last consistent point to the printed LSN is listed by
  `argon source status` as inexact, with a reason that starts with `resync`.
  Time travel into it shows a mixture.
- **What happened on the source in the gap is not in `main`'s history.**
  Between the last change the follower applied and the resync, only the
  outcome is recorded: one put or delete per document that differs, not the
  individual changes, their order or their times. A write-back whose
  transaction fell into that gap is not attributed to its merge, and its plan
  stays `writing_back`.
- **An unfinished resync is never reported as healthy.** The status is
  marked before the first write to `main`. If the resync is interrupted or
  fails after that, the status keeps `resync_unfinished`, mode `catch_up`
  and the old consistent point; `source status` exits nonzero and
  `source follow` refuses to start. Running `source resync` again completes
  it, comparing whatever `main` holds by then; the recorded range covers
  every attempt. A sandbox forked from `main` in that state starts from the
  mixture, as it does during an `--allow-inexact` catch-up.
- **It fails rather than guess** when the source drops or renames another
  collection while it runs; run it again. A source that does this
  continuously (for example an aggregation with `$out`, which renames a
  temporary collection on every run) cannot be followed without repeated
  resyncs.

Memory is bounded by the batch (500 documents), not by collection size. The
exception is a collection whose history on `main` garbage collection has
partly reclaimed: its documents are then listed from its snapshot, which is
loaded whole. The source's oplog has to cover the duration of the resync, as
for the import.

`argon source follow --auto-resync` does the same when following ends as
degraded and then continues following. It is off by default because each
resync reads the whole source and leaves an inexact range; it gives up if
following ends as degraded again within a minute of a resync.

Resync refuses a project whose follower did not end as degraded (`--force`
overrides that), and it must not run alongside `argon source follow` for the
same project: the two would compete for the branch head and one would fail.

### Writing a plan back to the source

`main` cannot be merged into while it follows a source, because it would then
hold documents its source does not. A reviewed plan is written to the source
instead:

```sh
argon merge preview -p myapp -b <sandbox>
argon merge apply <plan-id> --write-back
```

- The write is one transaction on the source, with snapshot reads and
  majority writes. Inside it every document in the plan is read and compared
  with the before-image the plan recorded. The first one that differs aborts
  the transaction: nothing is written, the plan is pending again, and the
  error names the document. Let following catch up and preview again; the
  new plan shows the production change as a conflict to resolve.
- The plan does not have to match `main`'s head, which moves with every
  production write. The per-document comparison is the staleness check.
- Single-document inserts, replacements and deletes are sent, nothing else,
  and only into collections the source already has. A plan that puts a
  document into a collection the source lacks is refused before anything is
  claimed or written: create the collection there, with the options it should
  have, and preview again. Write-back never upserts. The check is made just
  before the transaction; a collection dropped in the instant between the two
  would be created again by an insert.
- A replacement keeps production's layout. Fields the document already had
  stay in the order production has them, at every level of nesting, and
  fields the plan adds follow. An inserted document has the order the sandbox
  stored, or sorted fields when the sandbox was written through the SDK.
- The comparison with the before-image is byte for byte when `main` holds the
  source's own bytes for that document, which it does for everything the
  import copied or following stored: a document whose fields production
  reordered since the review is a conflict. When those bytes are not
  available (reclaimed by GC, or stored from an image MongoDB attached with a
  repeated field) values are compared and field order is not.
- Write-back is refused while following is `degraded` or a resync is
  unfinished: the plan was reviewed against a `main` known not to be the
  source. Resync, then preview again.
- `main` is not written by the write-back. It receives the change through
  following, like any other change to the source, attributed to the merge
  (`actor: merge:<sandbox>`) with a `merge` record carrying the plan ID.
- The plan is claimed in the metadata before the transaction starts and
  cannot be applied twice. If the process dies after the source commit and
  before recording it, the plan stays `writing_back`; the follower closes it
  as applied when it sees the transaction in the stream. A claim whose
  transaction never committed (the process died first, or was interrupted
  before the commit) is returned to pending: at once when the interruption is
  seen, otherwise by the follower once `main` has passed the claim by ten
  minutes without that transaction appearing. This relies on the clocks of
  the writing host and the source agreeing to within minutes.
- Undoing a write-back means writing another plan back. `argon undo` does not
  apply to a branch that follows a source.

### What Argon can never do to a source

Argon drops databases in exactly one function, which accepts only the
database it created for the branch in question (`argon_br_<branch-id>`) and
consults the source registry as well. A database with that prefix, the
metadata database and MongoDB's system databases cannot be imported online,
so a followed source can never carry a name a drop path accepts. A branch
record that names any other database as its physical database is refused by
release, discard, the sandbox sweep, capture, live merge and undo, and the
wire proxy. Import cleanup deletes rows and snapshots in the metadata
deployment only.

In the packages that can hold a connection to a source, every call that
writes to a database or sends it a command is on a list in the test suite, by
file and function, and the build fails when one is added or removed without
the list being updated. Three of them reach a source: the write-back
transaction body (single-document insert, replace and delete), the `collMod`
of `--enable-source-images`, and read-only commands (`hello`,
`replSetGetConfig`, `find`, `collStats`). The list pins where such calls are,
not what a future one would do; a reviewer still has to look.

### Source permissions

Reading and following need `find` and `changeStream` on the source database
and the ability to run `hello`. `replSetGetConfig` is what identifies the
deployment for certain (see above); once a project has recorded the ID, every
user that reaches the source for it needs that privilege too. `--write-back`
needs `readWrite` on the database. `--enable-source-images` needs `collMod`.

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
at or below the newest snapshot that is older than the retention window and
that every future reader can use, below every live child's fork-point
coverage, and below every pin's coverage. Consequences, stated plainly:

- No snapshot → nothing is ever deleted, no matter how old.
- Reclaiming entries ends time-travel/audit/undo below the cutoff — that
  is what a retention window means; pick it accordingly (default 7 days).
  Every LSN inside the window stays readable: the cutoff is a snapshot at or
  below the window's start, never a point between two snapshots.
- An older position that can no longer be rebuilt is refused with
  `history reclaimed by garbage collection` — by time-travel queries, and by
  `restore reset`, `restore branch`, `pin create --lsn` and `undo` before they
  change anything. It is not answered with partial data. (GC runs made by
  2.1.5 and earlier are not recorded, so this protection starts with the
  first GC run after upgrading.)
- Existing pins protect the history needed to materialize their referenced
  captured states. Deleting a pin releases that protection; pins do not replace
  independent backups.
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

A project that follows a source reports through the same capture status as
checked-out branches, with a `source` section: mode (`exact` or `catch_up`),
position, newest consistent point and inexact ranges.

`argon status` reports connectivity and system health; `argon metrics`
prints performance counters (operation rates, latencies, error rates). The
services log ingester lifecycle events and snapshot/GC warnings to stderr;
`wal.Monitor` runs periodic health checks inside every long-lived process.

## Authentication

Argon passes credentials through `MONGODB_URI` untouched. With the wire
proxy, clients must set `authSource=admin` explicitly (the URI database is
a branch alias, not a real database SCRAM can run against).

A source connection string (`--uri`, `ARGON_SOURCE_URI`) is used for the
command it is given to and is not written anywhere. Errors, status reports
and stored documents show the source as scheme, hosts and database only;
the connection string itself, the `user:password` pair, the password in any
escaping and the values of options that carry secrets
(`tlsCertificateKeyFilePassword`, `authMechanismProperties` and the like) are
removed from driver errors before they are printed or stored. A user name on
its own is not removed, and neither is a password shorter than four
characters outside the string and the pair, because removing it would cut up
ordinary words. Prefer the environment variable to `--uri`, which is visible
in the process list.

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
`changeStreamPreAndPostImages: {enabled: true}`. From 2.3.1, capture reads back
the exact states of updates and deletes made before images were enabled, as long
as it processes them within MongoDB's snapshot history
(`minSnapshotHistoryWindowInSeconds`, five minutes by default); an update that
is followed by another change to the same document in the same transaction, in
that interval, cannot be read back.
A watcher retries temporary index-build
or topology errors; a post-image that can no longer be read back, unsupported
drop/rename or expired resume token marks capture degraded. Stop writers, inspect the reported cause
and preserve the physical data. Rebuild/import into a new branch from a trusted
state when a history gap cannot be repaired; do not clear a token to pretend
that missing history was captured.

## Managed processes and lifecycle

`argon console` and API processes supervise live capture and sweep expired
sandboxes every minute. MCP does the same for its managed branches while the
process runs, and after a restart resumes capture for branches left checked out
(2.4.0 and newer). Bare CLI checkout/sandbox commands do not create a background
worker: keep `watch` running and schedule `sandbox sweep` explicitly. Automatic
snapshots are triggered by native capture and programmatic writers; retention
GC still runs through its explicit command/service.

Run one capture supervisor per branch. Checkpoint fencing prevents stale
streams from committing against a newer head; restart recovery does not
coordinate which process owns capture.

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

A control plane without a token also checks the `Host` header: it answers to
`localhost`, `*.localhost` and loopback addresses only, and returns 403 for any
other name (`/health` excepted). This is what keeps a web page from reaching
your local console under its own hostname (DNS rebinding). If you reach the
console through a tunnel, forwarded port or reverse proxy that presents another
name, list it: `ARGON_ALLOWED_HOSTS=console.internal,dev.example.com` (hostnames
without ports; `*` turns the check off). Setting the variable also enforces the
list on token-protected servers.

Forwarding headers are ignored unless `ARGON_TRUSTED_PROXIES` names the proxies
that write them (IPs or CIDR ranges). The demo's per-client write budget is
keyed on the client address, so behind a load balancer set this to the
balancer's range; otherwise all visitors share the balancer's budget, and
without it no caller can mint a fresh budget by inventing `X-Forwarded-For`.

`argon proxy` listens on `127.0.0.1:27018` by default. It authenticates nobody
itself: every connection is forwarded to the upstream deployment, including
commands for databases that are not branch aliases. A non-loopback `--listen`
address is refused unless `--allow-remote` is passed, and should only be used
when the upstream MongoDB requires authentication — against an open
development mongod it would publish that database to the network.

Native URIs use the engine's MongoDB credentials. They are suitable only for
trusted clients sharing those database permissions. For mutually untrusted
agents, provision restricted users or separate deployments; API bearer auth
does not restrict a returned MongoDB credential. Changing the URI's database
preserves its original SCRAM authentication database via `authSource`.

Capture status may include `last_captured_at`, `last_event_at`, `head_lsn` and
`last_event_lag_ms`. Lag is the last event wall-clock-to-checkpoint sample, not
a continuously measured queue lag or latency guarantee.
