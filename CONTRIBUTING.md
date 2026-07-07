# Contributing to Argon

Thank you for your interest in contributing to Argon! This document provides guidelines and instructions for contributing to the project.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [How to Contribute](#how-to-contribute)
- [Pull Request Process](#pull-request-process)
- [Coding Standards](#coding-standards)
- [Testing Guidelines](#testing-guidelines)
- [Documentation](#documentation)
- [Community](#community)

## Code of Conduct

Please read and follow our [Code of Conduct](CODE_OF_CONDUCT.md) to ensure a welcoming environment for all contributors.

## Getting Started

1. **Fork the repository** on GitHub
2. **Clone your fork** locally:
   ```bash
   git clone https://github.com/YOUR_USERNAME/argon.git
   cd argon
   ```
3. **Add the upstream repository**:
   ```bash
   git remote add upstream https://github.com/argon-lab/argon.git
   ```
4. **Create a new branch** for your feature or fix:
   ```bash
   git checkout -b feature/your-feature-name
   ```

## Development Setup

### Prerequisites

- Go 1.21+
- Docker (for MongoDB and MinIO)
- Node 20+ and Python 3.10+ only if you touch the driver-compatibility
  harness (`compat/`)

### MongoDB (replica set required)

Change streams — and therefore most integration tests — need a replica
set. One-node is fine:

```bash
docker run -d --name argon-mongo -p 27017:27017 mongo:7 --replSet rs0
docker exec argon-mongo mongosh --quiet --eval \
  'rs.initiate({_id:"rs0", members:[{_id:0, host:"localhost:27017"}]})'
```

### Building and testing

The repo holds three Go modules: the engine (root), `cli/`, and `api/`.

```bash
go build ./... && (cd cli && go build ./...) && (cd api && go build ./...)

go test ./... -count=1            # engine + integration tests (needs the replica set)
(cd api && go test ./...)         # REST control plane

golangci-lint run ./...           # run in each module you touched
```

The S3 chunk-store tests are gated: they skip unless
`ARGON_TEST_S3_ENDPOINT` / `ARGON_TEST_S3_BUCKET` (plus `AWS_*`
credentials) point at an S3-compatible store. Locally, MinIO works:

```bash
docker run -d --name argon-minio -p 9010:9000 \
  -e MINIO_ROOT_USER=argon -e MINIO_ROOT_PASSWORD=argon12345 minio/minio server /data
ARGON_TEST_S3_ENDPOINT=http://localhost:9010 ARGON_TEST_S3_BUCKET=argon-test \
  AWS_ACCESS_KEY_ID=argon AWS_SECRET_ACCESS_KEY=argon12345 AWS_REGION=us-east-1 \
  go test ./tests/wal/ -run TestChunkStore -count=1
```

The driver-compatibility harness (`bash compat/run.sh`) runs real pymongo
and mongoose workloads against a checked-out branch and verifies WAL
convergence; CI runs it on every push.

### Two rules the tests enforce

- **Determinism**: replaying the same WAL prefix must always produce the
  same state. If your change makes replay depend on map order, wall
  clocks, or anything else nondeterministic, the property tests will fail.
- **Honest performance**: in-repo performance tests are regression
  canaries with loose thresholds, not benchmarks. Performance claims come
  only from [argon-lab/benchmarks](https://github.com/argon-lab/benchmarks).

## How to Contribute

### Reporting Bugs

1. Check if the bug has already been reported in [Issues](https://github.com/argon-lab/argon/issues)
2. If not, create a new issue using the bug report template
3. Include:
   - Clear description of the bug
   - Steps to reproduce
   - Expected vs actual behavior
   - Environment details (OS, versions, etc.)
   - Error logs or screenshots

### Suggesting Features

1. Check existing [feature requests](https://github.com/argon-lab/argon/issues?q=is%3Aissue+label%3Aenhancement)
2. Create a new issue using the feature request template
3. Describe:
   - The problem you're trying to solve
   - Your proposed solution
   - Alternative solutions considered
   - Use cases and benefits

### Code Contributions

1. **Find an issue to work on**:
   - Look for issues labeled `good first issue` or `help wanted`
   - Comment on the issue to claim it
   - Wait for maintainer approval before starting major work

2. **Write your code**:
   - Follow our coding standards
   - Write tests for new functionality
   - Update documentation as needed
   - Keep commits atomic and well-described

3. **Submit a pull request**:
   - Fill out the PR template completely
   - Reference the issue being addressed
   - Ensure all tests pass
   - Request review from maintainers

## Pull Request Process

### Before Submitting

- [ ] Run `make lint` to check code style
- [ ] Run `make test` to ensure all tests pass
- [ ] Run `make bench` if you've made performance-related changes
- [ ] Update documentation for API changes
- [ ] Add tests for new functionality
- [ ] Rebase on latest main branch

### PR Guidelines

1. **Title**: Use conventional commit format:
   ```
   feat: add branch comparison API
   fix: resolve race condition in worker pool
   docs: update deployment guide
   test: add benchmarks for storage layer
   ```

2. **Description**: Include:
   - What changes were made and why
   - Link to related issue(s)
   - Testing performed
   - Breaking changes (if any)

3. **Size**: Keep PRs focused and reasonably sized:
   - Separate refactoring from feature additions
   - Break large features into smaller PRs when possible
   - One logical change per PR

### Review Process

1. Automated checks must pass (CI, tests, linting)
2. At least one maintainer approval required
3. Address review feedback promptly
4. Maintainers will merge when ready

## Coding Standards

### Go Code

- Follow [Effective Go](https://golang.org/doc/effective_go.html) guidelines
- Use `gofmt` for formatting
- Follow naming conventions:
  ```go
  // Exported types/functions
  type BranchEngine struct {}
  func NewBranchEngine() *BranchEngine {}
  
  // Unexported
  type branchStats struct {}
  func validateBranchName() error {}
  ```
- Error handling:
  ```go
  if err != nil {
      return fmt.Errorf("failed to create branch: %w", err)
  }
  ```
- Add comments for exported types and functions

### Python Code

- Follow [PEP 8](https://www.python.org/dev/peps/pep-0008/)
- Use type hints for Python 3.8+
- Format with `black`
- Docstrings for all public functions:
  ```python
  def create_branch(name: str, parent: str = "main") -> Branch:
      """Create a new branch from parent.
      
      Args:
          name: Branch name
          parent: Parent branch (default: main)
          
      Returns:
          Created Branch object
          
      Raises:
          ValidationError: If branch name is invalid
      """
  ```

## Testing Guidelines

### Unit Tests

- Test files should be named `*_test.go` or `test_*.py`
- Use table-driven tests in Go:
  ```go
  tests := []struct {
      name     string
      input    string
      expected string
      wantErr  bool
  }{
      {"valid branch", "feature-1", "feature-1", false},
      {"invalid name", "feat/1", "", true},
  }
  ```
- Mock external dependencies
- Aim for >80% code coverage

### Integration Tests

- Place in `integration/` directory
- Test real MongoDB interactions
- Use test containers when possible
- Clean up test data after runs

### Benchmarks

- Name benchmarks `Benchmark*` in Go
- Include memory allocations (`b.ReportAllocs()`)
- Test various input sizes
- Document performance expectations

## Documentation

### Code Documentation

- Document all exported functions, types, and packages
- Include examples for complex functionality
- Keep comments up-to-date with code changes

### User Documentation

- Update relevant docs in `docs/` directory
- Follow existing structure and style
- Include code examples
- Test all examples to ensure they work

### API Documentation

- CLI changes: update `docs/CLI.md`
- REST/MCP changes: update `docs/AGENTS.md`
- Engine behavior changes: update `docs/ARCHITECTURE.md` — it is the
  authoritative description and must stay truthful

## Community

### Getting Help

- **Documentation**: Check the [docs/](docs/) directory
- **GitHub Discussions**: For questions and ideas
- **Issue Tracker**: For bugs and feature requests

### Communication Channels

- **Development discussion**: GitHub Discussions
- **Real-time discussion**: GitHub Discussions
- **Security issues**: security@argonlabs.tech

### Recognition

We value all contributions! Contributors will be:
- Listed in our [CONTRIBUTORS.md](CONTRIBUTORS.md) file
- Mentioned in release notes for significant contributions
- Invited to our contributor recognition program

## Development Tips

### Debugging

```bash
# Enable debug logging
export ARGON_LOG_LEVEL=debug

# Run with race detector
go run -race ./cmd/argon

# Profile CPU usage
go run ./cmd/argon --cpuprofile=cpu.prof
```

### Common Issues

1. **MongoDB connection fails**: Ensure MongoDB is running and accessible
2. **Import errors**: Run `go mod tidy` to update dependencies
3. **Test failures**: Check if MongoDB test instance is clean

### Useful Commands

```bash
# Run specific tests
go test -run TestBranchCreation ./engine

# Update all dependencies
go get -u ./...

# Generate mocks
go generate ./...

# Check for security issues
gosec ./...
```

## Thank You!

Your contributions make Argon better for everyone. We appreciate your time and effort in improving the project!