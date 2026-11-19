# Changelog

All notable changes to the Argon project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Planned: GCS chunk-store backend; an opt-in read-your-writes barrier in
the wire proxy (hold a write's ack until the ingester confirms the WAL
entry — a synchronization barrier, not in-proxy capture).

### Changed

- As of this release, `merge apply --write-back` no longer requires
  `ARGON_EXPERIMENTAL_SOURCE=1`. The experimental options `--auto-resync`
  and `--enable-source-images` remain off by default and still require it.
- Format-3 merge plans store ordered BSON images. CLI preview and list
  identify literal documents with dollar-prefixed fields, unverified images
  and exact BSON bytes that canonical Extended JSON cannot represent.
  Table output reports how many paths require exact bytes.
- Write-back requires a verified, saved before-image for every
  replacement or delete and compares it byte for byte in the source transaction.
  Inserts require an absent key. Inserts and replacements write the saved
  after-image. After all writes, the transaction reads each key once more
  before commit to verify the exact stored BSON or confirm deletion. A
  mismatch rolls back the batch and leaves the plan pending.
- Write-back checks the raw BSON type and bytes of every selected key.
  Followed plans save after-images with `_id` first. Selected images with
  unverified bytes, duplicate fields or inconsistent keys require a fresh
  preview. A selected after-image with a top-level zero timestamp outside
  `_id` is refused because MongoDB replaces it with the current time.
- `merge apply --write-back` requires `--approve <digest>`, including for
  empty plans. CLI JSON preview and list provide the digest alongside the
  saved images. The claim verifies the approved contents and records the
  digest. A plan needs an assigned ID to receive a digest. An explicit BSON
  null auxiliary key survives reload. Plans saved by older writers require a fresh preview.
- Ordinary merge preview refuses a compound `_id` that repeats a field name.
  Version 2.7.0 merged it.
- Source detach refuses recent write-back claims and reports older unsettled
  plan IDs for inspection. Source transaction and commit retries share a
  two-minute writer deadline.

- CLI JSON output for `merge preview` and `merge list` uses canonical
  Extended JSON v2 for document IDs and images. For example, an int64
  field formerly printed as `"n":1` now prints as `"n":{"$numberLong":"1"}`.
  NaN and infinities can be previewed and listed.
- New programmatic WAL writes sort fields supplied in Go maps, including
  nested maps, and preserve caller-supplied BSON order. Existing entries
  retain their stored bytes.

### Fixed

- Reads that depend on a historical image-less delete under the shared
  empty/four-zero subtype-2 key now fail with the branch, collection and
  key. A forced source resync restores the collection at its consistent LSN.
  Re-import the collection on an ordinary branch. Snapshots that may have
  collapsed those IDs are skipped.
- Write-back refuses a capped target or a byte-identical replacement before
  claim. A replacement that modifies zero rows aborts the transaction. A
  capped-collection snapshot error ends retries and reports the server error.
- Following attributes and settles separate write-backs on one source client
  by session ID and transaction number.
- Stored BSON subtype-2 values with conflicting inner and outer lengths
  now fail document decoding, ordinary merge and quiesced import. Version
  2.7.0 could reject them or truncate the binary and interpret trailing
  bytes as another field. Raw WAL reads, online import and capture preserve
  the original bytes of non-key fields. Short historical lengthless values
  remain readable. The wire proxy reports a malformed command when it
  encounters conflicting full subtype-2 lengths.

- Ordinary merges into WAL-backed and checked-out branches collapse repeated
  names in documents and embedded documents, including array elements. The
  last value occupies the data field's first position in the raw image.
  The first top-level `_id` supplies document identity. JavaScript
  code scopes preserve their field order and repeated names.
- Reads of old BSON binary (subtype 2) preserve the stored payload length.
  A WAL image with an empty payload now decodes to zero bytes. Version
  2.7.0 decoded those same bytes as four zeros and could persist the four
  zeros during an ordinary merge. Images that already store four payload
  bytes still read as four. Historical lookups distinguish empty and
  four-byte IDs by their image when an older index key matches both.
  CLI merge JSON now renders a valid empty subtype-2 value without an
  `exact_bytes` annotation.
- REST merge preview, list, get and branch diff render non-finite doubles
  as Extended JSON `$numberDouble` values. A plan encoding error returns a
  JSON error body with HTTP 500.

- Ordinary merges preserve verified BSON order for current documents in a
  collection that also contains older WAL keys.
- CLI merge JSON escapes legacy DBPointer namespaces, including quotes.
- Source resync recaptures unverified documents even when their bytes match
  the source, allowing a fresh preview to record verified before-images.

- Saved plans use the stored BSON key for compound IDs on both apply paths.
  Preview gives target-specific apply guidance and identifies plans too
  large to leave space for claim and outcome records under MongoDB's 16 MiB limit.
- Both apply paths reject unknown strategies up front. Empty write-backs
  complete in metadata, and a new attempt clears the previous write-back error.
- Write-back verifies capture health in the claim transaction and refuses
  unverified sandbox replacements. The follower reads claims from the
  metadata primary and looks again for each new source transaction.

- Source and deployment status stay healthy while a follower reopens its
  stream from a verified exact checkpoint. Write-back still requires
  `exact`. Status confirms apparent lease expiry in a fresh snapshot.
- Followed checkpoints record a digest of the complete resume position so
  status and follow detect a replaced token even at the same cluster time.
