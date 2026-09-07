# Performance

Argon's performance numbers live in exactly one place: the public,
reproducible benchmark suite.

**[github.com/argon-lab/benchmarks](https://github.com/argon-lab/benchmarks)**
— pinned engine refs, recorded environment, reproducible with
`docker compose up`; results are committed to
[RESULTS.md](https://github.com/argon-lab/benchmarks/blob/main/RESULTS.md)
there.

This is policy, not an accident. Earlier versions of these docs quoted
numbers ("1ms branching", "86x faster", "37,905+ ops/sec", "220,000+
queries/sec") that could not be traced to a reproducible run, so we removed
them all. A performance claim you cannot reproduce is marketing, and it
does not belong in documentation.

What the benchmarks measure today:

- **Branch creation latency** — a branch is a metadata write; the suite
  measures p50/p99 on projects with substantial history, and the storage
  cost per branch.
- **Snapshot effectiveness** — cold-read latency with and without
  snapshots, i.e. what bounding replay depth actually buys.
The published July 2026 baseline measures metadata branch creation, not
checkout, time to connect, first read/write, or native capture overhead.
Snapshot/time-travel samples in that baseline do not establish p95/p99 read
latency. The recorded engine revision must accompany reused figures.

Still required: end-to-end sandbox startup, native write/capture latency,
write throughput, divergence storage growth, multiple dataset sizes and
concurrency levels. These workloads can use the public `WriterFor` API and
managed capture; they are no longer blocked on the old public-writer issue.

If you publish an Argon number anywhere — a README, a blog post, a talk —
it must come from a linked RESULTS.md run. Regressions the local
regression canaries can't catch show up there; treat the suite as the
source of truth.

In-repo performance tests (`tests/wal/*_performance_test.go`) are
deliberately *regression canaries* with loose thresholds — they catch
order-of-magnitude regressions in CI, they are not benchmarks, and their
numbers must never be quoted.

Performance workloads always verify data/history correctness. Timing floors
are enabled only with `ARGON_PERF_ASSERT=1` on calibrated hardware; default
CI reports timings without treating one developer machine's throughput as
a portable correctness requirement. Record engine SHA, MongoDB version,
CPU/RAM/storage and workload parameters with every comparison.
