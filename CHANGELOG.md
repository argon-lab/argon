# Changelog

All notable changes to the Argon project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Planned: GCS chunk-store backend; an opt-in read-your-writes barrier in
the wire proxy (hold a write's ack until the ingester confirms the WAL
entry — a synchronization barrier, not in-proxy capture).

## [2.1.6] - 2026-11-11

Security and data-correctness fixes. Read the upgrade notes marked below if
you run `argon proxy` for remote clients, reach the console under a
non-loopback hostname, or use `restore reset` together with GC.

### Security

- `argon proxy` now listens on `127.0.0.1:27018` by default instead of every
  interface. The proxy does not authenticate clients, so the previous default
  exposed the upstream MongoDB — typically an unauthenticated development
  instance — to the local network. A non-loopback `--listen` address is refused
  unless `--allow-remote` is passed. **Upgrade note:** remote clients of the
  proxy need `--listen <addr> --allow-remote`, with authentication enabled on
  the upstream deployment.
- Reject malformed wire-protocol frames in the proxy instead of crashing. A
  16-byte message from any client that could connect ended the process;
  frames are now length-checked before parsing, a bad frame closes only its
  own connection, and the parser is covered by a fuzz test.
- A control plane without `ARGON_API_TOKEN` (the `argon console` default)
  validates the `Host` header and answers only to `localhost`, `*.localhost`
  and loopback addresses, closing a DNS-rebinding path through which a web
  page could drive the local API. Set `ARGON_ALLOWED_HOSTS` for tunnels or
  reverse proxies that use another hostname.
- Ignore `X-Forwarded-For` unless `ARGON_TRUSTED_PROXIES` lists the proxies
  allowed to set it, so the demo write budget cannot be reset by forging the
  header.
- Build with Go 1.26.11 to address
  [GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859): an HTTP/1 server
  using `MaxBytesReader` can panic when a handler reads the request body while
  the response header is being written. The API server limits request bodies
  this way, so the scanner reports the fix as reachable; Argon's handlers read
  the body before responding, and no crash was reproduced. Source builds on
  the Go 1.27 line require 1.27.4 or newer.

### Fixed

- **Backup branches and pins survive repeated resets.** Resetting a branch
  twice with no write in between ended the second discarded range at the
  already rewound head. Views captured before the first reset — a `--backup`
  branch, a fork, a pin — then silently lost the history between the two
  reset targets. A range now ends at the branch's high-water mark. Ranges
  recorded by earlier releases are read with the corrected end, so affected
  backups and pins read correctly again after upgrading; no migration is
  needed.
- **A reset can no longer land inside discarded history.** Resetting to an
  LSN (or `--time`) that an earlier reset had discarded showed the discarded
  writes at the head until the next write hid them again, and the
  snapshot-accelerated read path could keep returning them. Such a reset is
  now refused with the nearest valid target. Discarded history remains
  readable and forkable up to the high-water mark — also immediately after a
  reset, which previously required a later write — so `argon restore branch`
  recovers it. If you reset into discarded history with an earlier release
  and still need those documents, fork that LSN.
- **Snapshots taken inside a discarded window serve only readers of that
  window.** A snapshot at an LSN inside an already discarded range holds the
  pre-reset history, but was treated as valid for everyone and could
  resurrect discarded documents; GC could then reclaim the entries that
  proved otherwise. Usability is now decided from what the snapshot actually
  left out.
- **GC keeps every LSN inside the retention window readable.** The reclaim
  cutoff was the retention boundary itself. A read just above it replays from
  the newest snapshot below, across entries GC had deleted, and returned a
  state missing those writes without an error. The cutoff is now the newest
  generally valid snapshot at or below the boundary, and the boundary accounts
  for entries whose timestamps are slightly out of LSN order.
- **Positions GC has reclaimed past are refused instead of misread.** A
  time-travel query, reset, fork, pin or undo that needed entries GC had
  deleted replayed across the gap and produced a state without those writes
  — after `restore reset`, as the branch's new head. GC now records how far it
  reclaimed per branch and collection (`wal_reclaim_floors`), reads that
  would cross that floor fail with `history reclaimed by garbage collection`,
  and those commands check before changing anything. Heads, forks, pins,
  positions on a retained snapshot and everything inside the retention
  window are unaffected. Floors are recorded from this release on.
- `argon restore reset --backup` validates the target before creating the
  backup branch, `restore preview` fails the way the reset would and no longer
  counts entries an earlier reset already discarded, and `time-travel query`
  without `--collection` rejects an LSN beyond the branch's history like the
  single-collection form.

### Changed

- Resolving `--time` reads one index entry instead of loading every WAL entry
  of the project up to that time.

### Removed

- A 14 MB `cli/argon` executable that had been committed to the repository by
  mistake. It was a development build of the long-superseded v1 CLI (July 2025,
  Go 1.24) and was never part of a release, but it shipped in every source
  archive. Released binaries come only from the release workflow.