- Deployment health skips unpublished imports before loading their checkpoints.
- Documentation now gives the default wait for the last change on an idle
  source as ten to twenty seconds. The reported time to store it includes
  that wait.

### Known limitations

- If a collection on a followed branch used deprecated BSON binary subtype 2
  for both an empty `_id` and one containing four zero bytes, a read at an old
  position can omit a surviving row after history written by 2.7.0 has been
  garbage-collected. Stop the follower and run
  `argon source resync -p <project> --force` to repair the current state.

## [2.7.0] - 2026-11-18

Online import and following a source database are available by default.
Write-back, automatic resync and enabling images on the source stay
experimental behind `ARGON_EXPERIMENTAL_SOURCE=1`. Projects that use none of
them have no data format changes since 2.6.0.

### Changed

- Online import and following no longer require `ARGON_EXPERIMENTAL_SOURCE=1`
  in this release. Source status, manual resync and detach are also available
  by default. Three options remain experimental and off by default behind
  that switch. `--write-back` writes reviewed changes to the source,
  `--enable-source-images` alters its collections, and `--auto-resync` can
  start a full comparison and record an inexact history range automatically.
- One follower or resync per project. A run takes a lease in Argon's
  metadata, judged by the metadata deployment's clock, and every write it
  makes is conditional on still holding it. A second `source follow` or
  `source resync` ends at once and names the run that holds the lease
  instead of marking the project degraded. Expiry revokes the lease even
  if no other run has taken over. The owner stops, and `argon source status`
  shows the expired lease and the last saved position.
- A write-back claim is settled only on evidence: the writer knows its
  transaction's outcome, or following sees the transaction. The follower no
  longer returns a claimed plan to pending after a waiting time. A plan that
  stays `writing_back` is not written again; check the source, let following
  catch up and preview a new plan, which finds nothing left to write if the
  earlier transaction committed. `merge list` prints this guidance.
- The source identity recorded at import is kept. A project bound by set name
  and member hosts is no longer upgraded to a replica-set ID by a later
  connection. Such a project can be followed and resynced, but write-back
  and `--enable-source-images` are refused for it: import as a user allowed
  to run `replSetGetConfig` (for example with the `clusterMonitor` role).
- `import database --online --dry-run` runs the online import's own checks
  (replica set, collection rules, the change stream the replay will open) and
  lists what an online import would copy. `import preview` skips views, and
  `import preview --online` applies the online collection rules.
- `source follow --auto-resync` checks the switch at startup and completes
  any interrupted resync. During following it resyncs only when the source's
  stream can no longer be followed. A head moved by something other than
  the follower needs an explicit `source resync`.
- Resync bounds document batches and both metadata and source lookup
  selectors by encoded size as well as by count.
- `connect` returns a connection string only for the branch's own physical
  database, and the console omits it for a sandbox whose record names
  another database.
- Operators can enable `changeStreamPreAndPostImages` directly on MongoDB
  or use the experimental `--enable-source-images` on online import or follow.
- Source status reports a stable health reason, the evidence for it and the
  next action in table and JSON output. Deployment status, doctor,
  write-back preflight and follow and resync admission use the same verdict.

### Fixed

- A second resync started while one was running replaced the first one's
  replay position; the first could then finish as exact with changes
  missing.
- A committed write-back whose record was not updated could be returned to
  pending after a resync or a long pause and applied a second time. A
  successful write-back that could not mark its plan now returns an error.
- A deployment presenting the same set name and member host strings as the
  source could be adopted as the project's identity, after which the real
  source was refused.
- Dropping, creating or modifying a view on the source ended following as
  degraded. Views are ignored. A resync now removes from `main` the
  documents of a collection the source has replaced by a view of the same
  name.
- A second follower left the project reported as degraded while the first
  kept following correctly. A resync that failed its first checks reset a
  degraded state to stopped, which allowed write-back again.
- A followed `main` checked out by Argon 2.5.0 was reported as healthy while
  following could not continue. It is reported as degraded; release its
  physical database, then resync.
- `source follow --enable-source-images` altered the source's collections
  before refusals it could have known beforehand. It alters nothing until
  following can start.
- Write-back reordered the fields of untouched array elements when an element
  before them was added or removed.
- The check of the change stream before an online import's copy did not use
  the replay's options, so a source that rejects them failed only after the
  copy.
- Repeated updates to a capped collection pushed the import's and resyncs'
  inexact ranges off the list `source status` shows. A recurring cause now
  displaces its own oldest range first.
- A status write from a follower or resync could recreate the state of a
  project that had been detached, or change the capture status of a branch
  checked out afterwards.
- A branch head moved outside the follower is reported as degraded by
  source and deployment status. An ordinary resync can repair it, and
  write-back refuses it.
- Deployment status also fails for unfinished resyncs and expired source
  leases.
- Write-back requires majority acknowledgment for plan claim transitions.
  A failed before-image history read aborts before the plan is claimed.
- Write-back preserves exact matches among equal-valued array elements
  with different BSON field order.
- Online import and resync accept documents with an explicit BSON null
  `_id`. Following tracks inserts, updates and deletes of those documents.
- The follower checks its lease before enabling images on each collection.
  If its lease expires, the error identifies which collections changed.
