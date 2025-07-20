# Quick Start Guide

Get up and running with Argon MongoDB branching in under 5 minutes.

## 📦 **Installation**

### Option 1: Homebrew (Recommended)
```bash
brew install argon-lab/tap/argonctl
```

### Option 2: Direct Download
```bash
# Download latest release
curl -L https://github.com/argon-lab/argon/releases/latest/download/argon-darwin-arm64 -o argon
chmod +x argon && sudo mv argon /usr/local/bin/
```

### Option 3: NPM (Coming Soon)
```bash
npm install -g argonctl  # (pending publication)
```

## 🚀 **First Steps**

### 1. Enable WAL and Check Status
```bash
export ENABLE_WAL=true
argon wal-simple status
```
**Expected Output:**
```
WAL System Status:
  Enabled: true
  Connection: OK
  Current LSN: 2
```

### 2. Create Your First Project
```bash
argon wal-simple project create my-app
```
**Expected Output:**
```
Created WAL-enabled project 'my-app' (ID: 6a7c9e12c395913d7800d91f)
Default branch: main (LSN: 4)
```

### 3. List Projects
```bash
argon wal-simple project list
```
**Expected Output:**
```
WAL-Enabled Projects:
  - my-app (ID: 6a7c9e12c395913d7800d91f)
    Branches: 1
```

### 4. View Time Travel Information
```bash
argon wal-simple tt-info -p 6a7c9e12c395913d7800d91f -b main
```
**Expected Output:**
```
Time Travel Info for branch 'main':
  Branch ID: 6a7c9e12c395913d7800d91f
  LSN Range: 0 - 4
  Total Entries: 0
```

## 🎉 **You're Ready!**

You now have a working MongoDB project with:
- ✅ **Git-like branching** enabled
- ✅ **Time travel** capabilities  
- ✅ **Zero-copy** branch creation
- ✅ **Complete audit trail** via WAL

## 🔄 **Next Steps**

### For Developers
- [Go SDK Guide](./GO_SDK.md) - Integrate with Go applications
- [CLI Reference](./CLI_REFERENCE.md) - Full command documentation

### For Data Scientists
- [Python SDK Guide](./PYTHON_SDK.md) - ML workflow integration
- [Jupyter Integration](./ML_INTEGRATIONS.md) - Notebook experiments

### For Production
- [Deployment Guide](./PRODUCTION_DEPLOYMENT_GUIDE.md) - Enterprise setup
- [Security Guide](./SECURITY.md) - Production security

## ❓ **Need Help?**

- [FAQ](./FAQ.md) - Common questions
- [Troubleshooting](./TROUBLESHOOTING.md) - Common issues
- [GitHub Issues](https://github.com/argon-lab/argon/issues) - Report bugs
- [Support](./SUPPORT.md) - Get community help