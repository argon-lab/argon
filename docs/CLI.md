# CLI reference

Every command talks to the MongoDB deployment named by `MONGODB_URI`
(default `mongodb://localhost:27017`); Argon's metadata lives in the
`argon_wal` database there. Shared flags: `-p/--project`, `-b/--branch`
(default `main`), `-o/--output table|json|yaml`.

## Projects and branches

```
argon projects create <name>          Create a project (with a main branch)
argon projects list
argon branches create <name> -p P [--from B]   A metadata write, no data copy
argon branches list -p P
argon branches delete <name> -p P     Refused for main, for branches with
                                      live children, and for pinned branches
```

## Working with real MongoDB databases

```
argon checkout -p P -b B    Materialize the branch into a physical MongoDB
                            database and print its connection string.
                            Re-running refreshes to the branch's WAL state.
argon connect  -p P -b B    Print the connection string of a checked-out branch
argon watch    -p P -b B    Capture the branch's direct writes into the WAL
                            (change stream; keep it running while you write)
argon release  -p P -b B    Drop the physical database; the WAL keeps history
argon proxy [--listen :27018]
                            Wire-protocol proxy: clients connect to
                            mongodb://host:27018/<project>~<branch>
                            (directConnection=true) and are routed to the
                            branch's physical database
```

While a branch is checked out, its WAL is fed by `watch` (or the API/MCP
server's supervised ingesters); programmatic SDK writes to it are refused
to keep one source of truth.

## Import ("git clone")

```
argon import preview  --uri U --database D
argon import database --uri U --database D --project P [--dry-run] [--yes]
argon import status
```

Import writes every document as versioned history and snapshots the result
automatically, so subsequent reads don't replay the whole import.

## History: time travel, undo, restore

```
argon time-travel info  -p P -b B
argon time-travel query -p P -b B --lsn N [-c collection]

argon undo -p P -b B --from-lsn N [--to-lsn M] [--actor A] [--dry-run]
        Revert a range by restoring pre-images — append-only compensations,
        never rewritten history. With --actor, reverts one writer's changes
        and refuses documents that someone else touched since.

argon restore preview -p P -b B (--lsn N | --time RFC3339)
argon restore reset   -p P -b B (--lsn N | --time RFC3339) [--backup NAME]
        Rewind the branch head. Recorded, not destructive: discarded
        entries stay for audit; --backup forks the pre-reset head first.
argon restore branch  -p P -b B (--lsn N | --time RFC3339) --as NAME
        Fork the historical state into a new branch instead.
```

## Merge — data pull requests

```
argon diff          -p P -b B      What merging B into its parent would change
argon merge preview -p P -b B      Compute and persist a reviewable plan
argon merge apply <plan-id> [--strategy theirs|ours]
argon merge list    -p P
```

Plans apply exactly once and refuse stale parent heads. Conflicts (both
sides changed the same document since the fork) fail loudly unless a
strategy resolves them; every merge is one WAL entry range, undoable like
any other.

## Pins — immutable datasets

```
argon pin create  -p P [-b B] --name N [--lsn L | --time T] [--note ...]
argon pin list    -p P
argon pin delete  -p P --name N
argon pin branch  -p P --name N --as NEW       Durable branch from the pin
argon pin sandbox -p P --name N [--ttl 1h]     TTL sandbox from the pin
```

A pin names a branch state and keeps it materializable forever: GC never
reclaims what a pinned read needs, and resets can't disturb it. Pin an
eval dataset once; fork a sandbox per run; every run starts identical.

## Sandboxes — disposable agent branches

```
argon sandbox create -p P [--from B] [--name N] [--ttl 1h]
argon sandbox list / discard / keep / sweep
```

`create` forks, checks out and TTL-stamps in one step and prints the
connection string. `sweep` reaps expired sandboxes (pinned ones are
skipped loudly); `keep` removes the TTL.

## Snapshots and GC

```
argon snapshot create -p P -b B      Manual snapshot (they're also automatic)
argon snapshot list   -p P -b B
argon gc -p P [--retention 168h] [--dry-run]
```

Snapshots bound replay depth; GC deletes WAL entries that are covered by
snapshots, outside the retention window, and needed by no live child or
pin. No snapshot → nothing is ever deleted.

## Agents and migration

```
argon mcp                 Serve the sandbox/merge/undo/pin workflow over the
                          Model Context Protocol (stdio); supervises an
                          ingester for every sandbox it hands out
argon console [--port 1818] [--host 127.0.0.1] [--no-browser]
                          Serve the web console (REST API + UI) against your
                          local engine and open it in a browser
argon migrate-wal --project P [--dry-run]    v1 → v2 WAL schema migration
argon status / metrics    Health and performance counters
```
