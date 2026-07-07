# Argon Features Overview

> This document describes what is implemented **today**. Where a capability is
> planned but not built, it says so and names the milestone. Performance
> numbers will be published together with the reproducible benchmark suite in
> M2 — until then we deliberately don't quote any. See
> [ARCHITECTURE.md](ARCHITECTURE.md) for how each feature works.

## 🚀 Core Features

### Git-like branching
- **Millisecond branch creation** — a branch is one metadata document
  (parent, fork LSN, head LSN); no data is copied, regardless of database size
- **Branch ancestry with fork-point isolation** — sibling branches can never
  see each other's writes
- **Branch deletion reclaims storage** — snapshots and unreferenced chunks
  are dropped with the branch

### Time travel & restore
- **Query any historical state**, addressed by LSN or timestamp
- **Deterministic replay** — the same history always reconstructs the same
  state, byte for byte; enforced structurally and verified by property tests
  in CI (repeated replay, cross-instance, historical LSNs)
- **Restore / reset** rewinds a branch; abandoned history stays in the log
  for audit (discarded ranges), so you can undo the undo
- **Snapshots bound replay depth** — materialization starts from the nearest
  snapshot and replays only the delta above it; snapshots are taken
  automatically off the write path, content-addressed, and deduplicated

### Audit trail
- Every write is a WAL entry with **full pre/post document images** and an
  **actor** field (`user:...`, `agent:...`, `importer`)
- Immutable, append-only log — the foundation for diff, undo, and compliance
  (merge/diff commands land in M4)
- WAL entries are compressed per entry (zstd by default)

## 📊 Developer Experience

### CLI
```bash
# Install
brew install argon-lab/tap/argonctl    # macOS
npm install -g argonctl                 # Cross-platform
pip install argon-mongodb               # Python SDK

# Use
export ENABLE_WAL=true
argon projects create my-app
argon import database --uri "mongodb://localhost:27017" --database myapp --project my-app
argon branches create feature-x -p my-app
argon time-travel info -p my-app -b main
argon restore preview --time "5 minutes ago"
argon snapshot create -p my-app -b main -c users
```

### SDKs
- **Go SDK** — `go get github.com/argon-lab/argon/pkg/walcli`
- **Python SDK** — `pip install argon-mongodb`, including MLflow, DVC, and
  Weights & Biases integrations for experiment tracking

### How writes are captured (current, honest)
Writes go through the Argon driver/SDK, which resolves each operation against
branch state once, at write time, and logs the outcome as put/delete entries
with full document images:

- **Real result counts** — matched/modified/deleted/upserted, duplicate-key
  errors on insert
- **Broad update-operator support** — `$set`, `$unset`, `$inc`, `$mul`,
  `$min`, `$max`, `$rename`, `$push`, `$addToSet`, `$pull`, `$pop`,
  `$setOnInsert`, `$currentDate`; integer types are preserved
- **Loud failures** — unsupported query or update operators return errors
  instead of being silently ignored
- ⚠️ **Writes made directly to MongoDB with a native driver bypass the WAL
  today.** True drop-in capture — change streams, per-branch connection
  strings, pymongo/mongoose unchanged — is Milestone 3.

## 🧠 ML/AI Workflows

- **Experiment isolation** — each experiment gets its own branch
- **Reproducible training data** — pin the exact LSN a run trained against
- **Safe exploration** — branch production-shaped data without touching
  production
- **Experiment tracking** — MLflow, DVC, and W&B integrations in the Python
  SDK

## 📈 Performance

We removed the performance table that used to live here: its numbers had no
reproducible benchmark behind them. M2 ships a public benchmark repo
(`docker compose up`), and every number we publish will link to a run you can
reproduce. What we can say structurally today:

- Branch creation cost is **one metadata write**, independent of data size
- Snapshots keep time-travel replay **bounded** regardless of history length
- WAL entries are compressed per entry (zstd)

## ⚠️ Current Limitations (deliberate scope)

- Reads materialize in memory: no indexes, no aggregation pipeline; Find
  options (sort/skip/limit/projection) are not applied yet. M3 fixes this
  structurally by running reads on real mongod.
- No merge/diff commands yet (M4) — pre-images already record the data they
  will need.
- All WAL entries live in MongoDB; cold-history offload to object storage is
  the remainder of M2.

## 🔮 Roadmap

| Milestone | Scope | Status |
|---|---|---|
| **M1 · Correctness** | Deterministic replay (property-tested), distributed LSN sequencer, branch ancestry isolation, truthful write results, WAL v2 migration | ✅ Shipped |
| **M2 · Bounded time travel** | Snapshots that bound replay depth (✅ merged), WAL segmentation + GC, **public reproducible benchmarks** | 🚧 In progress |
| **M3 · True drop-in** | One physical MongoDB database per branch, change-stream capture, per-branch connection strings, `argon undo --session` | Planned |
| **M4 · Merge & diff** | Document-level diff, three-way merge, reviewable data PRs | Planned |
| **M5 · Agent ecosystem** | MCP server, LangGraph checkpointer, TTL sandboxes, eval pinning | Planned |

Full roadmap: https://www.argonlabs.tech/roadmap

---

For detailed technical documentation, see:
- [Quick Start Guide](QUICK_START.md) - Get running in 5 minutes
- [API Reference](API_REFERENCE.md) - Complete CLI command reference
- [Architecture Guide](ARCHITECTURE.md) - WAL system design details
- [Use Cases](USE_CASES.md) - Real-world ML workflow examples
