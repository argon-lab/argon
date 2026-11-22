# CLI reference

Every command talks to `MONGODB_URI` (default `mongodb://localhost:27017`);
metadata lives in the `argon_wal` database. Shared flags: `-p/--project`,
`-b/--branch` (default `main`). Commands that advertise `-o/--output` support
`table|json`; unsupported formats fail before work begins. JSON produces one
value on stdout and diagnostics go to stderr. Every command whose help lists
`-o/--output` supports JSON. The former global `--api-key`, `--project-id` and `--config`
options were removed because they did not affect execution. Configure MongoDB
through `MONGODB_URI`; configure server auth through `ARGON_API_TOKEN`.

## Projects & branches

```
argon projects create <name>                   project + main branch
argon projects list
argon branches create <name> -p P [--from B]   a pointer; copies no documents
argon branches list   -p P
argon branches delete <name> -p P              refused for main, branches with
                                               live children, pinned branches
```

## Work with real databases

```
argon checkout -p P -b B      materialize into a physical MongoDB database,
                              print its URI (repeat preserves the live database)
argon collections prepare NAME -p P -b B  enable exact images before first writes
argon connect  -p P -b B      print a checked-out branch's URI
argon watch    -p P -b B [--actor A] capture writes with a persisted branch label
argon release  -p P -b B      drop the physical db; history stays
argon proxy [--listen 127.0.0.1:27018] [--allow-remote]
                              stable URIs: mongodb://host/<project>~<branch>;
                              non-loopback listeners need --allow-remote
argon console [--host 127.0.0.1] [--port 1818] [--no-browser]
                              local web console (REST API + UI)
```

