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
| `argon source follow -p P` | one per project that follows a source | [Applies the source database's changes](#following-a-source-database) to `main` and resumes from its saved position. The experimental `--auto-resync` option repairs source stream gaps and completes interrupted resyncs |
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

Online import and following are available by default.

```sh
export ARGON_SOURCE_URI="mongodb://…"      # used per command, never stored
argon import preview  --database myapp --online
argon import database --database myapp --project myapp --online --yes
argon source follow -p myapp               # keep main current from here on
```

The preview estimates document counts and sizes using the online collection
rules. Both preview modes skip views. To check whether the import can start,
run `argon import database
--database myapp --project myapp --online --dry-run`. It checks the database
name, replica-set topology and collection rules, then opens and reads the
change stream with the replay's options and start position. It reports the
collections it would copy and estimated document counts without initializing
metadata, copying data or enabling source images.

`--online` (releases after 2.5.0) needs the source to be a replica set; a
sharded cluster behind mongos is refused, and a standalone has no change
stream. The import takes the source's cluster time, copies its ordinary
collections, and replays the source's change stream from just after that
time until the stream has passed everything the copy read. Because entries are
whole documents, replaying a change the copy already contained is harmless.
What is published is the source at one cluster time, and the command prints
it together with the LSN it corresponds to (`consistent_lsn`,
`consistent_cluster_time`). LSNs below that are the copy in progress; they
are recorded as an inexact range and are not states the source was in.

By default, the import reads from the source and opens its change stream for
preflight and replay. Writes made during the import continue as usual.
Online import, following and resync preserve documents with a BSON null
`_id`.

What can make it fail, in which case nothing is published and the reserved
project is removed:

- **The copy outlasts the source's oplog.** The replay starts at the cluster
  time taken before the copy; if the oplog no longer reaches back that far
  when the copy ends, the import stops. Give the source a larger oplog, or
  import when it is quieter.
- **Another writer changes the unpublished branch head.** Each copy batch
  verifies the head left by the previous batch, beginning with the head
  created for the import. Replay verifies the final copy head. Stop the
  unexpected writer before retrying the import.
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

The `argon source` commands and write-back are available by default. A branch
bound to a source refuses ordinary merges, undo, reset, checkout and
programmatic writes. The experimental options `--auto-resync` and
`--enable-source-images` are off by default and each requires
`ARGON_EXPERIMENTAL_SOURCE=1`.

A project imported with `--online` stays tied to its source: `main` follows
that database. Argon keeps a record of which deployment and database that is
(`wal_sources`: replica-set name and ID, database name, hosts) but not how to
log in to it. Every command that reaches the source takes the connection
string again, from `--uri` or `ARGON_SOURCE_URI`, and checks it against the
binding recorded at import.

What "the recorded one" rests on depends on what the importing user could
read, and `argon source status` shows which (`identity`):

- **The replica set's ID**, when the import could run `replSetGetConfig`.
  Every later connection then has to show the same ID. One that cannot read
  it is refused. Grant the user `replSetGetConfig` (the `clusterMonitor`
  role has it). A file-level or volume snapshot clone keeps the ID, so a
  matching ID identifies the deployment or a file-level clone of it. Check
  where the connection string leads when the same ID exists on a clone.
- **The set name and member hosts**, when it could not. A later connection
  has to report the same set name and at least one of the hosts recorded at
  import. Later connections leave that binding in place, even if they can
  read the ID. Two deployments can present the same set name and member
  hosts, for example through a tunnel or in clusters with the same internal
  names. This check supports following and resync. Write-back and
  `--enable-source-images` require an ID recorded at import, and
  `argon source status` reports when the binding rules out write-back.

A network failure while reading `replSetGetConfig` aborts the preflight.

If all recorded member hosts have changed for a project bound by name and
hosts, Argon can no longer verify its source. To resume following in that
case, or to enable source writes for a project bound by name and hosts,
detach it with `argon source detach -p <project> --yes` and import the
source online into a new project. Use a source user allowed to run
`replSetGetConfig` for the import.

### What `argon source follow` guarantees

- **One owner per followed branch.** A follower or resync holds a lease on
  the metadata deployment. An overlapping run is refused while that lease
  is fresh, without changing the branch's status. The metadata server's
  clock determines when a lease expires, 60 seconds after the last
  heartbeat. Expiry revokes ownership immediately. The run cannot renew
  the lease, update status or advance the branch. A clean stop releases the
  lease. A follower that cannot renew against the metadata deployment for a
  full lease period stops with an error. Configure its supervisor to restart
  `argon source follow` after failures so it resumes from the saved position.
  A replacement records the lost owner when it takes over. To verify the
  position it must commit an exact checkpoint at or beyond the source time
  observed after acquisition, or complete a resync.
- **No change is skipped or repeated across restarts.** The entries, the
  branch head and the stream's resume token commit in one transaction on the
  metadata deployment. This holds whether or not the source is the same
  deployment as the metadata.
- **A verified exact head is the source at one cluster time.** Changes are
  published a whole cluster time at a time, so a transaction is never half
  visible. `argon source status` shows the newest such point. The last change
  waits for the next oplog entry. MongoDB's idle writer checks every ten
  seconds by default and writes only if nothing was written since its previous
  check. On an otherwise idle source the wait is ten to twenty seconds, or
  between one and two check intervals if `periodicNoopIntervalSecs` was
  changed. Any other write to the source deployment ends this wait at once.
  The “stored N ms after it happened” time in `argon source status` includes
  this wait.
- **Exact mode preserves source documents.** An insert or replacement carries its
  document. An update's document is read from the source as of the update's
  own cluster time and stored byte for byte.
- **Detaching stops every follower.** `argon source detach` cannot reach a
  follower in another process, so the follower's own writes are conditional
  on the branch still following the source, in the same transaction. After a
  detach its next write fails, it stops, and `main` is not written again.
- **Nothing is written to the source.** No marker collections, no collection
  options. The only writes Argon ever makes to a source are `--write-back`
  transactions and, when asked for by name, `--enable-source-images`.

### What `argon source status` reports and what to do

Both output formats report a reason and a next action. In JSON, read
`health.kind`, `health.reason`, `health.message` and `health.next_action`.
The message retains any stored capture error. The report also shows the
position, last consistent point and lease owner's host, process ID and last
heartbeat. The metadata server's clock determines lease freshness. Status
confirms an expired heartbeat by reading the metadata again in a fresh snapshot.

The table is in precedence order. When several conditions apply, the first
matching row determines the reason. An exact checkpoint establishes what
`main` held **at the reported source cluster time**. A cleanly stopped
follower can have a healthy checkpoint while newer source writes await replay.
Checkpoints without a position digest keep their existing health rules and
gain a digest the next time a follower or resync saves a position.

| Reason | What Argon found | What to do |
| --- | --- | --- |
| `importing` | The project is still importing. Deployment health omits it until publication. | Wait for publication. |
| `project_missing` | The followed branch has no project record. | Restore the missing project metadata. |
| `checked_out` | The followed branch is checked out or records a physical database. | Upgrade processes sharing the metadata, stop writers to the checkout, run `argon release -p P -b main`, then resync. Release discards checkout writes that never reached the WAL. |
| `head_changed` | The saved head and branch head differ. | Upgrade processes using this metadata, then run `argon source resync -p P`. |
| `resync_unfinished` | A resync started and has yet to commit its final checkpoint. | Let the owner finish. After interruption, run `argon source resync -p P` or `argon source follow -p P --auto-resync`. |
| `position_missing` | The saved position is missing, differs from its stored digest, or no longer identifies a usable position for this source. | Upgrade processes using this metadata, then resync. |
| `head_missing` | The branch or its checkpoint is missing its head LSN. | Resync, then start following. |
| `mode_unknown` | The checkpoint mode is absent or unrecognized. | Resync, then start following. |
| `range_inconsistent` | The checkpoint reports exact mode with an open inexact range. | Resync, then start following. |
| `checkpoint_unverified` | Exact mode lacks a consistent point matching both the branch head and the saved source time. | Run `argon source resync -p P` to rebuild the checkpoint, then start following. |
| `state_unknown` | The stored follower state is absent or unrecognized. | Resync, then start following. |
| `owner_expired` | The persisted owner passed its deadline, including during preflight with the previous state still marked stopped. | Start `argon source follow -p P` to take over and verify the checkpoint. |
| `degraded` | The follower recorded a persistent capture failure. | Address the reported cause, resync, then start following. |
| `unverified` | A previous owner was lost and its replacement has yet to verify the position. | Wait for the replacement to catch up. If it stopped, start following again. |
| `owner_missing` | A live follower or resync state has no lease owner. | Start following to verify the position. |
| `retrying` | The follower is retrying its source stream. | Restore access to the source and wait for the stream to reopen. |
| `capture_error` | A capture error is stored, including after a terminal stream failure leaves the state stopped. | Correct the reported error. Start following if stopped, or wait for the live owner to verify a checkpoint. |
| `catch_up` | Catch-up is still open, including after the follower stops. | Let the follower reach an exact checkpoint, or restart it from the saved position. |
| `restarting` | A live owner is reopening its stream from a verified exact checkpoint. | The checkpoint is healthy. Wait for the follower to be ready before write-back. |
| `starting` | The follower is opening its stream without a recorded healthy start. | Wait for the follower to reach an exact checkpoint. |
| `resyncing` | The owner reports a resync in progress. | Wait for it to finish. |
| `exact` | A live owner or a clean stop left a verified exact checkpoint at the published branch head. | No repair is needed. To apply later source writes after a clean stop, start following. |

`exact` and `restarting` are healthy and exit zero. An unhealthy verdict exits
nonzero in table and JSON output. `argon status` and `argon doctor` use the
same verdict for published followed projects. Write-back requires `exact`.

### Source changes that interrupt following

The stored capture error identifies what stopped the stream. Common causes
include the following.

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
    there shows a mixture. A live follower reports `catching_up` during this
    interval. Stopping it leaves the range open.
  - `--enable-source-images` (on `import database --online` or
    `source follow`) switches on `changeStreamPreAndPostImages` for the
    source's existing collections, so updates carry their own documents for
    as long as the oplog keeps them and no reads are needed. The option has
    its own experimental gate and requires `ARGON_EXPERIMENTAL_SOURCE=1`
    and a replica-set ID at import. Operators can enable
    `changeStreamPreAndPostImages` directly on MongoDB. MongoDB then stores
    a pre-image of every updated or deleted document in
    `config.system.preimages` until it expires with the oplog (or
    after `changeStreamOptions.preAndPostImages.expireAfterSeconds`): more
    writes and more storage on the source for every update and delete, a
    `collMod` on each collection, and the source user needs the privilege to
    run it. Argon never does this unless the flag is given, and does not
    undo it. If the import or follower subsequently fails, the error
    names the collections that were changed. Online import first reserves
    the project and binds the source. Before enabling images, `source follow`
    checks the binding, saved position and lease and opens the stream. If
    following was degraded, it waits for replay or a resync to clear the gap.
    It checks the lease before each collection and stops altering collections
    if it loses ownership. The `collMod` itself runs on the source.
    With separate deployments it can finish after a lease expires or
    another run takes over.
    Collections created later are not covered until the flag is given again.
    The setting applies to updates made after it is enabled.

  On MongoDB 6.0 an update cannot be read at its own cluster time once its
  collection has been altered afterwards: an index build, a `collMod`
  (enabling images is one), a rename or a drop. Updates the follower had not
  stored by then stop it, with a message that says so; `--allow-inexact` or a
  resync gets past them. On 6.0, enable images while the follower is caught
  up, not while a backlog is waiting. MongoDB 7.0 and 8.x serve such reads
  (checked on 7.0.45 and 8.3.17).

  A collection MongoDB does not serve at a past cluster time has no exact
  reads at all. A capped collection is one on MongoDB 7.0 and earlier (8.x
  serves it). Its updates are followed exactly only through attached images.
  Without images, an update stops following unless `--allow-inexact` is given,
  in which case Argon stores the document's current state. Adjacent inexact
  ranges with the same reason share one entry. Exact writes between such
  ranges keep the entries separate. When the list is full, Argon removes
  the oldest range with the same reason, or the oldest overall if none
  match, and counts the removal in `argon source status`.

Following also ends as `degraded`, rather than retrying for ever, when the
source keeps answering the same position with an error and nothing can be
stored in between: an update event too large to return (over 16 MB with its
images), a privilege the user lacks, a transaction too large to store in the
time allowed. An import in that situation fails with the error instead of
hanging. The follower also checks the expected branch head and source
position on startup and fences each write against concurrent changes.

A time-series collection created on a followed source ends following: its
measurements never reach a change stream as documents, so `main` could not
equal such a source, and a resync refuses it for the same reason. Online
import and following ignore view creation, modification and removal.
Collections named `config.*`, `admin.*` or
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
Before it records the resync, it opens and reads the change stream with
the replay's options and start position. If the source cannot serve that
stream, `main` stays untouched and no new unfinished resync is recorded.

For old-binary key errors on a followed branch, stop the follower and run
`argon source resync -p <project> --force`. The completed resync repairs
the collection at the consistent LSN it prints. Earlier reads that depend
on the missing history still fail. See [upgrading older snapshots](#upgrading-older-snapshots).

What it does to `main`:

- Resync stores the source BSON for each document whose bytes differ from
  `main`, whose capture was unverified, or whose WAL image is unavailable.
  Documents already held as verified, byte-identical images need no new entry.
- Every document `main` holds that the source no longer has is deleted.
  A collection dropped or replaced by a view loses all its documents on
  `main`. A renamed collection ends up under its new name. A dropped
  database leaves `main` empty. Resync verifies the source deployment
  against the binding recorded at import.
- `main` keeps its history. The state before the resync can still be read at
  its LSN, and sandboxes forked from it are not affected.

What it guarantees, and what it cannot:

- **Nothing is written to the source.** It issues reads only: one pass over
  every ordinary collection, and one `_id` lookup per batch of `main`'s
  documents in collections that still exist on the source.
- **The end is a state of the source.** `main` at the printed LSN is the
  source at the printed cluster time, also when the source was written
  throughout. Replay checks the branch head from the comparison before
  opening the stream. If another process moved the head, resync stops with
  the unfinished marker still set. Online import checks its copy's head in
  the same way and keeps the project unpublished on a mismatch.
- **The LSNs in between are not.** While it runs `main` moves batch by batch
  and is reported as `catching_up`; afterwards the range from the first LSN
  after the last consistent point to the printed LSN is listed by
  `argon source status` as inexact, with a reason that starts with `resync`.
  Time travel into it shows a mixture.
- **What happened on the source in the gap is not in `main`'s history.**
  Between the last change the follower applied and the resync, only the
  outcome is recorded. Resync writes a put or delete for each difference
  and recaptures documents whose earlier images were unverified. The gap's
  individual changes, order and times are unavailable. A write-back whose
  transaction fell into that gap is not attributed to its merge. If the
  writer had not marked the plan applied, it stays `writing_back`.
- The resync records its marker before the first write to `main` and keeps
  the old consistent point until replay completes. If the comparison restarts,
  the recorded range covers every attempt. A sandbox forked during this
  interval starts with the partially compared contents of `main`.
- **It fails rather than guess** when the source drops or renames another
  collection while it runs; run it again. A source that does this
  continuously (for example an aggregation with `$out`, which renames a
  temporary collection on every run) cannot be followed without repeated
  resyncs.

The comparison batches hold at most 500 documents by default and flush at
a 15 MiB budget for raw BSON and for the encoded metadata lookup keys.
Prune also bounds both the metadata key lookup and the source `_id` lookup
by encoded size to leave room for command overhead.
If garbage collection has partly reclaimed a collection's history on `main`,
prune lists its documents from the full snapshot, which is loaded into memory.
The source's oplog has to cover the duration of the resync, as for the import.

`argon source follow --auto-resync` checks its own experimental opt-in
(`ARGON_EXPERIMENTAL_SOURCE=1`) before starting. It completes interrupted
resyncs and repairs source stream gaps. Each resync reads the whole source
and records the range it writes as inexact. Automatic repair stops if
another resync would be needed within a minute.

A resync always needs the branch's lease. Stop a live follower first.
Without `--force`, resync accepts degradation, an unfinished resync, a
lost owner or a missing, changed or inconsistent checkpoint. After a
takeover, an unverified position is probed for resumability. If it can
be resumed, start following. Use `--force` for a full comparison even when
following can continue. The branch must be released and its project published.

### Writing a plan back to the source

`main` cannot be merged into while it follows a source, because it would then
hold documents its source does not. A reviewed plan is written to the source
instead:

```sh
argon merge preview -p myapp -b <sandbox> --output json
argon merge apply <plan-id> --write-back --approve <approval_digest>
```

Write-back tests have not covered Atlas, deployments with access control,
multi-member elections, sharded sources or MongoDB versions other than 7.0.

#### Ordinary apply versus write-back

| Apply target | What Argon checks | Accepted plans |
| --- | --- | --- |
| An Argon branch | Preview compares decoded BSON values with document keys sorted recursively. Apply checks the saved branch heads and, for formats that record them, branch versions. On a checked-out target, the transaction also compares each live document's decoded values with the plan. | Current plans, including sorted images. Ordinary replacements keep the last value of a repeated data field at its first position in the raw document. The first top-level `_id` supplies document identity. JavaScript code scopes preserve their field order and repetitions. A repeated field within a compound `_id` requires correcting the key first. Older formats can recover IDs and images from branch history. |
| A followed source | The transaction requires an absent key for each insert and exact saved target BSON for each replacement or delete. After all writes, it reads each key again and verifies the final BSON or absence. One mismatch rolls back all writes. | A plan saved by this version, its matching approval digest, and verified, unambiguous images for every selected write. |

#### Applying to the source

Inspect a saved plan with `argon merge list -p myapp --output json`, including
plans first opened through REST or MCP. CLI JSON shows the ordered images and
their exact-byte annotations. Pass the chosen plan's `approval_digest` to
`--approve`. Table output, REST, the console and MCP direct you to CLI JSON.
A missing approval or a digest for different contents leaves the source
untouched and the plan pending.

In 2.8.0, checkout rebuilt documents from Go maps and could reorder fields
before the first sandbox edit. Checkout and re-checkout now load the stored
BSON, including the order inside embedded documents and arrays. With the
imported image available, a sandbox starts with the source's field order.
Setting an existing field keeps its position, and pushing onto an array keeps
the existing elements in order, just as when MongoDB applies the same update
directly. The saved CLI JSON preview shows the exact field order that an
approved write-back will write.
If a 2.8.0 sandbox already captured an edit in a different order,
re-checkout reproduces that captured order.

Checkout uses a verified, typed WAL post-image or a snapshot frame when it
establishes the document's state at the requested position. If an older
capture repeats a data field, checkout keeps its last value at the field's
first position, including in documents inside arrays. As in decoded replay,
the first top-level `_id` supplies the BSON identity. Compound IDs and
JavaScript code scopes retain their original fields.

An untyped WAL entry, an image without a usable `_id`, or a capture marked
`attached_unverified` or `current` supplies decoded values with map fields
sorted by name at each depth. The same fallback applies when a historical
read succeeds but a later write to the key could have been reclaimed.
A snapshot from before 2.1.6 bounds possible deletions on its own branch.
An intact write at the exact fork position can still supply its field order.

New snapshots keep the BSON used for editing. Earlier snapshots sorted
ordinary fields by name. Checkout recovers their order from a retained WAL image or an earlier
ordered snapshot when the values match and the intervening history is intact. A
candidate with repeated data fields leaves the older frame's order in place.
MongoDB puts the top-level `_id` first. A subsequent exact capture can thus
record and write back a document whose fields are in name order.

- In the metadata snapshot that claims the plan, `main` has a verified exact
  checkpoint as of its reported
  source cluster time, and its follower binding and resume position are in
  place to record the outcome. A cleanly stopped follower can resume there.
  Production may already be ahead. The comparison inside the source transaction
  catches changes to planned documents since that checkpoint. Check
  [source status](#what-argon-source-status-reports-and-what-to-do) if admission fails.
- Write-back targets ordinary collections. Views are read-only, and a source
  with a time-series collection cannot be followed. Write-back checks
  collection options before claiming the plan. A capped target is refused
  with its first selected key. If a collection becomes
  capped after that check, its snapshot-read error aborts the transaction.
  The preflight also checks that an insert or replacement target exists.
  It never upserts. A collection dropped between this check and an insert
  can be recreated by the insert. Collection identity and intervening changes
  that restore the same document state are outside the comparison.
- Inserts and replacements write the saved sandbox BSON, with `_id` first.
  Each lookup uses the raw BSON `_id` from the image. Its type and bytes must
  match the saved key and every selected image for that document. Preview again
  if an older after-image has a field ahead of `_id`.
  Reclaiming either branch's history after preview leaves verified plan images
  usable. The comparison reads its before-image from the saved plan.
- Before claim, Argon refuses a top-level BSON timestamp `(0,0)` outside
  `_id` in a selected after-image. MongoDB replaces that value with the
  current time on write. Set the intended timestamp on the editable branch
  and preview again. A zero timestamp inside an embedded document or in
  `_id` is allowed.
- A saved replacement with identical before- and after-images is refused
  before claim. In the transaction each replacement must modify one row.
- Verification costs one extra source read per written document. If the
  server stores different bytes or a deleted document still exists, the
  transaction aborts. Argon releases the claim, records the reason in
  `write_back_last_error` and leaves the plan pending. You can retry the
  same approval or create a fresh preview.
- Before claiming, Argon reports all evidence problems by reason, with the
  total and up to five collection/ID pairs per reason. The plan stays pending.
  For a plan from any older writer, including format 3, preview again with
  this version. Inspect the new plan and its `approval_digest` in CLI JSON.
- An image in `sorted_images` means capture supplied unverified values or the
  original history was unavailable at preview. For a target before-image,
  stop the follower and run `argon source resync -p myapp --force`. Resync
  reads the source BSON and records it even if an unverified entry has identical
  bytes. For a sandbox after-image, record the intended document again with
  verified capture, for example with a programmatic `WriterFor` put on an
  editable branch. Preview again. A document containing duplicate field names
  must be corrected and captured again before write-back.
- Programmatic puts sort Go maps at every depth. Ordered documents, raw BSON
  values and structs retain their supplied order, including compound IDs.
- The encoded plan and room for its claim, outcome and failure text must fit
  in one 16 MiB BSON document. Split larger changes into smaller plans.
- If the plan has no writes, including when `ours` keeps every conflict,
  the same digest check and majority-acknowledged metadata transaction record
  its approval and mark it applied. There is no
  source transaction, source commit time or claim to settle. The CLI reports
  `written_to_source: false` in JSON.
- `main` is not written by the write-back. It receives the change through
  following, like any other change to the source, attributed to the merge
  (`actor: merge:<sandbox>`) with a `merge` record carrying the plan ID.
- The writer must receive a majority acknowledgment of the plan claim
  before starting the source transaction. It uses majority write concern to
  mark the plan applied after an acknowledged commit or return it to
  pending if it knows the transaction aborted or the commit was never sent.
  The follower marks it applied if it sees the transaction in the change
  stream. If neither records the commit, the plan stays `writing_back` with
  an unknown outcome. Session ID and transaction number identify the plan
  even when one source client applies several plans.
  A retry of a known abort clears the previous error as it claims the plan.
  The plan stores up to 4096 UTF-8 bytes of error text and up to 1024 bytes
  of the source display address. You cannot reuse an unsettled plan.
  Check the affected documents on the source, let following catch up, then run `argon merge preview` again for the sandbox.
- Claim and detach both write the follower binding in a metadata transaction.
  If detach wins, the claim fails. A `writing_back` claim less than five
  minutes old blocks detach. The metadata server timestamps each new claim
  and measures its age. The writer has two minutes from before the claim for
  the source transaction, including retries and commit. The five-minute
  window also covers the server's two-minute limit on an in-flight commit.
  Use the updated writer and detach commands together for this guarantee.
- After five minutes detach reports each unsettled plan ID. Inspect the
  source for those plans. They remain `writing_back` and retry stays refused.
  A follower stop or gap can prevent automatic settlement even while the
  binding remains in place.
- A fresh preview against a caught-up `main` omits changes that the earlier
  transaction already made and that the source still holds. It surfaces
  conflicts where both sides changed a document differently. Before writing
  the new plan, write-back still checks every planned document against the
  source's current state. If a source commit succeeds but Argon cannot mark
  the plan applied, the error reports the committed transaction and plan ID.
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
and the ability to run `hello`. Import with `replSetGetConfig` to record the
replica-set ID. Every later connection to an ID-bound source also needs
that privilege. `--write-back` also needs `readWrite` on the database.
`--enable-source-images` also needs `collMod`.

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

### Upgrading older snapshots

Argon 2.7.0 could store an empty BSON subtype-2 value and four zero bytes
under the same key, even inside a compound `_id`. A delete under that key
without a pre-image could remove both from the snapshot. If GC reclaimed
part of the collection's WAL through the snapshot LSN, Argon checks for
subtype-2 IDs of any length in the branch's and its ancestors' retained WAL,
in snapshots of that collection anywhere in the project, and in the
branch's own checkout if it has one. If any trace survives, reads that need
the suspect snapshot report the branch, collection and unknown LSN range.
Collections with ordinary IDs can continue using their old snapshots after GC.

An older import snapshot containing the key, or a later WAL entry for the
surviving row, is enough evidence. If every trace is gone, Argon accepts
the snapshot. For example, a followed branch can hold neither row even
though the four-zero-byte row still exists on the source. Stop the follower
and run `argon source resync -p <project> --force` to repair current
`main`. Earlier positions can still return a result missing the row.

On an ordinary branch, check a readable position before the affected
history with `argon time-travel query -p P -b B --lsn N`. Fork from it with
`argon restore branch -p P -b B --lsn N --as recovered` to keep the original
head, or rewind that branch with `argon restore reset -p P -b B --lsn N`
after releasing any checkout. If the good position predates the branch,
fork from its parent at that position. To start from an authoritative
database, pause its writers and DDL, then run
`argon import database -p NEW -d DB --uri URI --source-quiesced`.
This copies the database into a new project. The original branch keeps its history.

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
