# Argon for AI agents

Agents write to databases with confidence and no memory of consequences.
Argon's answer is to make every agent write *versioned*: give the agent a
real MongoDB database that is actually a disposable branch, review what it
did as a diff, merge what's good, undo what isn't, and reproduce any run
from a pinned dataset.

Four surfaces expose the same engine — pick by integration point:

| Surface | Start it | For |
|---|---|---|
| CLI | `argon sandbox`, `argon pin`, … | humans, scripts, CI |
| MCP server | `argon mcp` (stdio) | Claude/Cursor/any MCP client |
| REST API | `go run ./api` (port 8080) | language SDKs, services |
| Wire proxy | `argon proxy` | stable per-branch connection strings |

## Sandboxes

A sandbox is a branch forked from a parent, checked out into its own
physical MongoDB database, and stamped with a TTL:

```bash
argon sandbox create -p myapp --ttl 1h
# → connection string; hand it to the agent, any driver works
```

Every write the agent makes is captured as versioned history (the MCP and
REST servers run the capture ingester for you; with the bare CLI, run
`argon watch`). Then:

- `argon diff` — see exactly what the agent changed
- `argon merge preview/apply` — adopt it, as a reviewable data PR
- `argon undo --actor <agent>` — revert one writer's changes, refusing
  documents someone else touched since
- `argon sandbox discard` / TTL expiry — throw it away; storage reclaimed

## Pins: reproducible eval datasets

A pin is a named, immutable reference to a branch state that survives
garbage collection and resets forever:

```bash
argon pin create  -p myapp --name eval-v1 --note "golden dataset"
argon pin sandbox -p myapp --name eval-v1     # fresh sandbox per eval run
```

Pin the dataset once, fork a sandbox from the pin for every run: identical
input state on every run, no matter what happened to the branch since —
including resets. Delete the pin to release its history to GC.

## The MCP server

```bash
claude mcp add argon -- argon mcp
```

Thirteen tools over stdio: `argon_sandbox_create` / `argon_sandbox_discard`
/ `argon_sandbox_keep`, `argon_branch_list`, `argon_connect`, `argon_diff`,
`argon_merge_preview` / `argon_merge_apply`, `argon_undo`,
`argon_snapshot_create`, `argon_pin_create` / `argon_pin_list` /
`argon_pin_sandbox`. The server supervises a change-stream ingester for
every sandbox it hands out, so agent writes become history without any
extra process.

## The REST control plane

`api/` serves the same workflow over HTTP for language SDKs (default
`:8080`, `PORT` to change). Control plane only — data flows through the
MongoDB connection strings it returns.

```
POST   /api/v1/projects                                {name}
GET    /api/v1/projects
GET    /api/v1/projects/:p/branches
POST   /api/v1/projects/:p/branches                    {name, from}
GET    /api/v1/projects/:p/branches/:b
DELETE /api/v1/projects/:p/branches/:b
POST   /api/v1/projects/:p/branches/:b/checkout
POST   /api/v1/projects/:p/branches/:b/release
POST   /api/v1/projects/:p/sandboxes                   {name?, from?, ttl_minutes?}
GET    /api/v1/projects/:p/branches/:b/diff
POST   /api/v1/projects/:p/branches/:b/merge-preview
POST   /api/v1/merge-plans/:id/apply                   {strategy?}
POST   /api/v1/projects/:p/branches/:b/undo            {from_lsn, to_lsn?, actor?, dry_run?}
GET    /api/v1/projects/:p/branches/:b/time-travel
POST   /api/v1/projects/:p/branches/:b/snapshots
GET    /api/v1/projects/:p/pins
POST   /api/v1/projects/:p/pins                        {name, branch?, lsn?, note?}
DELETE /api/v1/projects/:p/pins/:name
POST   /api/v1/projects/:p/pins/:name/branches         {name}
POST   /api/v1/projects/:p/pins/:name/sandboxes        {name?, ttl_minutes?}
```

Sandbox-creating endpoints start a supervised ingester; errors return
`{"error": "..."}` with a meaningful status.

## Python: argon-agents

[argon-lab/argon-agents](https://github.com/argon-lab/argon-agents) wraps
the REST API for agent frameworks
(`pip install argon-agents` — add `[langgraph]` for the checkpointer):

```python
from argon_agents import ArgonClient, ArgonCheckpointSaver

argon = ArgonClient("http://localhost:8080")

# LangGraph: the official MongoDB checkpointer on a sandboxed branch
saver = ArgonCheckpointSaver.from_sandbox(argon, "myapp", ttl_minutes=60)
graph = builder.compile(checkpointer=saver)
...
saver.fork(argon)        # branch the whole checkpoint history
saver.merge()            # adopt the run   — or saver.discard()

# Mem0: sandboxed agent memory
from argon_agents import sandboxed_mem0_config
config, sandbox = sandboxed_mem0_config(argon, "myapp")

# Reproducible evals
argon.create_pin("myapp", "eval-v1")
run = argon.sandbox_from_pin("myapp", "eval-v1")
```

LangGraph's checkpoint ids give step-level rewind *within* a thread;
Argon adds branch-level fork/merge/undo/audit *across* the whole store.

## The wire proxy

Checked-out branches get machine-named physical databases
(`argon_br_<id>`). `argon proxy` gives them stable, human-readable
connection strings instead:

```
mongodb://proxy-host:27018/<project>~<branch>?directConnection=true
```

The proxy rewrites each command's `$db` to the branch's physical database
and forwards everything else byte-for-byte; mongod still evaluates every
query. Constraints, honestly: `directConnection=true` is required,
compression is negotiated away, use `authSource=admin` with auth, and
unresolvable aliases return a clean command error naming the proxy.
Capture stays asynchronous — run `argon watch` (or use API/MCP sandboxes)
for the branch behind the alias.