- Taking over an expired lease keeps source and deployment health failing
  and blocks write-back until the replacement verifies a checkpoint. Status
  retains the last saved position and consistent point during recovery.
- Resync and online import verify the head written by the preceding batch
  before replay. A head moved by another process fails the handoff.
- Source and deployment health fail while following reports a retry with a
  capture error. The follower keeps retrying from the saved position.
- Deployment capture health skips projects until their imports publish.
- A stopped follower with an open catch-up range or a stored capture
  error now reports unhealthy. A clean stop at a verified exact checkpoint
  still exits zero. Canceling a stream open or retry keeps its stored error
  visible until the stream reopens.
- Lease expiry is detected in every follower state, including when resync
  preflight acquired the lease while the previous report still said stopped.
- Each online copy batch checks the head left by its predecessor, starting
  at the branch creation LSN. An unexpected head aborts the unpublished import.

## [2.6.0] - 2026-11-18

The first diff after a checkout no longer scans the copy, and three experimental
features arrive switched off: online import, following a source database, and
write-back. No data format changes since 2.5.0 for projects that do not use
them.

### Added

- Online import is experimental and off by default behind
  `ARGON_EXPERIMENTAL_SOURCE=1`. `argon import database --online` imports a
  replica-set database while it is being written. The copy is followed by the
  source's change stream until the project equals the source at one cluster
  time, which the command reports with the LSN it corresponds to. Writes made
  during the import are not lost and not blocked. `--source-quiesced`
  imports are unchanged.
- The `argon source` commands are experimental and off by default behind
  `ARGON_EXPERIMENTAL_SOURCE=1`. A project imported online keeps its `main`
  branch tied to the source. `argon source follow` applies the source's
  changes and resumes from its saved position after a restart without
  skipping or repeating one. `argon source status` reports the position, the
  newest point at which `main` equalled the source, and which ranges of its
  history are not exact.
  `argon source detach` ends the tie. Nothing is written to the source.
  Updates are stored as the source held them, read back at their own cluster
  time. A position the oplog no longer holds, a dropped or renamed
  collection, and an update older than MongoDB's snapshot history are
  reported as degraded; `--allow-inexact` converges past the last of these
  and records the range, and has to be given on each run. The source may be a
  different deployment from the Argon metadata. Following also ends as
  degraded, instead of retrying without end or carrying on, when the source
  keeps answering one position with an error (an event too large to return,
  a missing privilege), when a time-series collection is created on the
  source, and when something other than the follower wrote to `main`. A
  follower's writes are conditional on the branch still following its source,
  so `source detach` stops a follower in another process as well.
- The source commands, write-back and `--enable-source-images` are
  experimental and off by default behind `ARGON_EXPERIMENTAL_SOURCE=1`. A
  source is identified by its replica-set ID when the importing user may
  read it, and otherwise by its set name together with its member hosts; a
  matching set name alone is never accepted, for following, resync,
  write-back or `--enable-source-images`. `argon source status` shows which
  applies.
- Resync is experimental and off by default behind
  `ARGON_EXPERIMENTAL_SOURCE=1`. `argon source resync` repairs a project whose follower ended as
  degraded, in place, instead of importing the database again: after a
  collection was dropped or renamed on the source, after the database was
  dropped, or when the oplog no longer holds the saved position. It compares
  the source with `main` in batches, stores what differs, deletes what the
  source no longer has, replays the changes made meanwhile and reports the
  cluster time at which `main` equals the source again. Nothing is written to
  the source; the LSNs it writes are recorded as an inexact range; an
  interrupted resync is reported as unfinished, blocks `source follow`, and
  is completed by running it again. `argon source follow --auto-resync` runs
  it when following ends as degraded and continues. Before it writes
  anything it opens the source's change stream once, so a source user who
  may not read it is told at the start, and it records its start only while
  the project still follows the source, so a detach in that instant leaves
  nothing behind.
  The consistent point reported on a source nobody is writing to is now the
  source's current cluster time; it could be one tick later, a time a later
  write might still be given. Dropping a collection Argon does not follow
  (`system.*`) no longer ends following.
- Write-back is experimental and off by default behind
  `ARGON_EXPERIMENTAL_SOURCE=1`. `argon merge apply <plan> --write-back` writes
  a reviewed plan to the database its target follows, in one transaction that
  compares every document with the plan's before-image and writes nothing if
  one differs.
  It happens only when the flag is given on the call; the MCP server and the
  REST API do not offer it. It writes single documents into collections the
  source already has and never creates one; a replaced document keeps the
  field order production gave it; a document whose fields were reordered
  since the review counts as changed; and it is refused while following is
  degraded. A plan whose write-back was interrupted before it committed is
  pending again.
- `--enable-source-images` is experimental and off by default behind
  `ARGON_EXPERIMENTAL_SOURCE=1`. `--enable-source-images` on `import database
  --online` and `source follow` switches on `changeStreamPreAndPostImages` for
  the source's collections.
  Argon does not alter a source's collections without it, makes the change
  only once the import or the follower can actually start, and names the
  altered collections in the error if the import then fails.
- Online import is experimental and off by default behind
  `ARGON_EXPERIMENTAL_SOURCE=1`. An online import copies and follows every
  collection of the source except `system.*`, including ones named `config.*`,
  `admin.*` or `local.*`. It skips views and refuses time-series collections.
  A failed or cancelled import, and `argon import cleanup`, remove the
  snapshots taken of the unpublished project as well as its rows.

