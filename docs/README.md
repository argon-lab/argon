# Argon documentation

Argon is a version-control engine for MongoDB: branches, time travel,
merges and undo over a deterministic write-ahead log, with real MongoDB
databases as the working surface.

Reading order:

1. **[QUICK_START.md](QUICK_START.md)** — install to first merge in ten
   minutes: import, branch, checkout, capture, merge, rewind.
2. **[CLI.md](CLI.md)** — the command reference.
3. **[AGENTS.md](AGENTS.md)** — the agent workflow: TTL sandboxes, dataset
   pins for reproducible evals, the MCP server, the REST control plane,
   the wire proxy, and the
   [argon-agents](https://github.com/argon-lab/argon-agents) Python
   package (LangGraph, Mem0).
4. **[ARCHITECTURE.md](ARCHITECTURE.md)** — how the engine works: the WAL,
   branches and ancestry, snapshots, GC, merge, the consistency model
   stated honestly, and known limitations.
5. **[OPERATIONS.md](OPERATIONS.md)** — running it: replica-set
   requirement, snapshot chunk-store backends (MongoDB/S3/filesystem),
   retention and GC, migrating v1 data.
6. **[PERFORMANCE.md](PERFORMANCE.md)** — a pointer, deliberately: every
   performance number lives in the reproducible
   [benchmark suite](https://github.com/argon-lab/benchmarks), nowhere
   else.

Contributing? Start with [CONTRIBUTING.md](../CONTRIBUTING.md).
