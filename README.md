<div align="right">
  <details>
    <summary >🌐 Language</summary>
    <div>
      <div align="center">
        <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=en">English</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=zh-CN">简体中文</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=zh-TW">繁體中文</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=ja">日本語</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=ko">한국어</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=hi">हिन्दी</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=th">ไทย</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=fr">Français</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=de">Deutsch</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=es">Español</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=it">Italiano</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=ru">Русский</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=pt">Português</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=nl">Nederlands</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=pl">Polski</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=ar">العربية</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=fa">فارسی</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=tr">Türkçe</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=vi">Tiếng Việt</a>
        | <a href="https://openaitx.github.io/view.html?user=argon-lab&project=argon&lang=id">Bahasa Indonesia</a>
      </div>
    </div>
  </details>
</div>

# Argon - MongoDB Time Machine 🚀

[![Build Status](https://github.com/argon-lab/argon/actions/workflows/ci.yml/badge.svg)](https://github.com/argon-lab/argon/actions/workflows/ci.yml)
[![Go Report](https://goreportcard.com/badge/github.com/argon-lab/argon)](https://goreportcard.com/report/github.com/argon-lab/argon)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

[![Homebrew](https://img.shields.io/badge/Homebrew-argonctl-orange?logo=homebrew)](https://github.com/argon-lab/homebrew-tap)
[![npm](https://img.shields.io/npm/v/argonctl?logo=npm&label=npm)](https://www.npmjs.com/package/argonctl)
[![PyPI](https://img.shields.io/pypi/v/argon-mongodb?logo=pypi&label=PyPI)](https://pypi.org/project/argon-mongodb/)

**Travel through time in your MongoDB database. Branch, restore, and experiment without fear.**

## What is Argon?

Argon gives MongoDB superpowers with **Git-like branching** and **time travel**. Create database branches in milliseconds, inspect any point in history, and rewind your mistakes.

### 🎯 Key Benefits

- **⚡ Millisecond Branches** - Creating a branch writes metadata, not data copies: [0.86 ms p50 / 1.93 ms p99 on a 50k-entry project, 479 bytes of storage per branch — measured](https://github.com/argon-lab/benchmarks/blob/main/RESULTS.md)
- **⏰ Time Travel** - Inspect and restore your data from any point in history, addressed by LSN or timestamp
- **🔬 Deterministic Replay** - The same history always reconstructs the same state, byte for byte — verified by property tests in CI
- **🔄 Safe Restore** - Preview changes before restoring; restores are themselves logged, so you can undo the undo
- **🗜️ Smart Compression** - WAL entries are automatically compressed with zstd
- **🧭 Honest Engineering** - Every performance number links to a run of the [public benchmark suite](https://github.com/argon-lab/benchmarks) that you can reproduce with `docker compose up`

> **A note on claims:** earlier versions of this README quoted numbers like "1ms branching" and "220,000+ queries/sec" that we could not back with reproducible benchmarks. We removed them. Numbers now come exclusively from [argon-lab/benchmarks](https://github.com/argon-lab/benchmarks) — pinned engine ref, recorded environment, reproducible by anyone.

## Quick Demo

```bash
# Install
brew install argon-lab/tap/argonctl    # macOS
npm install -g argonctl                 # Cross-platform

# Step 1: Import your existing MongoDB (like "git clone")
argon import database --uri "mongodb://localhost:27017" --database myapp --project myapp
# ✅ Your data now has time travel capabilities!

# Step 2: Use Argon like Git for your database
argon branches create test-env           # Branch like "git checkout -b"
argon time-travel query --project myapp --branch main --lsn 1000

# Step 3: Disaster recovery made simple
argon restore preview --time "1 hour ago"
argon restore reset --time "before disaster"
```

## Git-Like Workflow for MongoDB

### 🔄 **Step 1: Import ("git clone" for databases)**
```bash
# Bring your existing MongoDB into Argon
argon import preview --uri "mongodb://localhost:27017" --database myapp
argon import database --uri "mongodb://localhost:27017" --database myapp --project myapp
# ✅ Your existing data now has time travel capabilities!
```

### 🧪 **Step 2: Branch ("git checkout -b")**
```bash
# Create branches for testing, staging, experiments
argon branches create staging --project myapp
argon branches create experiment-v2 --project myapp
# Branches are metadata pointers - created instantly, no data copied 🚀
```

### 📊 **Step 3: Time Travel ("git log" for data)**
```bash
# See your data's history
argon time-travel info --project myapp --branch main
argon time-travel query --project myapp --branch main --lsn 1000
# Compare data across time like Git commits
```

### 🚨 **Step 4: Restore ("git reset" for disasters)**
```bash
# "Someone deleted all users!"
argon restore reset --time "5 minutes ago"
# Crisis averted in seconds, not hours
```

## How It Works

Every write goes through Argon's driver and is recorded in a **Write-Ahead Log (WAL)** as an LSN-addressed entry with full document images. That one idea powers everything else:
- Branching is a metadata write (parent, fork LSN, head LSN) — milliseconds, regardless of data size
- Time travel replays the log up to a target LSN; replay is deterministic and property-tested
- Restore is a deterministic rewind, and is itself logged

**Current integration path:** writes are captured through Argon's Go driver / SDKs and the `argon import` flow. True drop-in support — pymongo and mongoose working unchanged against per-branch connection strings — is Milestone 3 on the roadmap below, and we won't claim it before the official driver test suites pass against Argon branches.

## Status & Roadmap

Argon's engine is being rebuilt milestone by milestone, correctness first ([full roadmap](https://www.argonlabs.tech/roadmap)):

| Milestone | Scope | Status |
|---|---|---|
| **M1 · Correctness** | Deterministic replay (property-tested), distributed LSN sequencer, branch ancestry isolation, truthful write results, WAL v2 migration | ✅ Shipped |
| **M2 · Bounded time travel** | Snapshots that bound replay depth ✅ · retention-window WAL GC + full branch reclamation ✅ · S3/filesystem snapshot chunk stores ✅ · [public reproducible benchmarks](https://github.com/argon-lab/benchmarks) ✅ | ✅ Shipped |
| **M3 · True drop-in** | Physical MongoDB database per branch (`argon checkout` → connection string) ✅ · change-stream write capture ✅ · `argon undo` with per-actor conflict detection ✅ · real-driver validation in CI (pymongo + mongoose workloads, WAL capture verified canonical-byte-exact, full session undo) ✅ | ✅ Shipped |
| **M4 · Merge & diff** | `argon diff` ✅ · three-way merge with persisted, reviewable plans (`argon merge preview/apply`) ✅ · conflicts never resolved silently ✅ · merges undoable like any range ✅ | ✅ Shipped |
| **M5 · Agent ecosystem** | MCP server (`argon mcp`, 9 tools, supervised ingesters) ✅ · TTL sandboxes (`argon sandbox`) ✅ · LangGraph checkpointer 🚧 · eval dataset pinning 🚧 | MCP + sandboxes shipped · integrations remaining |

## Installation

```bash
# CLI
brew install argon-lab/tap/argonctl    # macOS
npm install -g argonctl                 # Node.js
pip install argon-mongodb               # Python SDK

# From Source
git clone https://github.com/argon-lab/argon
cd argon/cli && go build -o argon
```

## Documentation

- 📖 [Quick Start Guide](./docs/QUICK_START.md)
- 🛠️ [API Reference](./docs/API_REFERENCE.md)
- 💡 [Use Cases](./docs/USE_CASES.md)
- 🏗️ [Architecture](./docs/ARCHITECTURE.md)

## Community

- 📋 [Roadmap](https://www.argonlabs.tech/roadmap) - See what's coming
- 🐛 [Report Issues](https://github.com/argon-lab/argon/issues)
- 💬 [Discussions](https://github.com/argon-lab/argon/discussions)
- 🏗️ [Contributing](./CONTRIBUTING.md) - Help build Argon
- 📧 [Contact](https://www.argonlabs.tech)

---

<div align="center">

**Give your MongoDB a time machine. Rewind your mistakes.**

⭐ **Star us** if Argon saves your day!

[Get Started →](docs/QUICK_START.md) | [Interactive Demo →](https://www.argonlabs.tech/demo)

</div>