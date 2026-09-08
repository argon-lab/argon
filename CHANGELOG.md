# Changelog

All notable changes to the Argon project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Planned: GCS chunk-store backend; an opt-in read-your-writes barrier in
the wire proxy (hold a write's ack until the ingester confirms the WAL
entry — a synchronization barrier, not in-proxy capture).

## [2.1.1] - 2026-09-07

### Fixed

- Fix a monitor mutex deadlock that could leave `argon console` hanging on
  shutdown after its first health check. Alert updates now reuse the lock
  held by the health loop. Regression tests cover ticker shutdown, repeated
  failures and recovery.

## [2.1.0] - 2026-09-07

Correctness and workflow hardening for native MongoDB writes, reviewed
merges and undo. See the [release notes](docs/RELEASE_2_1.md) for regression
coverage and operating requirements.

### Added

- `argon doctor` checks connectivity, replica-set readiness, capture
  permissions, exact document images and transactions with temporary probes.
- `argon collections prepare` enables the document images needed to capture
  updates to new collections before application writes begin.
- Capture status includes persisted health, the last captured event and its
  observed delay. The delay is an event sample, not a live queue-lag estimate.
- Executable CLI and Python examples for pinned agent runs and native writes.

### Fixed

- Preserve BSON types in document keys. Repair legacy keys from authoritative
  document images and refuse ambiguous historical deletes.
- Capture exact document images for replay and undo. Missing update images or
  unsupported collection drop/rename operations mark history as degraded.
- Wait for durable capture readiness, retry transient failures, resume capture
  and drain acknowledged writes before release. Persist branch actor labels
  and trigger snapshots from native capture.
- Apply merge and undo changes transactionally with head and version checks.
  Reject stale plans and incomplete undo history before partial data changes.
- Coordinate snapshot publication and garbage collection across processes,
  and refuse reset while a branch is checked out.
- Make CLI JSON output usable by callers and validate installer behavior
  against published release assets.

### Security

- Disable native MongoDB checkout and sandbox credentials in the anonymous
  demo. Keep native-driver examples on the local deployment path.
- Bind control-plane listeners to loopback by default. Public listeners
  require explicit authentication outside restricted demo mode; cross-origin
  requests require an allowed origin.

### Changed

- Use one version source for the CLI, npm package and MCP manifest. Release
  automation publishes binaries before npm and the MCP Registry.
- Expand CI coverage for native-driver capture and undo, all Go modules,
  release metadata, installers and reachable dependency vulnerabilities.
  Source builds require Go 1.26.6 or newer.
- Document physical sandbox copying costs, retained-history requirements,
  branch/run actor attribution and capture lifecycle constraints. Benchmark
  reports distinguish metadata creation from full sandbox readiness.

## [2.0.1] - 2026-07-09

### Added

- `argon console` with an embedded browser UI, REST read endpoints and
  configurable server options.
- A scoped anonymous demo mode with request limits and sandbox cleanup.
- MCP Registry metadata and a GitHub OIDC publishing workflow. The npm
  package includes the server's `mcpName`.

### Fixed

- Recover managed capture after a control-plane restart.
- Include npm executable entry points in the published package and select
  the `.exe` release asset on Windows.
- Serve the console HTML without caching while caching hashed assets.

## [2.0.0] - 2026-07-07

The v2 engine: a ground-up rebuild around a deterministic physical WAL.
Every milestone of the rebuild plan shipped, each merged only with CI
green. See docs/ARCHITECTURE.md for the full design.

### Added
- **Deterministic replay** — WAL entries carry full document images
  (zstd-compressed); replay is a pure fold, property-tested (repeated,
  cross-instance, historical, cross-database convergence)
- **Distributed LSN sequencer** — per-project counters; correct under
  concurrent writers across processes
- **Snapshots** — content-addressed, deduplicated chunk layers that bound
  replay depth; automatic (per ~1000 entries and post-import) and manual;
  parallel chunk decode; MongoDB, S3 (MinIO/R2) and filesystem backends
- **Retention-window GC** — reclaims covered, out-of-window entries;
  respects live children's fork points and pins; full reclamation of
  deleted branches
- **Checkout: real MongoDB per branch** — `argon checkout` materializes a
  branch into a physical database any driver can use; `argon connect`,
  `argon release`
- **Change-stream capture** — `argon watch` turns direct driver writes
  into versioned history (resume tokens, transaction grouping, per-actor
  attribution); real-driver workloads (pymongo, mongoose) verified against
  WAL convergence in CI
- **Undo** — `argon undo` reverts LSN ranges or a single actor's writes
  with append-only compensations and conflict detection
- **Restore** — `argon restore preview/reset/branch`; resets record
  discarded ranges (recorded, not destructive), `--backup` forks first
- **Merge & diff** — three-way merges as persisted, reviewable plans
  (`argon diff`, `argon merge preview/apply`); exactly-once apply, stale
  heads refused, conflicts never silent
- **Agent sandboxes** — `argon sandbox`: fork + checkout + TTL in one
  step; sweep reaps expired sandboxes
- **Dataset pins** — `argon pin`: named immutable branch states that
  survive GC and resets forever; branch or sandbox from a pin for
  reproducible evals
- **MCP server** — `argon mcp`: 13 tools over stdio with supervised
  ingesters
- **REST control plane** — `api/`: projects, branches, checkout,
  sandboxes, diff/merge, undo, snapshots, pins; supervised ingesters
- **Wire-protocol proxy** — `argon proxy`: stable
  `mongodb://host/<project>~<branch>` connection strings
- **Import** — `argon import` brings existing databases in with automatic
  post-import snapshots
- **v1→v2 migration** — `argon migrate-wal` rewrites expression entries
  into deterministic document images, idempotently
- **argon-agents** (separate package) — LangGraph checkpointer with
  whole-store fork, Mem0 sandbox factory, REST client

### Changed
- All external performance claims now come exclusively from the
  reproducible public benchmark suite (argon-lab/benchmarks)
- Documentation rewritten around what is implemented and verified

### Removed
- In-process Mongo emulation (filter/update evaluation in Go) — mongod is
  the only query engine; expression evaluation survives solely for v1
  migration
- Unverifiable performance claims throughout docs and CLI output

## [1.0.0] - 2025-07-17, [1.0.1] - 2025-07-20

The v1 engine. Superseded wholesale by v2.0.0 and no longer supported;
`argon migrate-wal` converts v1 WAL data to v2. Published to Homebrew,
npm and (as `argon-mongodb`, now frozen) PyPI.

Earlier changelog entries claiming a 2024 release history, and the
performance/benchmark tables that accompanied them, were inaccurate — the
project's first commit is 2025-05-27 — and have been removed. Performance
numbers now live only in the reproducible benchmark suite
(https://github.com/argon-lab/benchmarks).

[Unreleased]: https://github.com/argon-lab/argon/compare/v2.1.1...HEAD
[2.1.1]: https://github.com/argon-lab/argon/releases/tag/v2.1.1
[2.1.0]: https://github.com/argon-lab/argon/releases/tag/v2.1.0
[2.0.1]: https://github.com/argon-lab/argon/releases/tag/v2.0.1
[2.0.0]: https://github.com/argon-lab/argon/releases/tag/v2.0.0
