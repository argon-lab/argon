# Changelog

All notable changes to the Argon project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Planned: GCS chunk-store backend; synchronous capture in the wire proxy.

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