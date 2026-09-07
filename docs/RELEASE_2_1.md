# Argon 2.1: correctness and workflow hardening

This release candidate addresses the September 2026 project review. The version
in `VERSION`, CLI, API, npm metadata and MCP manifest is 2.1.0. A prepared version
does not mean that a GitHub release, npm package or hosted deployment is live.

## Changes

| Review item | Result | Regression evidence |
| --- | --- | --- |
| P0: BSON identity | Type-preserving document keys; authoritative images repair legacy keys; ambiguous historical deletes fail closed | `internal/wal/docid_test.go`, `internal/materializer/keys_test.go`, `internal/merge/keys_test.go`, `tests/wal/hardening_core_test.go` |
| P0: exact history and undo | Exact change-stream images; missing update images degrade capture; stale or incomplete undo plans cannot partially alter data | `tests/wal/capture_hardening_test.go`, `tests/wal/undo_hardening_test.go` |
| P0: capture lifecycle | Durable readiness barrier, retry and resume, drain before release, persisted health and last-event observations | `tests/wal/capture_hardening_test.go`, `internal/ingest/errors_test.go` |
| P0: anonymous demo isolation | Native checkout and sandbox credentials disabled in demo; loopback by default; explicit authentication for public control planes; origin allowlist | `api/server/security_test.go`, `api/server/demo_test.go` |
| P1: concurrent state changes | Transactional merge and undo, head/version fencing, reset live guard, cross-process snapshot publication/GC lock | `tests/wal/hardening_core_test.go`, `tests/wal/undo_hardening_test.go` |
| P1: native driver capture | Persisted branch actor, automatic snapshot hook, unsupported DDL surfaces as degraded history | Capture regressions; `compat/run.sh` exercises PyMongo and Mongoose, convergence and actual undo |
| P1: first successful workflow | `doctor`, `collections prepare`, managed API/MCP capture and TTL cleanup, executable CLI and Python examples | `cli/cmd/cli_test.go`, `examples/pinned_agents.py` |
| P1: performance evidence | Full sandbox readiness, first query, observed capture delay, ancestry/concurrency matrix, raw quantiles and storage categories | Companion `argon-lab/benchmarks` repository, exact source provenance per run |
| P1: website and console | Demo/local CTAs, shared capability/version content, two agents from one pin, conflict/undo story, local handoff, capture health, readable mobile layouts and explicit errors | Companion website and console browser verification scripts |
| P2: publishing and automation | Shared version source; binary → GitHub release → npm → reusable MCP workflow; installer checks; usable CLI JSON | Version/installer Node tests; CLI integration tests; module build/vet checks |
| P2: agent integration | Real LangGraph invoke/async/fork, mandatory merge conflict, actual undo, Mem0 provider document round trip, executable two-agent business-data example | Companion `argon-lab/argon-agents` live-stack suite |

## Operational boundaries

- Use a current supported MongoDB 7-or-later patch with a replica set and exact
  pre/post-image permissions. The historical local 7.0.14 test environment is not
  a production version recommendation; consult the [official release notes](https://www.mongodb.com/docs/v7.0/release-notes/7.0/).
  Prepare a newly created collection before rapid updates; images cannot be
  recovered retrospectively. Missing images and unsupported drop/rename operations
  stop healthy capture instead of silently claiming complete history.
- Actor labels apply to one branch/run. Change streams do not identify individual
  application users. Use separate sandboxes for separate agents.
- Native clients must stop writing before a destructive release or discard.
  One control-plane owner should coordinate checkout/release operations. The
  transaction and snapshot protections do not promise distributed lifecycle leases.
- A snapshot/GC publication lock deliberately does not expire during an external
  object-store operation. After a crash, follow the stopped-worker recovery
  procedure in [OPERATIONS.md](OPERATIONS.md) before clearing an abandoned lock.
- Last-event delay is an observed sample, not current queue lag or an SLA.
  The benchmark separately measures majority acknowledgement to observed durable
  WAL visibility, including polling overhead.
- Serial transactional publication trades single-branch concurrent throughput for
  correct history and pre-images. Timing is always reported. Hardware-specific
  legacy speed floors require `ARGON_PERF_ASSERT=1`; data/head/history assertions
  remain mandatory in ordinary tests. See [PERFORMANCE.md](PERFORMANCE.md).
- A native sandbox is a physical database copy. Metadata branch latency and bytes
  do not describe complete sandbox readiness or total physical storage.
- Mem0 document operations run on ordinary MongoDB. Semantic vector retrieval
  additionally requires Atlas Search or a compatible MongoDB Search deployment;
  it is not covered by the plain replica-set integration suite. Search indexes
  are provisioned per physical database and are not part of versioned WAL data.
- Funnel events are optional, fixed-vocabulary counters. They contain no user or
  document identifiers and are not evidence of customer adoption or conversion
  gains. Those require measurements after rollout.

## Verification

Run the root, API and CLI module suites against an isolated MongoDB replica set.
Set the S3 test variables to include the MinIO-compatible chunk-store suite.
Run `go vet ./...` in all three modules, `compat/run.sh`, and:

```sh
node scripts/release-version.js --check
node --test scripts/release-version.test.js npm/scripts/install.test.js
```

The companion SDK uses `ARGON_REQUIRE_STACK=1` in CI so an unavailable engine
cannot produce a successful skipped integration run. Website and console builds
and browser checks live with their source. Published benchmark records must name
the exact engine source used, including any patch, rather than assuming a branch
name identifies the measured code.