### Changed

- The first `argon diff` or `argon merge preview` of a new branch no longer
  takes time that grows with the database when the parent is checked out,
  has a saved capture actor and nothing is watching it. Both drain the
  parent's capture first, and the parent's stream position was from before
  the branch's checkout, so MongoDB scanned the oplog of that whole copy.
  A checkout (and a sandbox, which includes one) now reads a checked-out
  parent's stream while it copies and leaves the parent's position after the
  copy. A parent that is not checked out, or whose capture cannot start, is
  left as before (#120).
- A branch that follows an external database accepts changes only from its
  source: `merge apply` without `--write-back`, `undo`, `restore reset`,
  `checkout` and programmatic writes on it are refused, with the alternatives
  named. Projects imported with `--source-quiesced` or created empty are not
  affected.
- Database drops go through one function that accepts only the database
  Argon created for the branch. Release, discard, the sandbox sweep, capture,
  live merge and undo, and the wire proxy refuse a branch record that names
  any other database as its physical database.
- Source connection strings are removed from import errors before they are
  printed, and `--uri` may be given as `ARGON_SOURCE_URI`.
- `checkout` of an already checked-out branch whose record names a database
  other than the one Argon created for it is refused instead of returning
  that database's connection string.
- The README, the npm package description and the MCP Registry description
  name what is underneath a sandbox as Git for MongoDB. The README sets seven
  Git commands beside the Argon command that does the same job and says where
  the comparison stops. "MongoDB sandboxes for AI agents" remains the first
  line everywhere.

### Fixed

- Checking out a child branch no longer assigns the default capture actor to
  a parent without a saved actor. A later `argon watch -b main --actor etl`
  can choose its actor and capture the parent's pending writes under that
  label.

## [2.5.0] - 2026-11-17

Diff and merge preview do work in proportion to what a branch changed. No
data format changes since 2.4.1.

### Changed

- `argon diff` and `argon merge preview` do work in proportion to what the
  branch changed, not to the size of the database. A plan used to rebuild
  three complete states (the fork point, the branch and its parent) and compare
  every document. Only a document the branch wrote since its fork can be part
  of a plan, so the plan is now built from those documents: their state at the
  fork, on the branch and on the parent is read from each document's own
  history. The plans are the same, and the REST API, the MCP tools and the
  console use the same path. Two cases still read a whole collection. When
  `argon gc` has deleted the parent's entries, the state of a changed document
  at the fork and on the parent comes from the parent's snapshot, which is
  loaded for each. When it has deleted entries of the branch itself, or the
  history holds entries from before 2.1 that do not identify their document,
  or the branch holds more than 200,000 entries of its own, the plan is
  computed the previous way.

## [2.4.1] - 2026-11-17

Sandboxes and checkouts work on MongoDB Atlas free clusters. No data format
changes since 2.4.0.

### Fixed

- Sandboxes and checkouts work on MongoDB Atlas free (M0) clusters. Checkout
  read its capture boundary from the reply to `ping`, which a free cluster
  answers without `operationTime`, so `argon sandbox create` and
  `argon checkout` failed with "missing operationTime" although `argon doctor`
  passed. The boundary now comes from `hello`.
- `examples/boxd-sandbox`: the host script reviews a sandbox the agent wrote
  nothing to instead of stopping with a TypeError.
- Engine warnings go to stderr. A failed storage reclaim after a branch was
  deleted, a failed post-import snapshot and a branch that could not be deleted
  with its project were reported on stdout. Under `argon mcp` stdout is the
  protocol stream, and with `--output json` it is the JSON value, so such a
  warning corrupted both.

### Added

- `examples/boxd-sandbox`: agent-written code runs in an isolated boxd machine
  and writes to an Argon sandbox through a MongoDB user restricted to that
  sandbox's database; the host prints the diff and discards the sandbox, or
  merges it with `--apply`.
- `examples/openai_agents_sandbox.py`: an OpenAI Agents SDK agent changes one
  account in an Argon sandbox through `argon-agents`; the script prints the diff
  and discards the sandbox, or merges it with `--apply`. `--no-llm` runs it
  without an API key.

### Changed

- CI's audit of the console's dependencies can excuse an advisory by name. The
  whole tree is still audited, a production install must be clean, and an
  exception ends as soon as npm offers a compatible fix. One exception is
  recorded: GHSA-fc5q-39xc-vjpf in picomatch, a build-time dependency of vite
  for which no patched release exists.

## [2.4.0] - 2026-11-16

JSON output for `gc` and `restore`, and a restarted MCP server resumes capture.
No data format changes since 2.3.3.

### Added

- `argon gc` and `argon restore preview|reset|branch` accept `-o json`.

### Fixed

- A restarted `argon mcp` resumes capture for the branches an earlier server
  left checked out, as soon as it connects. Before, capture resumed only when a
  tool next drained the branch, for example `argon_diff`. Writes made in
  between were caught up then, but a change to a collection the agent had
  created could by that time be outside MongoDB's snapshot history (five
  minutes by default), and the drain reported the history as incomplete. The
  REST API already re-attached capture at startup; the MCP server now does the
  same.

## [2.3.3] - 2026-11-12

Fixes a regression in 2.3.2. No data format changes since 2.3.2.

### Fixed

