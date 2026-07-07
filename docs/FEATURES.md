# Argon Features Overview

> This document describes what is implemented **today**. Where a capability is
> planned but not built, it says so and names the milestone. Every performance
> number quoted here links to a run of the
> [public benchmark suite](https://github.com/argon-lab/benchmarks) you can
> reproduce with `docker compose up`. See
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
- Immutable, append-only log — the foundation for `argon diff`,
  `argon merge` (reviewable data PRs), `argon undo`, and compliance
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

All numbers below come from the
[first official run](https://github.com/argon-lab/benchmarks/blob/main/RESULTS.md)
of the public benchmark suite (engine `8bf0f1e`, MongoDB 7.0.25 in Docker on
Apple Silicon, 50k-document history). Absolute values vary with hardware —
reproduce on yours with `docker compose up`:

- **Branch creation: 0.86 ms p50 / 1.93 ms p99** on a project with a
  50k-entry history — one metadata write, independent of data size
- **Storage per branch: 479 bytes** (data+index delta, n=200)
- **Bulk import: ~48k documents/second** through `walcli.ImportDatabase`
- **Time-travel materialization: 9.5 ms at LSN 1k → 296.8 ms at 50k**
  (full replay in that run; see RESULTS.md for the honest notes on
  snapshot behavior)

## ⚠️ Current Limitations (deliberate scope)

- Time-travel and metadata-only branch reads materialize in memory; live
  branches checked out to a physical database get real mongod reads
  (indexes, aggregation, everything).
- WAL entries live in MongoDB and are reclaimed by retention-window GC once
  snapshots cover them; snapshot chunks can additionally live in an
  S3-compatible or filesystem chunk store.
- Per-operation write throughput and divergence storage amplification are
  not yet benchmarked — blocked on a public write surface
  ([#16](https://github.com/argon-lab/argon/issues/16)).

## 🔮 Roadmap

| Milestone | Scope | Status |
|---|---|---|
| **M1 · Correctness** | Deterministic replay (property-tested), distributed LSN sequencer, branch ancestry isolation, truthful write results, WAL v2 migration | ✅ Shipped |
| **M2 · Bounded time travel** | Snapshots that bound replay depth ✅ · retention-window WAL GC + full branch reclamation ✅ · S3/filesystem snapshot chunk stores ✅ · [public reproducible benchmarks](https://github.com/argon-lab/benchmarks) ✅ | ✅ Shipped |
| **M3 · True drop-in** | Physical MongoDB database per branch (`argon checkout` → connection string) ✅ · change-stream write capture ✅ · `argon undo` with per-actor conflict detection ✅ · real-driver validation in CI (pymongo + mongoose workloads, WAL capture verified canonical-byte-exact, full session undo) ✅ | ✅ Shipped |
| **M4 · Merge & diff** | `argon diff` ✅ · three-way merge with persisted, reviewable plans (`argon merge preview/apply`) ✅ · conflicts never resolved silently ✅ · merges undoable like any range ✅ | ✅ Shipped |
| **M5 · Agent ecosystem** | MCP server (`argon mcp`, 9 tools, supervised ingesters) ✅ · TTL sandboxes (`argon sandbox`) ✅ · LangGraph checkpointer + Mem0 factory (`argon-agents` on the REST API) ✅ · eval dataset pinning 🚧 | Shipped · eval pinning remaining |

Full roadmap: https://www.argonlabs.tech/roadmap

---

For detailed technical documentation, see:
- [Quick Start Guide](QUICK_START.md) - Get running in 5 minutes
- [API Reference](API_REFERENCE.md) - Complete CLI command reference
- [Architecture Guide](ARCHITECTURE.md) - WAL system design details
- [Use Cases](USE_CASES.md) - Real-world ML workflow examples
