# CLI reference

Every command talks to `MONGODB_URI` (default `mongodb://localhost:27017`);
metadata lives in the `argon_wal` database. Shared flags: `-p/--project`,
`-b/--branch` (default `main`). Commands that advertise `-o/--output` support
`table|json`; unsupported formats fail before work begins. JSON produces one
value on stdout and diagnostics go to stderr. Projects/branches, sandbox, pin,
diff/merge, undo, checkout/connect/release, history, snapshot, doctor and status
all support JSON. The former global `--api-key`, `--project-id` and `--config`
options were removed because they did not affect execution. Configure MongoDB
through `MONGODB_URI`; configure server auth through `ARGON_API_TOKEN`.

## Projects & branches

```
argon projects create <name>                   project + main branch
argon projects list
argon branches create <name> -p P [--from B]   instant — a pointer, no copy
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
argon console [--port 1818]   local web console (REST API + UI), opens browser
```

While a branch is checked out, its history is fed by `watch` (or the
API/MCP servers' ingesters); SDK writes to it are refused — one source of
truth. `checkout` and `connect` return a connection string only for the
branch's own physical database.

## Import ("git clone")

```
argon import preview  --uri U --database D [--online]
argon import database --uri U --database D --project P --online [--follow [--allow-inexact]] [--enable-source-images] [--yes] [--dry-run]
argon import database --uri U --database D --project P --source-quiesced [--yes]
argon import database --uri U --database D --project P --dry-run
argon import cleanup --project P --yes   # only after stopping a crashed importer
argon import status
```

Imports auto-snapshot, so reads never replay the whole import. A real import
needs one of two flags, and `--yes` replaces neither:

- `--online` (releases after 2.5.0) imports a replica-set database while it
  is being written. It is experimental, off by default, and has known open
  issues. Set `ARGON_EXPERIMENTAL_SOURCE=1` to enable it. The result is the
  source at one cluster time, which the command prints, and the project's
  `main` then follows that database (next section). `--follow` keeps following
  in the foreground once the import is done, and `--allow-inexact` (only with
  `--follow`) means what it means for `argon source follow`. Nothing is written
  to the source. `--enable-source-images` is the one exception and has to be
  asked for: it switches on `changeStreamPreAndPostImages` for the source's
  collections, and needs an import by a user allowed to run
  `replSetGetConfig`, such as one with the `clusterMonitor` role.
- `--source-quiesced` is for a database nobody is writing to: pause its
  writers and DDL for the whole import. The result is a one-time copy.

Both preview modes skip views and estimate copy size. `import preview --online`
also includes ordinary collections whose names start with `admin.`, `config.`
or `local.`. `import database --online --dry-run` checks whether an online
import can start and reports its collections and estimated document count
without writing anything.

`--uri` can be left out when `ARGON_SOURCE_URI` is set. Source preflight
checks collection types and readability. Online preflight opens and reads the
stream with the replay's options and start position. The target stays unpublished
until the import succeeds. Ordinary failures clean up the reserved project and
history.
After a hard kill, stop the old process and use `import cleanup` before
retrying the same name. Completed projects cannot be removed by cleanup. See
[the import contract](OPERATIONS.md#importing-existing-data).

## Follow a live database

The `argon source` commands and `merge apply --write-back` are experimental
and off by default. They have known open issues. Set
`ARGON_EXPERIMENTAL_SOURCE=1` to enable them.

```
argon source follow -p P [--uri U] [--allow-inexact] [--enable-source-images] [--auto-resync]
argon source status -p P [-o json]
argon source resync -p P [--uri U] [--force] [-o json]
argon source detach -p P --yes
argon merge apply <plan-id> --write-back [--uri U] [--strategy theirs|ours]
```

A project imported with `--online` has a `main` that mirrors its source.
`source follow` applies the source's changes to it until interrupted and
resumes where it stopped. The connection string comes from `--uri` or
`ARGON_SOURCE_URI` on every command and is never stored. Each connection
must match the identity recorded at import. If the import could read the
replica-set ID, each connection must prove the same ID. Otherwise following
and resync match on the set name and at least one member host.

`--write-back` and `--enable-source-images` require a replica-set ID recorded
at import. Separate deployments can report the same set name and member
hosts. For a project bound by name and hosts, detach it with
`argon source detach -p P --yes`, then import the source online into a new
project as a user allowed to run `replSetGetConfig`, such as one with the
`clusterMonitor` role.

`source follow --enable-source-images` checks the binding, saved position
and lease, then opens the stream before altering collections. A degraded
position must recover or be resynced first. If following subsequently
fails, the error names any source collections on which it enabled images.
The follower checks the lease before each alteration.

While `main` follows a source, the source is the only thing that changes it:
`merge apply` without `--write-back`, `undo`, `restore reset`, `checkout` and
programmatic writes on `main` are refused. Sandboxes forked from it work as
usual. To bring a sandbox's changes back, `merge apply <plan-id> --write-back`
writes the plan to the source database in one transaction. It starts after
a majority-acknowledged metadata claim, and checks every document against
the plan's before-image before writing that document. One mismatch aborts the
transaction. `main` shows the result once following has read it back from
the source. The MCP server and REST API do not offer write-back.

`source status` exits nonzero when following is degraded, a resync is
unfinished, or the lease of the last running follower or resync has
expired. It lists the inexact ranges of `main`'s history.
It shows the binding's identity check and reports when that check rules
out write-back (`write_back_unavailable_reason` in JSON).
It reports the lease owner's run type, host, process ID and last heartbeat
in both table and JSON output. Only one follower or resync can hold the
lease for a project at a time. Expiry revokes the lease. The status is
reported as stopped and retains the saved position.

If Argon 2.5.0 checked out a followed `main`, status reports it as
degraded. Upgrade all processes using that metadata database, stop writers
to the checkout, then run `argon release -p P -b main` and
`argon source resync -p P`.

`source resync` repairs degraded following in place, such as after a
collection was dropped or renamed on the source, after the source database
was dropped, when the source's oplog no longer holds the saved position,
or when another process moved `main`'s head.
It compares the source with `main` in batches, stores what differs,
deletes from `main` what the source no longer has, and replays the source's
changes made meanwhile. View creation, modification and removal are ignored,
and a view that replaced a collection counts as an absent collection during
resync. The command prints the cluster time and LSN at which `main`
equals the source again; `source follow` continues from there. It reads the
source and writes nothing to it. The LSNs it writes are recorded as an
inexact range. Run without `--force`, it requires degraded following,
an unfinished resync or an expired run. If a resync is interrupted or
the source drops or renames another collection while it runs, `source status`
reports the unfinished resync and plain `source follow` refuses to start.
Run `source resync` again or restart with `source follow --auto-resync` to
complete it. `--auto-resync` also repairs a gap in the source stream before
following again. It stops if another automatic resync would be needed
within a minute. If another process moves the branch head, follow stops as
degraded and requires `source resync` to be run explicitly.

`source detach` ends the tie for good
and turns `main` back into an ordinary branch. See
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
argon merge apply <plan-id> [--strategy theirs|ours]
argon merge list    -p P
```

Plans apply exactly once and refuse stale parent heads; conflicts fail
loudly unless a strategy resolves them. Merges are undoable like any range.
When the parent follows an external database, a plan is applied with
`--write-back`. Write-back is experimental, off by default, and has known
open issues. Set `ARGON_EXPERIMENTAL_SOURCE=1` to enable it. See
[Follow a live database](#follow-a-live-database).
A write-back plan in `writing_back` is claimed and cannot be used again.
Check the affected documents on the source, let following catch up, then
run `argon merge preview` again for the sandbox. `merge list` prints this
guidance and includes it as `writing_back_advice` in JSON output.

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
argon sandbox create -p P --watch [--actor A]                 …and capture it in the foreground
argon sandbox list / discard / keep / sweep
```

`sweep` reaps expired sandboxes (pinned ones skipped loudly); `keep`
removes the TTL. Bare CLI commands do not start background workers. Keep
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
argon migrate-wal --project P [--dry-run]      v1 → v2 schema migration
argon status / metrics              health and performance counters
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
capture, the status is the worker's last stored report.
The CLI does not support relative time on `time-travel info`; use retained
LSNs for queries and RFC3339 timestamps on restore/pin commands.

`console` binds 127.0.0.1 by default. Non-loopback binding requires
`ARGON_API_TOKEN` unless the restricted hosted demo is explicitly enabled.
Without a token it answers only to `localhost` and loopback addresses; set
`ARGON_ALLOWED_HOSTS` when a tunnel or reverse proxy uses another hostname.
