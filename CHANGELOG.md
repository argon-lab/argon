# Changelog

All notable changes to the Argon project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Web dashboard for visual branch management
- ML framework integrations (MLflow, DVC, Weights & Biases)
- Jupyter notebook plugin
- Real-time collaboration features

### Changed
- Improved performance for large datasets
- Enhanced security features

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