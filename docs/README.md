# Argon documentation

Argon versions MongoDB the way Git versions code: branch → work with any
driver → diff → merge or undo, with real MongoDB databases as the working
surface and versioned history underneath.

Reading order:

1. **[QUICK_START.md](QUICK_START.md)** — install → first merge, step by
   step.
2. **[CLI.md](CLI.md)** — every command.
3. **[AGENTS.md](AGENTS.md)** — the agent flow: TTL sandboxes, dataset
   pins for reproducible evals, MCP server, REST API + web console,
   [argon-agents](https://github.com/argon-lab/argon-agents) (LangGraph,
   Mem0), wire proxy.
4. **[ARCHITECTURE.md](ARCHITECTURE.md)** — how the engine works: the
   WAL, branches, snapshots, GC, merge, the consistency model stated
   honestly, known limitations.
5. **[OPERATIONS.md](OPERATIONS.md)** — running it: replica-set
   requirement, snapshot chunk stores (MongoDB/S3/filesystem), retention
   and GC, migrating v1 data.
6. **[PERFORMANCE.md](PERFORMANCE.md)** — a pointer, deliberately: every
   number lives in the reproducible
   [benchmark suite](https://github.com/argon-lab/benchmarks), nowhere
   else.

Contributing? Start with [CONTRIBUTING.md](../CONTRIBUTING.md).