- `argon mcp` sweeps expired sandboxes again when no tool is called. 2.3.2 made
  the server connect to MongoDB on the first tool call so that it could start
  without a database; a server that was restarted and then only listed its
  tools, or sat idle, therefore never connected and never swept. It now reaches
  for the database in the background from the moment it starts and keeps
  trying until it answers. Starting without a database, and listing tools
  without one, work as in 2.3.2.

## [2.3.2] - 2026-11-12

The MCP server starts without a database, and capture corrects a post-image
MongoDB attaches wrongly. No data format changes since 2.3.1.

### Changed

- `argon mcp` starts without a reachable MongoDB. The handshake, `ping` and
  `tools/list` no longer need a connection, so an MCP client or a directory
  that only lists the server's tools can start it anywhere. The server connects
  when a tool is first called. If MongoDB is not reachable then, the call
  returns a tool error that names `MONGODB_URI` and the replica-set
  requirement, the server stays up, and the next call tries again; an attempt
  waits at most ten seconds for MongoDB to answer. Capture for the sandboxes it
  hands out and the sweep of expired ones begin with that first connection.
  Until now the command exited at startup with `failed to connect`, before
  answering any request. With MongoDB reachable nothing changes except that the
  connection is made on the first tool call. For Go embedders this is
  `mcpserver.NewLazy` with `walcli.Connect`; `mcpserver.New` is unchanged.

### Fixed

- On MongoDB 7.0 and 8.x, the post-image attached to an update that moves a
  field names that field twice: at its old position with the old value and at
  the end with the new one. Capture stored that image as it came. It now reads
  the stored document back in its place and marks the entry `capture_images:
  snapshot`. Inside a transaction, or when MongoDB's snapshot history no longer
  covers the change, the attached image is kept as before and marked
  `capture_images: attached_unverified`; decoded values were and remain
  correct, because the newer occurrence comes last.

## [2.3.1] - 2026-11-12

Collections an agent creates are captured exactly. No data format changes
since 2.3.0; recovered entries carry one extra metadata field.

### Fixed

- A collection created by the application no longer breaks capture when one of
  its documents is updated or deleted before capture has enabled images on the
  collection. Such a sandbox previously stopped with `exact post-image
  unavailable` and could not be reviewed. Capture now reads the missing states
  back from MongoDB at the event's cluster time, so the recorded images are the
  stored documents byte for byte; the entry is marked `capture_images:
  snapshot`. Deletes and replaces in that interval gain their pre-images the
  same way, which makes them undoable. Two cases still stop capture with a
  reported gap: an update older than MongoDB's snapshot history (five minutes by
  default) when it is captured, and an update that is followed by another change
  to the same document in the same transaction, before images were enabled.

## [2.3.0] - 2026-11-12

A sandbox in one command. No data format changes since 2.2.0.

### Added

- `argon sandbox create --watch [--actor <label>]` creates a sandbox and
  captures its writes in the same command. Capture is running before the
  connection string is printed, and the command stays in the foreground until
  interrupted. With `--output json` it reports `capture_managed: true`.
  Without `--watch` the command behaves as before.

### Changed

- The README, quick start and agents guide lead with the sandbox workflow and
  state two limits next to it: a collection the agent creates needs
  `argon collections prepare` before its first update or delete, and a sandbox
  is a full physical copy whose readiness time the benchmark suite measures.
- The npm package description and keywords describe Argon as MongoDB
  sandboxes for AI agents.

## [2.2.0] - 2026-11-11

MCP tool restrictions and annotations, a verified npm install, and lighter
checkout and history reads. No data format changes since 2.1.6.

### Added

- `argon mcp --disable-tools` and `--read-only` restrict what an agent can do
  through the MCP server. `--disable-tools argon_merge_apply` keeps applying a
  merge plan a human step; a withheld tool is neither listed nor callable.
- MCP tools declare titles and behaviour annotations (read-only, destructive,
  idempotent), so clients can ask before `argon_merge_apply`, `argon_undo` and
  `argon_sandbox_discard`.

### Changed

- `argon checkout` (and sandbox creation) materializes and loads one
  collection at a time instead of holding the whole branch in memory. The
  database is the same state at the same LSN; peak memory is the largest
  collection rather than the sum of all of them.
- Time-travel info — shown whenever the console opens a branch — is read from
  the index instead of loading every WAL entry of the branch with its
  document images.

### Fixed

- The MCP server reported its version as `2.0` and always answered the
  handshake with protocol revision 2024-11-05. It now reports the real
  version and negotiates 2024-11-05, 2025-03-26 or 2025-06-18.

### Security

- The npm installer verifies the binary it downloads. Each published package
  now carries the SHA-256 digests of that release's binaries, and
  `argonctl`'s install script refuses to run a download that does not match.
  Until now the binary was fetched from the GitHub release and executed with
  no integrity check beyond TLS, so the registry's own package integrity did
  not extend to it.
- The release workflow installs the exact tarball it is about to publish —
  downloading and verifying the real binary — before publishing it, and its
  actions are pinned to commit SHAs like the rest of CI.

## [2.1.6] - 2026-11-11

Security and data-correctness fixes. Read the upgrade notes marked below if
you run `argon proxy` for remote clients, reach the console under a
non-loopback hostname, or use `restore reset` together with GC.

### Security

