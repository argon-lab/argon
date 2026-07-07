# Argon for AI agents

Give the agent a real MongoDB database that is secretly a disposable
branch. Review what it did as a diff. Merge the good, undo the bad,
reproduce any run from a pinned dataset.

```
pin ──▶ sandbox (TTL) ──▶ agent writes via any driver ──▶ diff
                                                           │
                              merge (data PR) ◀────────────┴──▶ undo / discard
```

Four surfaces, one engine — pick by integration point:

| Surface | Start it | For |
|---|---|---|
| CLI | `argon sandbox`, `argon pin`, … | humans, scripts, CI |
| MCP server | `argon mcp` (stdio) | Claude / Cursor / any MCP client |
| REST API + console | `argon console` (or `go run ./api`) | language SDKs, browsers |
| Wire proxy | `argon proxy` | stable per-branch connection strings |

## Sandboxes

Fork + checkout + TTL, one step:

```bash
argon sandbox create -p myapp --ttl 1h
# → connection string; hand it to the agent, any driver works
```

MCP and REST sandboxes capture writes automatically (supervised
ingesters); with the bare CLI, run `argon watch`. Then:

```bash
argon diff -p myapp -b <sandbox>              # exactly what the agent changed
argon merge preview/apply …                   # adopt it — a reviewable data PR
argon undo … --actor <agent>                  # revert one writer, conflict-aware
argon sandbox discard …                       # or let the TTL reclaim it
```

## Pins — reproducible evals

A pin is a named, immutable branch state that survives GC and resets
forever:

```bash
argon pin create  -p myapp --name eval-v1 --note "golden dataset"
argon pin sandbox -p myapp --name eval-v1     # fresh sandbox per run
```

Pin once, fork per run: identical input state every time, no matter what
happened to the branch since. Delete the pin to release its history.

## MCP server

```bash
claude mcp add argon -- argon mcp
```

Thirteen tools: sandbox create/discard/keep · branch list · connect ·
diff · merge preview/apply · undo · snapshot create · pin create/list/
sandbox. The server runs a capture ingester for every sandbox it hands
out — no extra process.

## REST control plane

`argon console` serves the REST API plus a web UI locally (binds
127.0.0.1, opens your browser); `go run ./api` serves the API alone
(default `:8080`, `PORT` to change). Control plane only — data flows
through the MongoDB connection strings it returns.

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
GET    /api/v1/projects/:p/sandboxes
DELETE /api/v1/projects/:p/sandboxes/:b
POST   /api/v1/projects/:p/sandboxes/:b/extend         {ttl_minutes}
POST   /api/v1/projects/:p/sandboxes/:b/keep
GET    /api/v1/projects/:p/branches/:b/diff
POST   /api/v1/projects/:p/branches/:b/merge-preview
GET    /api/v1/merge-plans?project=:p
GET    /api/v1/merge-plans/:id
POST   /api/v1/merge-plans/:id/apply                   {strategy?}
POST   /api/v1/projects/:p/branches/:b/undo            {from_lsn, to_lsn?, actor?, dry_run?}
GET    /api/v1/projects/:p/branches/:b/entries         ?from_lsn&to_lsn&actor&collection&order&limit
GET    /api/v1/projects/:p/branches/:b/time-travel
GET    /api/v1/projects/:p/branches/:b/time-travel/query  ?lsn&collection&skip&limit
POST   /api/v1/projects/:p/branches/:b/snapshots
GET    /api/v1/projects/:p/pins
POST   /api/v1/projects/:p/pins                        {name, branch?, lsn?, note?}
DELETE /api/v1/projects/:p/pins/:name
POST   /api/v1/projects/:p/pins/:name/branches         {name}
POST   /api/v1/projects/:p/pins/:name/sandboxes        {name?, ttl_minutes?}
GET    /api/v1/meta
GET    /api/v1/status/ingesters
```

Sandbox-creating endpoints start a supervised ingester; errors return
`{"error": "..."}` with a meaningful status. Optional switches, all off
by default: `ARGON_API_TOKEN` (Bearer auth on every `/api` endpoint
except `/meta`), `ARGON_READ_ONLY=1`, `ARGON_CORS_ORIGINS`, and
`ARGON_DEMO_MODE=1` — an anonymous hosted playground: one ephemeral
seeded project per visitor, requests scoped to it, writes rate-limited,
everything reclaimed after `ARGON_DEMO_TTL_MINUTES` (default 60).

## Python — argon-agents

`pip install argon-agents` (add `[langgraph]` for the checkpointer):

```python
from argon_agents import ArgonClient, ArgonCheckpointSaver

argon = ArgonClient("http://localhost:8080")

# LangGraph: the official MongoDB checkpointer on a sandboxed branch
saver = ArgonCheckpointSaver.from_sandbox(argon, "myapp", ttl_minutes=60)
graph = builder.compile(checkpointer=saver)
saver.fork(argon)     # branch the whole checkpoint history
saver.merge()         # adopt the run — or saver.discard()

# Mem0: sandboxed agent memory
from argon_agents import sandboxed_mem0_config
config, sandbox = sandboxed_mem0_config(argon, "myapp")

# Reproducible evals
argon.create_pin("myapp", "eval-v1")
run = argon.sandbox_from_pin("myapp", "eval-v1")
```

LangGraph's checkpoint ids rewind steps *within* a thread; Argon adds
fork/merge/undo/audit *across* the whole store.

## Wire proxy

Checked-out branches get machine-named physical databases. `argon proxy`
serves stable, human-readable URIs instead:

```
mongodb://proxy-host:27018/<project>~<branch>?directConnection=true
```

The proxy rewrites each command's `$db` to the branch's physical database;
mongod still evaluates every query. Constraints: `directConnection=true`
required, compression negotiated away, `authSource=admin` with auth,
unresolvable aliases return a clean command error. Capture stays
asynchronous — run `argon watch` (or use API/MCP sandboxes) behind it.