## [2.1.5] - 2026-11-09

### Security

- Update `github.com/klauspost/compress` to v1.20.2 for
  [GO-2026-6865](https://pkg.go.dev/vuln/GO-2026-6865), a decoder panic on
  crafted Zstandard frames. The Go toolchain requirement is unchanged.

### Fixed

- Accept an explicit `--lsn 0` for restore preview, branch, and reset when the
  branch permits that target, and reject requests that also supply `--time`.

## [2.1.4] - 2026-11-02

### Fixed

- Reject zero or negative sandbox TTL extensions before changing expiry. The REST
  endpoint returns HTTP 400 for invalid durations, preventing an invalid extension
  from making the sandbox eligible for the next cleanup sweep.

### Security

- Build with Go 1.26.10 to address
  [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809), which adds distrust
  of known compromised intermediate certificates in `crypto/x509`.
  Source builds on the Go 1.27 line require 1.27.3 or newer.
- Update the console build tool to Vite 8.3.5, removing the development-server
  and editor-launch advisories affecting Vite 8.3.0–8.3.2. Rebuild the embedded
  assets from the public source and lockfile; application source behavior and
  the CSS theme are unchanged.
- Run the required reachable Go scans with official govulncheck 1.9.0.

## [2.1.3] - 2026-10-19

### Security

- Build with Go 1.26.9 and update the API/CLI HTTP dependencies to
  `golang.org/x/net` 0.60.0, addressing the October 2026 reachable
  standard-library and HTTP vulnerabilities. Source builds on the Go 1.27
  line require 1.27.2 or newer.
- Remove vulnerable Tailwind 3 build dependencies and update source-map-js
  to 1.2.2. Full npm auditing and reachable Go vulnerability gates remain
  required.

### Changed

- The embedded console uses Tailwind CSS 4.3.3 with the existing brand theme,
  button affordances and keyboard focus styling. Its browser baseline is
  Safari 16.4+, Chrome 111+, and Firefox 128+.
- CI uses the same immutable MongoDB 7.0.45 image for every replica-set
  fixture and records container logs when startup fails.

## [2.1.2] - 2026-09-24

### Fixed

- REST branch creation drains the live parent's capture before fixing its fork
  point and refuses degraded history; omitted `from` defaults to `main`.
- Concurrent engine/CLI startup preserves the current unique WAL index instead
  of dropping and rebuilding it on every connection. Legacy definitions still
  migrate when needed.
- Canceling capture while enabling images for a new collection preserves the
  resumable boundary and stops cleanly instead of marking a permanent history gap.
- Go consumers use valid `github.com/argon-lab/argon/v2` and
  `github.com/argon-lab/argon/api/v2` modules, with separate matching release tags.
- Console Undo applies the exact actor and bounded LSN range just reviewed;
  changing scope invalidates review, including delayed preview responses.
- Expired/reset demo sessions offer a fresh start, clear stale queries and
  recover old deep links. Late old-session failures cannot end a renewed session.

### Import and release hardening

- Real imports require `--source-quiesced`: stop source writes and DDL for the
  entire copy. Preflight rejects unreadable/unsupported collections before
  creating target data. Imports remain hidden until successful completion.
- Failed or canceled imports remove their unpublished project/history so the
  name can be retried. `argon import cleanup --project P --yes` recovers an
  unfinished import after a killed process; completed projects are protected.
- Release publication depends on the complete CI suite for the exact tagged
  source, consistent committed version metadata and matching root/API tags.
  CI also compiles an external Go consumer of both v2 modules.
- The MIT console source is public in `web/`. Locked builds reproduce the
  embedded assets; browser regressions run against the same engine commit.
  Release provenance records engine/UI source and every binary checksum.
- Publishing and performance documentation now describes the actual release
  flow, source-quiescence contract and completed small-scale benchmark evidence.

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

[Unreleased]: https://github.com/argon-lab/argon/compare/v2.1.6...HEAD
[2.1.6]: https://github.com/argon-lab/argon/releases/tag/v2.1.6
[2.1.5]: https://github.com/argon-lab/argon/releases/tag/v2.1.5
[2.1.4]: https://github.com/argon-lab/argon/releases/tag/v2.1.4
[2.1.3]: https://github.com/argon-lab/argon/releases/tag/v2.1.3
[2.1.2]: https://github.com/argon-lab/argon/releases/tag/v2.1.2
[2.1.1]: https://github.com/argon-lab/argon/releases/tag/v2.1.1
[2.1.0]: https://github.com/argon-lab/argon/releases/tag/v2.1.0
[2.0.1]: https://github.com/argon-lab/argon/releases/tag/v2.0.1
[2.0.0]: https://github.com/argon-lab/argon/releases/tag/v2.0.0