- `argon proxy` now listens on `127.0.0.1:27018` by default instead of every
  interface. The proxy does not authenticate clients, so the previous default
  exposed the upstream MongoDB — typically an unauthenticated development
  instance — to the local network. A non-loopback `--listen` address is refused
  unless `--allow-remote` is passed. **Upgrade note:** remote clients of the
  proxy need `--listen <addr> --allow-remote`, with authentication enabled on
  the upstream deployment.
- Reject malformed wire-protocol frames in the proxy instead of crashing. A
  16-byte message from any client that could connect ended the process;
  frames are now length-checked before parsing, a bad frame closes only its
  own connection, and the parser is covered by a fuzz test.
- A control plane without `ARGON_API_TOKEN` (the `argon console` default)
  validates the `Host` header and answers only to `localhost`, `*.localhost`
  and loopback addresses, closing a DNS-rebinding path through which a web
  page could drive the local API. Set `ARGON_ALLOWED_HOSTS` for tunnels or
  reverse proxies that use another hostname.
- Ignore `X-Forwarded-For` unless `ARGON_TRUSTED_PROXIES` lists the proxies
  allowed to set it, so the demo write budget cannot be reset by forging the
  header.
- Build with Go 1.26.11 to address
  [GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859): an HTTP/1 server
  using `MaxBytesReader` can panic when a handler reads the request body while
  the response header is being written. The API server limits request bodies
  this way, so the scanner reports the fix as reachable; Argon's handlers read
  the body before responding, and no crash was reproduced. Source builds on
  the Go 1.27 line require 1.27.4 or newer.

### Fixed

- **Backup branches and pins survive repeated resets.** Resetting a branch
  twice with no write in between ended the second discarded range at the
  already rewound head. Views captured before the first reset — a `--backup`
  branch, a fork, a pin — then silently lost the history between the two
  reset targets. A range now ends at the branch's high-water mark. Ranges
  recorded by earlier releases are read with the corrected end, so affected
  backups and pins read correctly again after upgrading; no migration is
  needed.
- **A reset can no longer land inside discarded history.** Resetting to an
  LSN (or `--time`) that an earlier reset had discarded showed the discarded
  writes at the head until the next write hid them again, and the
  snapshot-accelerated read path could keep returning them. Such a reset is
  now refused with the nearest valid target. Discarded history remains
  readable and forkable up to the high-water mark — also immediately after a
  reset, which previously required a later write — so `argon restore branch`
  recovers it. If you reset into discarded history with an earlier release
  and still need those documents, fork that LSN.
- **Snapshots taken inside a discarded window serve only readers of that
  window.** A snapshot at an LSN inside an already discarded range holds the
  pre-reset history, but was treated as valid for everyone and could
  resurrect discarded documents; GC could then reclaim the entries that
  proved otherwise. Usability is now decided from what the snapshot actually
  left out.
- **GC keeps every LSN inside the retention window readable.** The reclaim
  cutoff was the retention boundary itself. A read just above it replays from
  the newest snapshot below, across entries GC had deleted, and returned a
  state missing those writes without an error. The cutoff is now the newest
  generally valid snapshot at or below the boundary, and the boundary accounts
  for entries whose timestamps are slightly out of LSN order.
- **Positions GC has reclaimed past are refused instead of misread.** A
  time-travel query, reset, fork, pin or undo that needed entries GC had
  deleted replayed across the gap and produced a state without those writes
  — after `restore reset`, as the branch's new head. GC now records how far it
  reclaimed per branch and collection (`wal_reclaim_floors`), reads that
  would cross that floor fail with `history reclaimed by garbage collection`,
  and those commands check before changing anything. Heads, forks, pins,
  positions on a retained snapshot and everything inside the retention
  window are unaffected. Floors are recorded from this release on.
- `argon restore reset --backup` validates the target before creating the
  backup branch, `restore preview` fails the way the reset would and no longer
  counts entries an earlier reset already discarded, and `time-travel query`
  without `--collection` rejects an LSN beyond the branch's history like the
  single-collection form.

### Changed

- Resolving `--time` reads one index entry instead of loading every WAL entry
  of the project up to that time.

### Removed

- A 14 MB `cli/argon` executable that had been committed to the repository by
  mistake. It was a development build of the long-superseded v1 CLI (July 2025,
  Go 1.24) and was never part of a release, but it shipped in every source
  archive. Released binaries come only from the release workflow.

## [2.1.5] - 2026-11-09

### Security

- Update `github.com/klauspost/compress` to v1.20.2 for
  [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865), a decoder panic on
  crafted Zstandard frames. The Go toolchain requirement is unchanged.

### Fixed

- Accept an explicit `--lsn 0` for restore preview, branch, and reset when the
  branch permits that target, and reject requests that also supply `--time`.

## [2.1.4] - 2026-11-02

### Fixed

- Reject zero or negative sandbox TTL extensions before changing expiry. The REST
  endpoint returns HTTP 400 for invalid durations, preventing an invalid extension
  from making the sandbox eligible for the next cleanup sweep.

### Security

- Build with Go 1.26.10 to address
  [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809), which adds distrust
  of known compromised intermediate certificates in `crypto/x509`.
  Source builds on the Go 1.27 line require 1.27.3 or newer.
- Update the console build tool to Vite 8.3.5, removing the development-server
  and editor-launch advisories affecting Vite 8.3.0–8.3.2. Rebuild the embedded
  assets from the public source and lockfile; application source behavior and
  the CSS theme are unchanged.
