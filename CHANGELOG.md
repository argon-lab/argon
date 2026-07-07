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

## [1.0.1] - 2025-07-20

### Added
- **Pure WAL Architecture** - Complete legacy system removal (74 files, 15,849 lines)
- **Time Travel Functionality** - Query any historical database state
- **Instant Branching** - 1ms branch creation (86x faster than alternatives)
- **Package Distribution**:
  - Homebrew: `brew install argon-lab/tap/argonctl`
  - NPM: `npm install -g argonctl`
  - PyPI: `pip install argon-mongodb`
- **Production Monitoring**:
  - 119+ test coverage with comprehensive assertions
  - GitHub Actions CI/CD with MongoDB service
  - Performance metrics and health checks
- **ML Integrations**:
  - Python SDK with unified CLI interface
  - Go SDK for high-performance operations
  - Clean CLI commands without legacy prefixes

### Changed
- **Unified CLI Interface** - Clean commands (`argon projects`, `argon status`) without `wal-simple` prefixes
- **Single Architecture** - Pure WAL system replaces all legacy components
- **Performance Improvements** - 37,905+ ops/sec with 1ms branching
- **Documentation Overhaul** - Updated all docs to reflect current WAL architecture

### Fixed
- MongoDB connection issues in production environments
- TypeScript compatibility with Next.js 15
- OAuth redirect URI handling
- SSL/TLS connection problems with MongoDB Atlas

### Security
- Implemented user data isolation preventing cross-tenant access
- Added rate limiting to prevent API abuse
- Secure authentication with industry-standard OAuth
- Proper error handling without information leakage

## [0.9.0] - 2024-11-15

### Added
- Initial MongoDB branching engine
- Basic CLI implementation
- Docker development environment
- Core storage abstraction

### Changed
- Refactored architecture for scalability
- Improved error handling

## [0.8.0] - 2024-10-30

### Added
- MongoDB change streams integration
- Basic branch operations (create, list, delete)
- REST API endpoints
- Initial documentation

### Fixed
- Memory leaks in change stream processing
- Concurrent access issues

## [0.7.0] - 2024-10-15

### Added
- Initial project structure
- Basic MongoDB operations
- Simple CLI interface

### Known Issues
- Performance bottlenecks with large datasets
- Limited error handling
- No authentication system

---

## Version History

| Version | Release Date | Key Features |
|---------|-------------|--------------|
| 1.0.0   | 2024-12-18  | Production-ready platform with auth, multi-tenancy, rate limiting |
| 0.9.0   | 2024-11-15  | MongoDB branching engine, CLI, Docker environment |
| 0.8.0   | 2024-10-30  | Change streams, REST API, initial documentation |
| 0.7.0   | 2024-10-15  | Initial project, basic MongoDB operations |

## Migration Guide

### From 0.9.x to 1.0.0

1. **Update CLI**: Download the latest version from releases
2. **Authentication**: Set up Google OAuth credentials for cloud features
3. **Configuration**: Update MongoDB connection strings for production
4. **API Changes**: Review API documentation for any breaking changes

### From 0.8.x to 0.9.0

1. **Docker**: Update docker-compose.yml with new service definitions
2. **Environment**: Set required environment variables
3. **Dependencies**: Update Go and Python dependencies

## Development Notes

### Performance Benchmarks

| Operation | v0.8.0 | v0.9.0 | v1.0.0 |
|-----------|--------|--------|--------|
| Branch Creation | 2-5s | 1-2s | <500ms |
| Change Processing | 1k ops/s | 5k ops/s | 15k+ ops/s |
| Storage Compression | 20% | 35% | 42.40% |
| Memory Usage | 200MB | 100MB | 30-50MB |

### Architecture Evolution

- **v0.7.0**: Monolithic Go application
- **v0.8.0**: Added REST API layer
- **v0.9.0**: Microservices with Docker
- **v1.0.0**: Hybrid Go+Python with cloud platform

## Contributing

We welcome contributions from the community! Please see our [Contributing Guide](CONTRIBUTING.md) for details on:

- Setting up the development environment
- Code style and standards
- Testing requirements
- Pull request process

## Support

- **Documentation**: [docs/](docs/)
- **Issues**: [GitHub Issues](https://github.com/argon-lab/argon/issues)
- **Discussions**: [GitHub Discussions](https://github.com/argon-lab/argon/discussions)
- **Security**: security@argonlabs.tech

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.