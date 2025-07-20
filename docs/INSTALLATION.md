# Installation Guide

Complete installation instructions for Argon CLI and SDKs.

## 🖥️ **CLI Installation**

### **macOS (Homebrew)**
```bash
# Install
brew install argon-lab/tap/argonctl

# Verify
argon --version
```

### **Cross-platform (NPM)**
```bash
# Install globally
npm install -g argonctl

# Verify
argonctl --version
```

### **From Source**
```bash
# Clone repository
git clone https://github.com/argon-lab/argon
cd argon

# Build CLI
cd cli
go build -o argon

# Add to PATH
export PATH=$PATH:$(pwd)
```

## 📚 **SDK Installation**

### **Python SDK**
```bash
# Basic installation
pip install argon-mongodb

# With ML integrations
pip install argon-mongodb[ml]

# Verify
python -c "from argon import ArgonClient; print('Success!')"
```

### **Go SDK**
```go
// Add to your go.mod
require github.com/argon-lab/argon v1.0.0

// Or install directly
go get github.com/argon-lab/argon/pkg/walcli
```

### **Node.js/JavaScript**
```bash
# CLI provides programmatic access
npm install -g argonctl

# Use in Node.js
const { exec } = require('child_process');
exec('argon projects list', callback);
```

## 🐳 **Docker**
```bash
# Pull image (coming soon)
docker pull argonlab/argon:latest

# Run with MongoDB
docker run -it \
  -e MONGODB_URI=mongodb://host.docker.internal:27017 \
  argonlab/argon:latest
```

## ⚙️ **Configuration**

### **Environment Variables**
```bash
# Enable WAL architecture (required)
export ENABLE_WAL=true

# MongoDB connection (optional, defaults to localhost)
export MONGODB_URI=mongodb://localhost:27017

# Custom database name (optional)
export ARGON_DB=argon_wal
```

### **Verify Installation**
```bash
# Check CLI
argon status

# Expected output:
# 🚀 Argon System Status:
#    Time Travel: ✅ Enabled
#    Instant Branching: ✅ Enabled
#    Performance Mode: ✅ WAL Architecture
```

## 🔧 **Troubleshooting**

### **MongoDB Connection Issues**
```bash
# Check MongoDB is running
mongosh --eval "db.adminCommand('ping')"

# Start MongoDB if needed
mongod --dbpath /path/to/data
```

### **Permission Errors (NPM)**
```bash
# Use npx to avoid global install
npx argonctl status

# Or fix npm permissions
npm config set prefix ~/.npm-global
export PATH=~/.npm-global/bin:$PATH
```

### **Build from Source Issues**
```bash
# Ensure Go 1.20+ installed
go version

# Clear module cache
go clean -modcache

# Rebuild
go build -v ./cli
```

## 📦 **Package Managers**

| Platform | Package Manager | Command |
|----------|----------------|---------|
| macOS | Homebrew | `brew install argon-lab/tap/argonctl` |
| Any OS | NPM | `npm install -g argonctl` |
| Python | PyPI | `pip install argon-mongodb` |
| Go | Go Modules | `go get github.com/argon-lab/argon` |

## 🚀 **Next Steps**

- [Quick Start Guide](./QUICK_START.md) - Get started in 5 minutes
- [CLI Reference](./CLI_REFERENCE.md) - Complete command documentation
- [Python SDK Guide](./PYTHON_SDK.md) - ML/Data Science workflows
- [Examples](../examples/) - Real-world demos