- Run the required reachable Go scans with official govulncheck 1.9.0.

## [2.1.3] - 2026-10-19

### Security

- Build with Go 1.26.9 and update the API/CLI HTTP dependencies to
  `golang.org/x/net` 0.60.0, addressing the October 2026 reachable
  standard-library and HTTP vulnerabilities. Source builds on the Go 1.27
  line require 1.27.2 or newer.
- Remove vulnerable Tailwind 3 build dependencies and update source-map-js
  to 1.2.2. Full npm auditing and reachable Go vulnerability gates remain
  required.

### Changed

- The embedded console uses Tailwind CSS 4.3.3 with the existing brand theme,
  button affordances and keyboard focus styling. Its browser baseline is
  Safari 16.4+, Chrome 111+, and Firefox 128+.
- CI uses the same immutable MongoDB 7.0.45 image for every replica-set
  fixture and records container logs when startup fails.

## [2.1.2] - 2026-09-24

### Fixed

- REST branch creation drains the live parent's capture before fixing its fork
  point and refuses degraded history; omitted `from` defaults to `main`.
- Concurrent engine/CLI startup preserves the current unique WAL index instead
  of dropping and rebuilding it on every connection. Legacy definitions still
  migrate when needed.
- Canceling capture while enabling images for a new collection preserves the
  resumable boundary and stops cleanly instead of marking a permanent history gap.
- Go consumers use valid `github.com/argon-lab/argon/v2` and
  `github.com/argon-lab/argon/api/v2` modules, with separate matching release tags.
- Console Undo applies the exact actor and bounded LSN range just reviewed;
  changing scope invalidates review, including delayed preview responses.
- Expired/reset demo sessions offer a fresh start, clear stale queries and
  recover old deep links. Late old-session failures cannot end a renewed session.

### Import and release hardening

- Real imports require `--source-quiesced`: stop source writes and DDL for the
  entire copy. Preflight rejects unreadable/unsupported collections before
  creating target data. Imports remain hidden until successful completion.
- Failed or canceled imports remove their unpublished project/history so the
  name can be retried. `argon import cleanup --project P --yes` recovers an
  unfinished import after a killed process; completed projects are protected.
- Release publication depends on the complete CI suite for the exact tagged
  source, consistent committed version metadata and matching root/API tags.
  CI also compiles an external Go consumer of both v2 modules.
- The MIT console source is public in `web/`. Locked builds reproduce the
  embedded assets; browser regressions run against the same engine commit.
  Release provenance records engine/UI source and every binary checksum.
- Publishing and performance documentation now describes the actual release
  flow, source-quiescence contract and completed small-scale benchmark evidence.

## [2.1.1] - 2026-09-07

### Fixed

- Fix a monitor mutex deadlock that could leave `argon console` hanging on
  shutdown after its first health check. Alert updates now reuse the lock
  held by the health loop. Regression tests cover ticker shutdown, repeated
  failures and recovery.

## [2.1.0] - 2026-09-07

Correctness and workflow hardening for native MongoDB writes, reviewed
merges and undo. See the [release notes](docs/RELEASE_2_1.md) for regression
coverage and operating requirements.

### Added

- `argon doctor` checks connectivity, replica-set readiness, capture
  permissions, exact document images and transactions with temporary probes.
- `argon collections prepare` enables the document images needed to capture
  updates to new collections before application writes begin.
- Capture status includes persisted health, the last captured event and its
  observed delay. The delay is an event sample, not a live queue-lag estimate.
- Executable CLI and Python examples for pinned agent runs and native writes.

### Fixed

- Preserve BSON types in document keys. Repair legacy keys from authoritative
  document images and refuse ambiguous historical deletes.
- Capture exact document images for replay and undo. Missing update images or
  unsupported collection drop/rename operations mark history as degraded.
- Wait for durable capture readiness, retry transient failures, resume capture
  and drain acknowledged writes before release. Persist branch actor labels
  and trigger snapshots from native capture.
- Apply merge and undo changes transactionally with head and version checks.
  Reject stale plans and incomplete undo history before partial data changes.
- Coordinate snapshot publication and garbage collection across processes,
  and refuse reset while a branch is checked out.
- Make CLI JSON output usable by callers and validate installer behavior
  against published release assets.

### Security

- Disable native MongoDB checkout and sandbox credentials in the anonymous
  demo. Keep native-driver examples on the local deployment path.
- Bind control-plane listeners to loopback by default. Public listeners
  require explicit authentication outside restricted demo mode; cross-origin
  requests require an allowed origin.

### Changed

- Use one version source for the CLI, npm package and MCP manifest. Release
  automation publishes binaries before npm and the MCP Registry.
- Expand CI coverage for native-driver capture and undo, all Go modules,
  release metadata, installers and reachable dependency vulnerabilities.
  Source builds require Go 1.26.6 or newer.
- Document physical sandbox copying costs, retained-history requirements,
  branch/run actor attribution and capture lifecycle constraints. Benchmark
  reports distinguish metadata creation from full sandbox readiness.

## [2.0.1] - 2026-07-09

### Added

- `argon console` with an embedded browser UI, REST read endpoints and
  configurable server options.
- A scoped anonymous demo mode with request limits and sandbox cleanup.
- MCP Registry metadata and a GitHub OIDC publishing workflow. The npm
  package includes the server's `mcpName`.

