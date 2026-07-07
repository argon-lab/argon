# Argon — Git for MongoDB

[![Build Status](https://github.com/argon-lab/argon/actions/workflows/ci.yml/badge.svg)](https://github.com/argon-lab/argon/actions/workflows/ci.yml)
[![Go Report](https://goreportcard.com/badge/github.com/argon-lab/argon)](https://goreportcard.com/report/github.com/argon-lab/argon)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Homebrew](https://img.shields.io/badge/Homebrew-argonctl-orange?logo=homebrew)](https://github.com/argon-lab/homebrew-tap)
[![npm](https://img.shields.io/npm/v/argonctl?logo=npm&label=npm)](https://www.npmjs.com/package/argonctl)

**Branch, time-travel, merge and undo your MongoDB — with real drivers, real
mongod, and versioned history underneath. Built for the age of AI agents.**

## What is Argon?

Argon is a version-control engine for MongoDB. Every write becomes an entry
in a deterministic write-ahead log; branches are metadata pointers into that
log. On top of that one idea:

- **Branch in milliseconds** — creating a branch writes a pointer, not a
  copy, regardless of data size
- **Work with any MongoDB driver** — `argon checkout` materializes a branch
  into a real MongoDB database; pymongo, mongoose, the Go driver, indexes,
  aggregation and transactions all run on mongod itself, and every write is
  captured back into versioned history
- **Time-travel and undo** — materialize any historical state by LSN or
  timestamp; revert ranges of history (even one agent's writes) with
  append-only compensations
- **Merge with review** — three-way merges as persisted, reviewable plans:
  a data pull request; conflicts are never resolved silently
- **Agent-native** — TTL sandboxes, dataset pins for reproducible evals, an
  MCP server, a REST control plane, and LangGraph/Mem0 adapters
  ([argon-agents](https://github.com/argon-lab/argon-agents))

Performance numbers live in one place: the
[public benchmark suite](https://github.com/argon-lab/benchmarks), pinned
engine refs, reproducible with `docker compose up`. This README quotes none,
by policy.

## Sixty seconds

```bash
brew install argon-lab/tap/argonctl        # or: npm install -g argonctl
# MongoDB must run as a replica set (change streams) — see docs/OPERATIONS.md

# Bring an existing database in ("git clone")
argon import database --uri mongodb://localhost:27017 --database myapp --project myapp

# Branch ("git checkout -b") — a metadata write, instant at any size
argon branches create experiment -p myapp

# Materialize the branch into a real MongoDB database and get a URI
argon checkout -p myapp -b experiment
argon watch -p myapp -b experiment        # capture writes as history

# ... point any driver at the URI, write freely ...

# Review and merge the work back — a data pull request
argon diff -p myapp -b experiment
argon merge preview -p myapp -b experiment
argon merge apply <plan-id>

# Disaster recovery: preview, then rewind
argon restore preview -p myapp -b main --time 2026-07-07T09:00:00Z
argon restore reset   -p myapp -b main --time 2026-07-07T09:00:00Z --backup pre-incident
```

For agents, the same engine over MCP: `claude mcp add argon -- argon mcp`
hands your agent sandboxes, diffs, merges, undo and pins as tools.

## Status

The v2 engine is complete: every milestone of the
[rebuild plan](docs/ARCHITECTURE.md) has shipped, each merged only with CI
green.

| Milestone | Scope | Status |
|---|---|---|
| **M1 · Correctness** | Deterministic replay (property-tested), distributed LSN sequencer, branch ancestry isolation, truthful write results, WAL v2 migration | ✅ |
| **M2 · Bounded time travel** | Content-addressed snapshots bound replay depth · retention-window GC + full branch reclamation · MongoDB/S3/filesystem chunk stores · [public benchmarks](https://github.com/argon-lab/benchmarks) | ✅ |
| **M3 · True drop-in** | Physical MongoDB database per branch (`argon checkout` → URI) · change-stream capture with transaction grouping (`argon watch`) · `argon undo` with per-actor conflict detection · real-driver workloads (pymongo, mongoose) verified against WAL convergence in CI on every push | ✅ |
| **M4 · Merge & diff** | `argon diff` · three-way merges as persisted, reviewable plans (`argon merge preview/apply`) · conflicts never silent · merges undoable like any range | ✅ |
| **M5 · Agent ecosystem** | MCP server (13 tools, supervised ingesters) · TTL sandboxes · dataset pins for reproducible evals (`argon pin`) · REST control plane · LangGraph checkpointer + Mem0 factory ([argon-agents](https://github.com/argon-lab/argon-agents)) | ✅ |
| **Beyond** | Wire-protocol proxy: stable `mongodb://proxy/<project>~<branch>` connection strings (`argon proxy`) | ✅ |

Planned next: GCS chunk-store backend; synchronous capture in the wire
proxy. Honest limitations are listed in
[ARCHITECTURE.md](docs/ARCHITECTURE.md#known-limitations-and-roadmap).

## How it works

One deterministic, physical write-ahead log per project. Entries carry full
document images (zstd-compressed), so replay is a pure fold — the same
prefix always materializes the same state, byte for byte, property-tested in
CI. Branches are `(parent, fork LSN, head LSN)` pointers; snapshots are
content-addressed chunks that bound replay depth (and deduplicate across
branches); GC reclaims what snapshots cover and retention allows, and never
touches history that a live branch, a pin, or an uncovered read still
needs.

The full design — including the consistency model, stated honestly — is in
[ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Documentation

| | |
|---|---|
| [Quick start](docs/QUICK_START.md) | Install to first merge in ten minutes |
| [CLI reference](docs/CLI.md) | Every command, honestly described |
| [Architecture](docs/ARCHITECTURE.md) | How the engine works and what it guarantees |
| [Agents](docs/AGENTS.md) | Sandboxes, pins, MCP server, REST API, argon-agents |
| [Operations](docs/OPERATIONS.md) | Deployment, chunk stores, GC, v1→v2 migration |
| [Performance](docs/PERFORMANCE.md) | Where the numbers live and how to reproduce them |

## Community

- 🐛 [Issues](https://github.com/argon-lab/argon/issues)
- 💬 [Discussions](https://github.com/argon-lab/argon/discussions)
- 🏗️ [Contributing](CONTRIBUTING.md)
- 📧 [argonlabs.tech](https://www.argonlabs.tech)

---

<div align="center">

**Give your MongoDB a time machine. Branch without fear.**

⭐ **Star us** if Argon saves your day!

</div>
