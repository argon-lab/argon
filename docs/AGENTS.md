# Argon for AI agents

Give the agent a real MongoDB database that is secretly a disposable
branch. Review what it did as a diff. Merge the good, undo the bad,
repeat runs from the same pinned document state.

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
| REST API + console | `argon console` (or `(cd api && go run .)`) | language SDKs, browsers |
| Wire proxy | `argon proxy` | stable per-branch connection strings |

## Sandboxes

Fork + checkout + TTL, one step:

```bash
argon sandbox create -p myapp --ttl 1h
# → connection string; hand it to the agent, any driver works
```

MCP and REST sandboxes wait for capture readiness and drain before returning
URIs. Their server processes also sweep expired sandboxes every minute. With
the bare CLI, add `--watch --actor <run-label>` to `sandbox create` (or keep
`argon watch --actor <run-label>` running) and explicitly schedule
`argon sandbox sweep`. Actor labels apply to an entire branch and
are persisted; use a new sandbox for a different run label.

Collections inherited from the parent are ready for exact capture. A collection
the agent creates is captured exactly as well (2.3.1 and newer): Argon reads the
states of its first writes back from MongoDB, which keeps them for five minutes
by default, so capture has to be running or a `diff` has to follow within that
time. Two cases still need `argon collections prepare` (or
`changeStreamPreAndPostImages: {enabled: true}` at creation): older versions,
and a document that is updated and then changed again inside one transaction on
a collection created moments earlier. A sandbox is a full physical copy; the
[benchmarks](https://github.com/argon-lab/benchmarks) measure how long one takes
to be ready as `sandbox_capture_ready_ms`. Then:

```bash
argon diff -p myapp -b <sandbox>              # review supported document changes
argon merge preview/apply …                   # adopt it — a reviewable data PR
argon undo … --actor <agent>                  # filter the branch actor label, conflict-aware
argon sandbox discard …                       # or let the managed server sweep it
```

## Pins — reproducible evals

A pin is a named, immutable reference to a captured branch state. Its
referenced history is protected from GC and resets while the pin exists:

```bash
argon pin create  -p myapp --name eval-v1 --note "golden dataset"
argon pin sandbox -p myapp --name eval-v1     # fresh sandbox per run
```

Pin once, fork per run: identical input state every time, no matter what
happened to the branch since. Delete the pin to release its history.
Pins are not independent backups, and matching input data does not make an
agent's output deterministic.

## MCP server

```bash
claude mcp add argon -- argon mcp
```

Thirteen tools: sandbox create/discard/keep · branch list · connect ·
diff · merge preview/apply · undo · snapshot create · pin create/list/
sandbox. The server runs a capture ingester for every sandbox it hands
out — no extra process.

The server starts without a database (releases after 2.3.1; up to 2.3.1 it
exits at startup when MongoDB is unreachable). The handshake and `tools/list`
need no connection, so a client or a directory can start it just to see what it
offers. It reaches for `MONGODB_URI` in the background from the start (2.3.3; in 2.3.2 only when a tool was first called), so expired sandboxes are swept even if no tool is ever called. If MongoDB is
not reachable when a tool is called, that call returns a tool error that says so, the server
keeps running, and the next call tries again. Capture and the sweep of expired
sandboxes begin as soon as the database answers.

Every tool declares how it behaves (MCP tool annotations): `argon_branch_list`,
`argon_diff` and `argon_pin_list` are read-only; `argon_merge_apply`,
`argon_undo` and `argon_sandbox_discard` are marked destructive, so clients
that ask before destructive calls will ask before those.

By default the agent can apply a merge plan it prepared itself: a preview is
a reviewable artifact, not an approval gate. To keep the decision with a
person, withhold the tool and apply reviewed plans from the CLI:

```bash
claude mcp add argon -- argon mcp --disable-tools argon_merge_apply
argon merge apply <plan-id>          # after you have read the plan
```

`--disable-tools` takes a comma-separated list. `argon mcp --read-only`
offers only the three read-only tools. A withheld tool is neither listed nor
callable.

The server speaks the handshake-based MCP revisions 2024-11-05, 2025-03-26
and 2025-06-18 and answers with the client's revision when it is one of
them. The per-request protocol introduced in 2026-07-28 (`server/discover`,
no `initialize`) is not implemented yet; clients that support both fall back
to the handshake.

## REST control plane

`argon console` serves the REST API plus a web UI locally (binds
127.0.0.1, opens your browser); `(cd api && go run .)` serves the API alone
(default `127.0.0.1:8080`, `PORT`/`HOST` to change). Control plane only — data flows
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
POST   /api/v1/projects/:p/sandboxes                   {name?, from?, ttl_minutes?, actor?}
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
POST   /api/v1/projects/:p/pins/:name/sandboxes        {name?, ttl_minutes?, actor?}
GET    /api/v1/meta
GET    /api/v1/status/ingesters
```

Sandbox-creating endpoints start a supervised ingester; errors return
`{"error": "..."}` with a meaningful status. Optional switches, all off
by default: `ARGON_API_TOKEN` (Bearer auth on every `/api` endpoint
except `/meta`), `ARGON_READ_ONLY=1`, `ARGON_CORS_ORIGINS`,
`ARGON_ALLOWED_HOSTS` (extra hostnames a token-less local server answers
to; by default only `localhost` and loopback addresses), and
`ARGON_DEMO_MODE=1` — a restricted anonymous hosted playground with no native URIs/checkout:
one ephemeral
seeded project per visitor, requests scoped to it, writes rate-limited,
everything reclaimed after `ARGON_DEMO_TTL_MINUTES` (default 60).

## Python — argon-agents

Install SDK v0.2.0 from PyPI, with `[langgraph]` for the checkpointer:

```bash
python3 -m pip install 'argon-agents[langgraph]==0.2.0'
```

Start `argon console --no-browser` in another terminal. It listens on port
1818; use port 8080 instead if you started the standalone API with `go run .`.

```python
from argon_agents import ArgonClient, ArgonCheckpointSaver

argon = ArgonClient("http://127.0.0.1:1818")
argon.get_or_create_project("myapp")

# LangGraph: the official MongoDB checkpointer on a sandboxed branch
saver = ArgonCheckpointSaver.from_sandbox(argon, "myapp", ttl_minutes=60)
# Compile and run your graph with checkpointer=saver.
# Optional: forked_saver = saver.fork(argon) branches its checkpoint history.
plan = argon.merge_preview("myapp", saver.sandbox.branch)
print(plan)           # inspect document changes and conflicts
# After reviewing this exact plan:
# argon.merge_apply(plan["id"])
# Or reject the run: saver.discard()

# Mem0: sandboxed agent memory
from argon_agents import sandboxed_mem0_config
config, sandbox = sandboxed_mem0_config(argon, "myapp")

# Reproducible evals
argon.create_pin("myapp", "eval-v1")
run = argon.sandbox_from_pin("myapp", "eval-v1")
```

LangGraph's checkpoint ids rewind steps *within* a thread; Argon adds
fork/merge/undo/audit *across* the whole store.
The `saver.merge()`, `sandbox.merge()` and `argon.merge()` convenience methods
preview and apply immediately, without an approval pause. Use separate preview
and apply calls for reviewed changes. The running API manages capture and TTL
sweeps; materializing a sandbox copies data into a physical MongoDB database.
Mem0 semantic vector retrieval also requires Atlas Search or a compatible
MongoDB Search deployment and separately provisioned search indexes.

## Wire proxy

Checked-out branches get machine-named physical databases. `argon proxy`
serves stable, human-readable URIs instead:

```
mongodb://127.0.0.1:27018/<project>~<branch>?directConnection=true
```

It listens on loopback unless you pass `--listen <addr> --allow-remote`; the
proxy does not authenticate clients, so only do that in front of a MongoDB
that requires authentication.

The proxy rewrites each command's `$db` to the branch's physical database;
mongod still evaluates every query. Constraints: `directConnection=true`
required, compression negotiated away, `authSource=admin` with auth,
unresolvable aliases return a clean command error. Capture stays
asynchronous — run `argon watch` (or use API/MCP sandboxes) behind it.

## Complete business-data example

[examples/pinned_agents.py](../examples/pinned_agents.py) runs two deterministic
agent proposals against one pin, writes through PyMongo, checks isolation,
reviews both diffs, merges the approved discount and discards the rejected
proposal. Start `argon console --no-browser`, install `pymongo`, then run:

```bash
python3 examples/pinned_agents.py --api http://127.0.0.1:1818
```

Set `ARGON_API_TOKEN` in the example's environment when the local server uses
a bearer token. This requires your own engine; the hosted demo does not
provide database credentials. Native MongoDB credentials remain deployment
credentials, not a security boundary between mutually untrusted agents.
Provision restricted MongoDB users/deployments for those workloads.

[examples/boxd-sandbox](../examples/boxd-sandbox/) does that for agent-written
code running in a separate code sandbox: a boxd machine gets a MongoDB user
that can only read and write its Argon sandbox, then the host reviews the diff
and merges or discards it.

A newly materialized collection is already prepared by checkout, and from
2.3.1 capture reads back the exact states of writes that reach a new collection
before its images are enabled. Preparing a collection first, with
`argon collections prepare` or the MongoDB driver's
`changeStreamPreAndPostImages: {enabled:true}` option, avoids those reads and
is required on older versions.