While a branch is checked out, its history is fed by `watch` (or the
API/MCP servers' ingesters); SDK writes to it are refused — one source of
truth. `checkout` and `connect` return a connection string only for the
branch's own physical database.

A checkout also builds the secondary indexes recorded for its branch, so
unique constraints hold as on the source. A TTL index is built without its
expiry and an index the data violates is skipped; results report `indexes`
and `index_notes`. See [indexes in checkouts](OPERATIONS.md#indexes-in-checkouts).

## Import ("git clone")

```
argon import preview  --uri U --database D [--online]
argon import database --uri U --database D --project P --online [--follow [--allow-inexact]] [--enable-source-images] [--yes] [--dry-run]
argon import database --uri U --database D --project P --source-quiesced [--yes]
argon import database --uri U --database D --project P --dry-run
argon import cleanup --project P --yes   # only after stopping a crashed importer
argon import status -p P
```

Imports auto-snapshot, so reads never replay the whole import. A real import
needs one of two flags, and `--yes` replaces neither:

- `--online` (introduced in 2.6.0) imports a replica-set database while it
  is being written. It is available by default. The result is the source
  at one cluster time, which the command prints, and the project's
  `main` then follows that database (next section). `--follow` keeps following
  in the foreground once the import is done, and `--allow-inexact` (only with
  `--follow`) means what it means for `argon source follow`. Import reads
  the source by default. The experimental `--enable-source-images` requires
  `ARGON_EXPERIMENTAL_SOURCE=1` and switches on `changeStreamPreAndPostImages`
  for source collections. It needs an import by a user allowed to run
  `replSetGetConfig`, such as one with the `clusterMonitor` role. Operators
  can enable `changeStreamPreAndPostImages` directly on MongoDB.
- `--source-quiesced` is for a database nobody is writing to: pause its
  writers and DDL for the whole import. The result is a one-time copy.

Both preview modes skip views and estimate copy size. `import preview --online`
also includes ordinary collections whose names start with `admin.`, `config.`
or `local.`. `import database --online --dry-run` checks whether an online
import can start and reports its collections and estimated document count
without writing anything.

`--uri` can be left out when `ARGON_SOURCE_URI` is set. Source preflight
checks collection types and readability. Online preflight opens and reads the
stream with the replay's options and start position. Each online copy batch and
the start of replay check the expected branch head. A concurrent write to the
unpublished branch fails the import. Ordinary failures clean up the reserved
project and history.
After a hard kill, stop the old process and use `import cleanup` before
retrying the same name. Completed projects cannot be removed by cleanup. See
[the import contract](OPERATIONS.md#importing-existing-data).

## Follow a live database

The `argon source` commands and `merge apply --write-back` are available by
default. The experimental options `--auto-resync` and `--enable-source-images`
are off by default and each requires `ARGON_EXPERIMENTAL_SOURCE=1`. For known
issues and limits, see the [operations guide](OPERATIONS.md#following-a-source-database).

```
argon source follow -p P [--uri U] [--allow-inexact] [--enable-source-images] [--auto-resync]
argon source status -p P [-o json]
argon source resync -p P [--uri U] [--force] [-o json]
argon source detach -p P --yes
argon merge check <plan-id> [--uri U] [--strategy theirs|ours] [-o json]
argon merge apply <plan-id> --write-back --approve <digest> [--uri U] [--strategy theirs|ours]
```

A project imported with `--online` has a `main` that mirrors its source.
`source follow` applies the source's changes to it until interrupted and
resumes where it stopped. The connection string comes from `--uri` or
`ARGON_SOURCE_URI` on every command and is never stored. Each connection
must match the identity recorded at import. If the import could read the
replica-set ID, each connection must prove the same ID. Otherwise following
and resync match on the set name and at least one member host.

`merge check`, `--write-back` and `--enable-source-images` require a replica-set
ID, or a TLS-verified MongoDB Atlas connection (M0, Flex), recorded at import;
see [MongoDB Atlas as a source](OPERATIONS.md#mongodb-atlas-as-a-source). Separate deployments can report the same set name and member
hosts. For a project bound by name and hosts, detach it with
`argon source detach -p P --yes`, then import the source online into a new
project as a user allowed to run `replSetGetConfig`, such as one with the
`clusterMonitor` role.

`source follow --enable-source-images` checks the binding, saved position
and lease, then opens the stream before altering collections. The option
has its own experimental gate and requires `ARGON_EXPERIMENTAL_SOURCE=1`.
A degraded position must recover or be resynced first. If following
subsequently fails, the error names any source collections on which it enabled images.
The follower checks the lease before each alteration.

While `main` follows a source, the source is the only thing that changes it:
`merge apply` without `--write-back`, `undo`, `restore reset`, `checkout` and
programmatic writes on `main` are refused. Sandboxes forked from it work as
usual. To bring a sandbox's changes back, `merge apply <plan-id> --write-back --approve <digest>`
writes the plan to the source database in one transaction. It starts after
a majority-acknowledged metadata claim, and checks replacements and deletes byte for byte
against verified before-images in the plan. Inserts require an absent key.
After all writes it reads each key again to verify the stored image or deletion.
One mismatch aborts the transaction. `main` shows the result once following has read it back from
the source. The MCP server and REST API do not offer write-back.

`source status` reports the saved position and consistent point, inexact
ranges, lease owner, source identity and any write-back restriction. Table
and JSON output also give the health reason, message and next action. Use
`health.kind` and `health.reason` in scripts. The full report is available
even on a nonzero exit.

```sh
argon source status -p myapp -o json | jq '.health'
```

Exit zero means the published branch head is a verified exact checkpoint.
A clean stop at that checkpoint is healthy. Reopening the stream from it
reports `restarting` and also exits zero. Write-back requires `exact`.
An open catch-up range, a stored capture error or an expired owner makes
even a stopped branch unhealthy.
Canceling a follow attempt before the stream reopens preserves its capture
error. A successful stream open clears a transient or terminal open error.
After takeover, the replacement must verify an exact checkpoint at or beyond
the source time it observed on acquisition, or finish a resync. Deployment
status and doctor report the same health. See the
[source status reasons and actions](OPERATIONS.md#what-argon-source-status-reports-and-what-to-do).

`source resync` repairs degraded following in place, such as after a
collection was dropped or renamed on the source, after the source database
was dropped, when the source's oplog no longer holds the saved position,
or when another process moved `main`'s head.
It compares the source with `main` in batches, records changed documents
and recaptures unverified or unavailable images. It deletes from `main` what
the source no longer has, then replays intervening source changes. View creation, modification and removal are ignored,
and a view that replaced a collection counts as an absent collection during
resync. The command prints the cluster time and LSN at which `main`
equals the source again; `source follow` continues from there. It reads the
source and writes nothing to it. The LSNs it writes are recorded as an
inexact range. Without `--force`, it accepts degradation, an unfinished
resync, a lost owner or missing or inconsistent checkpoint evidence. If a resync is interrupted or
the source drops or renames another collection while it runs, `source status`
reports the unfinished resync and plain `source follow` refuses to start.
Run `source resync` again or restart with `source follow --auto-resync` to
complete it. `--auto-resync` has its own experimental check at startup
and requires `ARGON_EXPERIMENTAL_SOURCE=1`. It also repairs a gap in the
source stream before following again. It stops if another automatic resync would be needed
within a minute. If another process moves the branch head, follow stops as
degraded and requires `source resync` to be run explicitly.
If a replacement stopped before verifying a checkpoint, resync checks whether
MongoDB can still open the stream from the saved position. If it can, run
`source follow` to catch up. `--force` starts the full comparison immediately.

`source detach` ends following and turns `main` back into an ordinary branch.
An ambiguous old-binary delete reports the branch, collection and key.
When GC removed the WAL needed to check an older snapshot and subtype-2
ID evidence survives, reads that depend on it report the unknown LSN range.
Stop the follower and run `argon source resync -p <project> --force` to
recover the collection from the printed consistent LSN onward. On an
ordinary branch, check an earlier readable LSN with `argon time-travel query`.
Use `argon restore branch` to fork there or `argon restore reset` to rewind
the existing head. To start from a fresh copy, `argon import database`
imports the whole database into a new project and preserves the original branch.

A write-back claim less than five minutes old blocks detach. The error names
the plans to wait for. After that window, detach reports unsettled plan IDs
in the table output or in JSON with `unsettled_plans` and `warning`. Inspect
the source for each ID. Those claims stay `writing_back`. See
[following a source](OPERATIONS.md#following-a-source-database) for what is
guaranteed, what it costs the source and what stops it.

## History: time travel, undo, restore

```
argon time-travel info  -p P -b B
argon time-travel query -p P -b B --lsn N [-c collection]

argon undo -p P -b B --from-lsn N [--to-lsn M] [--actor A] [--dry-run]
    Revert a range by restoring pre-images — append-only, never rewrites
    history. --actor filters a recorded label; later unselected writes
    are conflicts and skipped. Missing images are reported as unrecoverable.

argon restore preview -p P -b B (--lsn N | --time RFC3339)
argon restore reset   -p P -b B (--lsn N | --time RFC3339) [--backup NAME]
    First stop writers and release a live branch. Rewind the head: discarded entries stay
    for audit; --backup forks the pre-reset head first.
argon restore branch  -p P -b B (--lsn N | --time RFC3339) --as NAME
    Fork the historical state into a new branch instead.
```

For restore commands, supply exactly one selector. Use `--lsn 0` when zero is
within the branch's valid restore range; it selects LSN zero, not the head.

A reset never changes what earlier forks, `--backup` branches and pins read,
however many resets follow. History that a reset discarded stays readable and
forkable: `restore branch` (or `time-travel query --lsn`) at such an LSN
recovers it. `restore reset` to an LSN inside discarded history is refused
with the nearest valid target; `--time` resolves to where the branch stood at
that moment, so the same applies when that moment was later discarded.

## Merge — data pull requests

```
argon diff          -p P -b B                  what merging B would change
argon merge preview -p P -b B                  persist a reviewable plan
argon merge check <plan-id> [--uri U] [--strategy theirs|ours] [-o json]
argon merge apply <plan-id> [--strategy theirs|ours] [--write-back --approve <digest>]
argon merge list    -p P
```

An ordinary apply requires the parent head saved by preview. Both apply paths
accept only `theirs` and `ours` when a strategy is supplied. Conflicts require
an explicit choice. For an ordinary target, table preview prints the apply
command. For a followed target, it gives the JSON inspection and approval steps.

In `merge preview --output json` and `merge list --output json`, document IDs,
keys and document images use canonical MongoDB Extended JSON v2. For example,
a BSON int64 count appears as `"count":{"$numberLong":"1"}`. Plan IDs, branch
names, counts of changes, timestamps and statuses keep their ordinary JSON
types. A BSON int32 and a literal embedded document can print identically.
For example, both a numeric `quantity` and a literal document `annotation`
can appear as `{"$numberInt":"1"}`. The change's
`"literal_dollar_paths":["document.annotation"]` identifies the literal
document. Each change or conflict lists every image document containing a
`$`-prefixed field. Paths start with `document`, `before`, `base`, `ours` or
`theirs` and use dotted array indices such as `document.items.0`. Literal
dots and backslashes in field names are escaped with a backslash. Code scopes
use `.$scope`. Empty lists are omitted. Table preview reports the path count.

Format 3 preserves BSON field order, including in nested documents. When two
stored images differ, their change JSON differs. For values that canonical
Extended JSON cannot reproduce byte for byte, `exact_bytes` maps an
image-prefixed path to the BSON type (one byte in hex) and the original value
bytes (hex). The same paths apply to conflicts.

| Stored difference | Shared JSON value | How to distinguish the original bytes |
| --- | --- | --- |
| Double NaN payloads 1 and 2 | `{"$numberDouble":"NaN"}` | The driver's default NaN (payload 1) needs no entry. Payload 2 appears as `"document.value":{"type":"01","hex":"020000000000f87f"}`. |
| Regex options `mi` and `im` for pattern `sample` | `{"$regularExpression":{"pattern":"sample","options":"im"}}` | For stored `mi`, `"before.value":{"type":"0b","hex":"73616d706c65006d6900"}` includes both terminating NUL bytes. |
| A string containing byte `ff` and a string containing U+FFFD | `"�"` | The invalid UTF-8 string has `"before.value":{"type":"02","hex":"02000000ff00"}`. The valid replacement character needs no entry. |

A repeated or invalid UTF-8 field name makes a path ambiguous. The image name
alone then maps to type `03` and the complete BSON document in hex. Empty
`exact_bytes` maps are omitted. Table preview and list give the count of
paths requiring exact bytes. Use `--output json` to inspect them.

A `sorted_images` entry identifies an image for which only decoded values
were available or an ordinary merge collapsed repeated field names. When a
raw image is available, ordinary merge keeps the last value at the field's
first position. The first top-level `_id` supplies document identity. JavaScript
scope documents keep their order and repetitions. Ordinary
apply can use these images and older formats. A repeated field within a
compound `_id` would change the lookup key if collapsed. Ordinary merge
refuses that document.
A stored subtype-2 binary whose inner length disagrees with its payload
prevents document decoding and ordinary merge preview. The error identifies
the document. Raw WAL image reads preserve the stored bytes.

Write-back requires verified before-images for replacements and deletes and
verified after-images for inserts and replacements. Use this version for
write-back even when an older executable can load the plan. Older writers use
their own comparison rules. Preview plans saved by older versions again,
including format 3. This version refuses a selected write with a duplicate
field name. Write-back also checks that the BSON type and bytes of each
image's `_id` match the saved key and that the after-image starts with `_id`.
MongoDB sets top-level zero timestamps to the current time on insert and
replacement, so a selected after-image with such a field outside `_id` is
refused before claim. Nested zero timestamps and a zero timestamp in `_id`
are allowed. The error groups affected documents by cause. See the recovery steps in
[Writing a plan back to the source](OPERATIONS.md#writing-a-plan-back-to-the-source).
When listing older plans, CLI JSON shows the field order actually stored in
each image and the typed lookup key separately. Unreadable plans appear by ID.
Preview reports the encoded size if the plan and its outcome fields exceed
MongoDB's 16 MiB document limit.

Before approving a write-back, run `argon merge check <plan-id>` with the
source URI in `--uri` or `ARGON_SOURCE_URI`. For a plan with conflicts, pass
the same `--strategy` you intend to apply. The command reads the pending plan
and scans indexed fields in affected source collections. It reports duplicate
keys among proposed inserts and replacements and keys held by another source
document, allowing for keys the plan deletes or replaces. The check leaves
the plan pending and its approval digest intact.

A collision returns a nonzero exit status. Table output and `--output json`
give the collection, index, key and both document IDs. The JSON field `in_plan`
identifies a collision between proposed documents. When several plan documents
share a key, each additional document names the first as its collision partner.
The check excludes the `_id` index. The `not_checked` list gives reasons for
skipping partial indexes, indexes with a collation, special index types such as
hashed, text, wildcard and geospatial, and documents with arrays on a key path.
Skips alone return zero. The report describes the proposed final state. Write
order and concurrent changes to the source can still affect apply.

For a followed parent, inspect `merge preview --output json` or
`merge list -p <project> --output json`. Each eligible pending plan includes
an `approval_digest` alongside the images. Use that plan's 64 lowercase hex
characters in `merge apply <plan-id> --write-back --approve <digest>`.
Write-back checks each selected collection for capped storage and each
selected replacement for identical before- and after-images. Either leaves
the plan pending and identifies the affected document before claim.
The source transaction reads each key again after all writes and before
commit. It compares the entire stored BSON with the approved after-image
and checks that each deletion left the key absent. A mismatch rolls back
the batch and leaves the plan pending for retry or a fresh preview.
Approval is explicit on each invocation, including plans with zero writes.
An absent, malformed or mismatched digest leaves the plan pending. Ordinary
apply rejects `--approve`. For source binding and capture requirements, see
[Follow a live database](#follow-a-live-database).
A write-back plan in `writing_back` is claimed and cannot be used again.
Check the affected documents on the source, let following catch up, then
run `argon merge preview` again for the sandbox. `merge list` prints this
guidance and includes it as `writing_back_advice` in JSON output. An empty
write-back, including one emptied by `--strategy ours`, marks the plan applied
and reports `"written_to_source": false`.

## Pins — immutable datasets

```
argon pin create  -p P [-b B] --name N [--lsn L | --time T] [--note …]
argon pin list    -p P
argon pin delete  -p P --name N
argon pin branch  -p P --name N --as NEW       durable branch from the pin
argon pin sandbox -p P --name N [--ttl 1h]     TTL sandbox from the pin
```

Pins protect their referenced captured states from GC and resets while they
exist. Pin an eval dataset once and fork a sandbox per run for identical
document input. Deleting the pin releases that protection; keep independent
backups.

## Sandboxes — disposable agent branches

```
argon sandbox create -p P [--from B] [--name N] [--ttl 1h]   fork+checkout+TTL
argon sandbox create -p P --credential                       issue a MongoDB user for this sandbox
argon sandbox create -p P --watch [--actor A]                 …and capture it in the foreground
argon sandbox list / discard / keep / sweep
```

`--credential` returns a URI with read/write access only to the sandbox's
physical database. Save it from the create output. Argon returns its password
once. Without this flag, the URI uses the engine's MongoDB credentials.
See [required MongoDB roles](OPERATIONS.md#exposure-and-credentials).

`sweep` reaps expired sandboxes and reports any it skips, including pinned
ones. A sandbox credential stays valid after expiry until a sweep reaps the
sandbox. `keep` removes the TTL and leaves the credential valid until
`discard`. Bare CLI commands do not start background workers. Keep
`watch` running and schedule `sweep`, or use console/API/MCP managed sandboxes.
`sandbox create --watch` starts capture before it prints the connection string
and runs until interrupted, so one terminal replaces `sandbox create` plus
`watch`. From 2.3.1 a collection the agent creates is captured exactly without
`collections prepare`; see [the agents guide](AGENTS.md#sandboxes) for the two
cases that still need it.

## Snapshots, GC, agents, migration

```
argon snapshot create -p P -b B     manual (they're also automatic)
argon snapshot list   -p P -b B
argon gc -p P [--retention 168h] [--dry-run]
    Reclaim entries covered by snapshots, outside retention, and needed
    by no live child or pin. No snapshot → nothing is ever deleted.

argon mcp [--read-only] [--disable-tools a,b]
                                    MCP server over stdio (13 tools); withhold tools
                                    such as argon_merge_apply to keep them manual.
                                    Starts without a database and connects in the
                                    background (releases after 2.3.1)
argon migrate-wal -p P [-o json]   v1 → v2 schema migration (idempotent)
argon status                        health and capture checks
argon metrics [-o json]             branches, sandboxes, checkouts and newest LSN per project
```

## Readiness and scripting

```bash
argon doctor --output json          # actual temporary write/capture probe
argon status --output json          # read-only checks, last reported capture status
argon projects list --output json   # {"projects": [...]}
argon branches list -p P --output json
argon merge preview -p P -b B --output json  # {"plan": {"id": ...}}
argon gc -p P --dry-run --output json             # {"dry_run": ..., "branches": [...], "entries_removed": ...}
argon restore preview -p P -b B --lsn N --output json  # {"branch": ..., "target_lsn": ...}
```

Doctor/status return nonzero when readiness fails. They apply the same
health checks to followed projects as `source status`. For ordinary
capture, the status is the worker's last stored report. Projects in the
middle of import enter deployment capture health when they are published.
The CLI does not support relative time on `time-travel info`; use retained
LSNs for queries and RFC3339 timestamps on restore/pin commands.

`console` binds 127.0.0.1 by default. Non-loopback binding requires
`ARGON_API_TOKEN` unless the restricted hosted demo is explicitly enabled.
Without a token it answers only to `localhost` and loopback addresses; set
`ARGON_ALLOWED_HOSTS` when a tunnel or reverse proxy uses another hostname.
