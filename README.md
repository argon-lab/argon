# Argon v2: Git-like MongoDB Branching for ML/AI Workflows

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://golang.org)
[![Python](https://img.shields.io/badge/Python-3.9+-3776AB?logo=python)](https://python.org)
[![MongoDB](https://img.shields.io/badge/MongoDB-7.0+-47A248?logo=mongodb)](https://mongodb.com)

> **🚀 Complete rewrite in progress!** Argon v2 brings enterprise-grade MongoDB branching with sub-500ms operations, ML-native features, and a hybrid Go+Python architecture.

## What is Argon v2?

Argon v2 is a MongoDB branching system that provides Git-like database operations optimized for ML/AI workflows. Think "Neon for MongoDB" with first-class support for data science teams.

### Key Features

- **⚡ Instant Branching**: Create database branches in <500ms regardless of size
- **🔄 Copy-on-Write**: Efficient storage with 90%+ space savings vs full copies  
- **🧠 ML-Native**: Built-in integrations with MLflow, DVC, Weights & Biases
- **🌐 Real-time**: Live change streams and WebSocket-based dashboard
- **☁️ Multi-cloud**: AWS S3, Google Cloud Storage, Azure Blob support
- **🔒 Enterprise**: Authentication, RBAC, audit logs, compliance features

## Architecture

Argon v2 uses a hybrid architecture optimizing for both performance and developer productivity:

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   CLI Tool      │    │  Web Dashboard  │    │ ML Integrations │
│   (Go Binary)   │    │   (Next.js)     │    │ (Python APIs)   │
└─────────────────┘    └─────────────────┘    └─────────────────┘
         │                       │                       │
         └───────────────────────┼───────────────────────┘
                                 │
                    ┌─────────────────┐
                    │  Python API     │
                    │  (FastAPI)      │
                    └─────────────────┘
                                 │
                    ┌─────────────────┐
                    │  Go Engine      │
                    │ (Performance)   │
                    └─────────────────┘
                                 │
                    ┌─────────────────┐
                    │    MongoDB      │
                    │ + Change Streams│
                    └─────────────────┘
```

**Performance Tier (Go)**: Change streams, branching engine, CLI, storage
**Productivity Tier (Python)**: Web APIs, ML integrations, admin features

## Quick Start

### Prerequisites

- Docker & Docker Compose
- Go 1.21+ (for CLI development)
- Python 3.9+ (for API development)

### Development Setup

```bash
# Clone the repository
git clone https://github.com/argon-lab/argon.git
cd argon

# Start the development environment
docker-compose up -d

# Verify services are running
curl http://localhost:8080/health  # Go engine
curl http://localhost:8000/docs    # Python API (when implemented)
curl http://localhost:3000         # Web dashboard (when implemented)
```

### CLI Usage (Coming Soon)

```bash
# Install the CLI
pip install argon-cli

# Initialize a project
argon init my-ml-project

# Create a branch
argon branch create experiment-1

# Make changes to your data
# ... modify MongoDB collections ...

# See what changed
argon status

# Sync changes to cloud
argon push

# Switch branches instantly
argon switch main
```

## Development Status

**🟢 Completed:**
- [x] Hybrid Go+Python architecture
- [x] MongoDB change streams processor
- [x] Core branching engine
- [x] REST API foundation
- [x] Docker development environment

**🟡 In Progress:**
- [ ] Python FastAPI service
- [ ] CLI tool implementation
- [ ] Storage engine with compression
- [ ] Web dashboard

**🔴 Planned:**
- [ ] ML tool integrations (MLflow, DVC)
- [ ] Real-time WebSocket updates
- [ ] Advanced branch operations (merge, diff)
- [ ] Enterprise features (auth, RBAC)

## Performance Targets

| Metric | Target | Current Status |
|--------|--------|----------------|
| Branch Creation | <500ms | 🟡 In Development |
| Change Processing | 10,000+ ops/sec | 🟡 In Development |
| Storage Efficiency | 90%+ reduction | 🔴 Not Started |
| CLI Startup | <50ms | 🔴 Not Started |

## Use Cases

### Data Science Teams
```python
# In Jupyter notebook
import argon

# Create experiment branch
argon.branch.create("model-v2-experiment")

# Train model with versioned data
model = train_model(argon.data.get_collection("training_data"))

# Track experiment metadata
argon.experiment.log(model_accuracy=0.95, dataset_version="v2.1")

# Merge successful experiment
argon.branch.merge("model-v2-experiment", "main")
```

### Development Teams
```bash
# Create feature branch with production data copy
argon branch create feature-new-analytics --from production

# Develop and test against real data
# ... make database schema changes ...

# Review changes before merge
argon diff main..feature-new-analytics

# Deploy to production
argon branch merge feature-new-analytics main
```

## Contributing

We welcome contributions! This is an open-source project built for the community.

### Development Workflow

1. **Fork the repository**
2. **Set up development environment**: `docker-compose up -d`
3. **Make changes** in the appropriate service:
   - Go engine: `services/engine/`
   - Python API: `services/api/`
   - Web dashboard: `services/web/`
4. **Test your changes**: Run the test suite
5. **Submit a pull request**

### Project Structure

```
argon/
├── services/
│   ├── engine/          # Go performance engine
│   ├── api/             # Python FastAPI service
│   └── web/             # Next.js web dashboard
├── docs/                # Documentation
├── examples/            # Example usage and tutorials
├── scripts/             # Development and deployment scripts
└── docker-compose.yml   # Development environment
```

## Roadmap

### v2.0 (Current) - Foundation
- Hybrid Go+Python architecture
- Core branching operations
- MongoDB change streams
- Basic CLI and web interface

### v2.1 - ML Integration
- MLflow connector
- DVC integration
- Weights & Biases support
- Jupyter notebook examples

### v2.2 - Enterprise Features
- User authentication and RBAC
- Team collaboration features
- Advanced branch operations
- Performance optimization

### v2.3 - Scale & Polish
- Multi-region deployment
- Advanced analytics
- Plugin architecture
- Enterprise support

## Architecture Deep Dive

For detailed technical documentation, see:
- [Architecture Overview](docs/architecture.md)
- [API Documentation](docs/api.md)
- [Development Guide](docs/development.md)
- [Deployment Guide](docs/deployment.md)

## Community

- **GitHub Discussions**: Ask questions and share ideas
- **Discord**: Real-time chat with the community (link coming soon)
- **Twitter**: Follow [@argondb](https://twitter.com/argondb) for updates

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Built by MongoDB Engineers

Argon v2 is built with deep MongoDB expertise, leveraging advanced features like change streams, optimized aggregation pipelines, and performance best practices learned from production deployments.

---

**⭐ Star this repository if you find it useful!**

[![GitHub stars](https://img.shields.io/github/stars/argon-lab/argon?style=social)](https://github.com/argon-lab/argon)