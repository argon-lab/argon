# CLI reference

Every command talks to `MONGODB_URI` (default `mongodb://localhost:27017`);
metadata lives in the `argon_wal` database. Shared flags: `-p/--project`,
`-b/--branch` (default `main`), `-o/--output table|json|yaml`.

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
                              print its URI (re-run to refresh)
argon connect  -p P -b B      print a checked-out branch's URI
argon watch    -p P -b B      capture direct writes into history (keep running)
argon release  -p P -b B      drop the physical db; history stays
argon proxy [--listen :27018] stable URIs: mongodb://host/<project>~<branch>
argon console [--port 1818]   local web console (REST API + UI), opens browser
```

While a branch is checked out, its history is fed by `watch` (or the
API/MCP servers' ingesters); SDK writes to it are refused — one source of
truth.

## Import ("git clone")

```
argon import preview  --uri U --database D
argon import database --uri U --database D --project P [--dry-run] [--yes]
argon import status
```

Imports auto-snapshot, so reads never replay the whole import.

## History: time travel, undo, restore

```
argon time-travel info  -p P -b B
argon time-travel query -p P -b B --lsn N [-c collection]

argon undo -p P -b B --from-lsn N [--to-lsn M] [--actor A] [--dry-run]
    Revert a range by restoring pre-images — append-only, never rewrites
    history. --actor reverts one writer and refuses documents someone
    else touched since.

argon restore preview -p P -b B (--lsn N | --time RFC3339)
argon restore reset   -p P -b B (--lsn N | --time RFC3339) [--backup NAME]
    Rewind the head. Recorded, not destructive: discarded entries stay
    for audit; --backup forks the pre-reset head first.
argon restore branch  -p P -b B (--lsn N | --time RFC3339) --as NAME
    Fork the historical state into a new branch instead.
```

## Merge — data pull requests

```
argon diff          -p P -b B                  what merging B would change
argon merge preview -p P -b B                  persist a reviewable plan
argon merge apply <plan-id> [--strategy theirs|ours]
argon merge list    -p P
```

Plans apply exactly once and refuse stale parent heads; conflicts fail
loudly unless a strategy resolves them. Merges are undoable like any range.

## Pins — immutable datasets

```
argon pin create  -p P [-b B] --name N [--lsn L | --time T] [--note …]
argon pin list    -p P
argon pin delete  -p P --name N
argon pin branch  -p P --name N --as NEW       durable branch from the pin
argon pin sandbox -p P --name N [--ttl 1h]     TTL sandbox from the pin
```

Pinned states survive GC and resets forever — pin an eval dataset once,
fork a sandbox per run, get identical input every time.

## Sandboxes — disposable agent branches

```
argon sandbox create -p P [--from B] [--name N] [--ttl 1h]   fork+checkout+TTL
argon sandbox list / discard / keep / sweep
```

`sweep` reaps expired sandboxes (pinned ones skipped loudly); `keep`
removes the TTL.

## Snapshots, GC, agents, migration

```
argon snapshot create -p P -b B     manual (they're also automatic)
argon snapshot list   -p P -b B
argon gc -p P [--retention 168h] [--dry-run]
    Reclaim entries covered by snapshots, outside retention, and needed
    by no live child or pin. No snapshot → nothing is ever deleted.

argon mcp                           MCP server over stdio (13 tools)
argon migrate-wal --project P [--dry-run]      v1 → v2 schema migration
argon status / metrics              health and performance counters
```