### Fixed

- Recover managed capture after a control-plane restart.
- Include npm executable entry points in the published package and select
  the `.exe` release asset on Windows.
- Serve the console HTML without caching while caching hashed assets.

## [2.0.0] - 2026-07-07

The v2 engine: a ground-up rebuild around a deterministic physical WAL.
Every milestone of the rebuild plan shipped, each merged only with CI
green. See docs/ARCHITECTURE.md for the full design.

### Added
- **Deterministic replay** — WAL entries carry full document images
  (zstd-compressed); replay is a pure fold, property-tested (repeated,
  cross-instance, historical, cross-database convergence)
- **Distributed LSN sequencer** — per-project counters; correct under
  concurrent writers across processes
- **Snapshots** — content-addressed, deduplicated chunk layers that bound
  replay depth; automatic (per ~1000 entries and post-import) and manual;
  parallel chunk decode; MongoDB, S3 (MinIO/R2) and filesystem backends
- **Retention-window GC** — reclaims covered, out-of-window entries;
  respects live children's fork points and pins; full reclamation of
  deleted branches
- **Checkout: real MongoDB per branch** — `argon checkout` materializes a
  branch into a physical database any driver can use; `argon connect`,
  `argon release`
- **Change-stream capture** — `argon watch` turns direct driver writes
  into versioned history (resume tokens, transaction grouping, per-actor
  attribution); real-driver workloads (pymongo, mongoose) verified against
  WAL convergence in CI
- **Undo** — `argon undo` reverts LSN ranges or a single actor's writes
  with append-only compensations and conflict detection
- **Restore** — `argon restore preview/reset/branch`; resets record
  discarded ranges (recorded, not destructive), `--backup` forks first
- **Merge & diff** — three-way merges as persisted, reviewable plans
  (`argon diff`, `argon merge preview/apply`); exactly-once apply, stale
  heads refused, conflicts never silent
- **Agent sandboxes** — `argon sandbox`: fork + checkout + TTL in one
  step; sweep reaps expired sandboxes
- **Dataset pins** — `argon pin`: named immutable branch states that
  survive GC and resets forever; branch or sandbox from a pin for
  reproducible evals
- **MCP server** — `argon mcp`: 13 tools over stdio with supervised
  ingesters
- **REST control plane** — `api/`: projects, branches, checkout,
  sandboxes, diff/merge, undo, snapshots, pins; supervised ingesters
- **Wire-protocol proxy** — `argon proxy`: stable
  `mongodb://host/<project>~<branch>` connection strings
- **Import** — `argon import` brings existing databases in with automatic
  post-import snapshots
- **v1→v2 migration** — `argon migrate-wal` rewrites expression entries
  into deterministic document images, idempotently
- **argon-agents** (separate package) — LangGraph checkpointer with
  whole-store fork, Mem0 sandbox factory, REST client

### Changed
- All external performance claims now come exclusively from the
  reproducible public benchmark suite (argon-lab/benchmarks)
- Documentation rewritten around what is implemented and verified

### Removed
- In-process Mongo emulation (filter/update evaluation in Go) — mongod is
  the only query engine; expression evaluation survives solely for v1
  migration
- Unverifiable performance claims throughout docs and CLI output

## [1.0.0] - 2025-07-17, [1.0.1] - 2025-07-20

The v1 engine. Superseded wholesale by v2.0.0 and no longer supported;
`argon migrate-wal` converts v1 WAL data to v2. Published to Homebrew,
npm and (as `argon-mongodb`, now frozen) PyPI.

Earlier changelog entries claiming a 2024 release history, and the
performance/benchmark tables that accompanied them, were inaccurate — the
project's first commit is 2025-05-27 — and have been removed. Performance
numbers now live only in the reproducible benchmark suite
(https://github.com/argon-lab/benchmarks).

[Unreleased]: https://github.com/argon-lab/argon/compare/v2.4.1...HEAD
[2.4.1]: https://github.com/argon-lab/argon/releases/tag/v2.4.1
[2.4.0]: https://github.com/argon-lab/argon/releases/tag/v2.4.0
[2.3.3]: https://github.com/argon-lab/argon/releases/tag/v2.3.3
[2.3.2]: https://github.com/argon-lab/argon/releases/tag/v2.3.2
[2.3.1]: https://github.com/argon-lab/argon/releases/tag/v2.3.1
[2.3.0]: https://github.com/argon-lab/argon/releases/tag/v2.3.0
[2.2.0]: https://github.com/argon-lab/argon/releases/tag/v2.2.0
[2.1.6]: https://github.com/argon-lab/argon/releases/tag/v2.1.6
[2.1.5]: https://github.com/argon-lab/argon/releases/tag/v2.1.5
[2.1.4]: https://github.com/argon-lab/argon/releases/tag/v2.1.4
[2.1.3]: https://github.com/argon-lab/argon/releases/tag/v2.1.3
[2.1.2]: https://github.com/argon-lab/argon/releases/tag/v2.1.2
[2.1.1]: https://github.com/argon-lab/argon/releases/tag/v2.1.1
[2.1.0]: https://github.com/argon-lab/argon/releases/tag/v2.1.0
[2.0.1]: https://github.com/argon-lab/argon/releases/tag/v2.0.1
[2.0.0]: https://github.com/argon-lab/argon/releases/tag/v2.0.0
