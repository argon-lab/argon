# Quick Start Guide

Get up and running with Argon MongoDB branching in under 5 minutes.

## 📦 **Installation**

### Option 1: Homebrew (macOS)
```bash
brew install argon-lab/tap/argonctl
```

### Option 2: NPM (Cross-platform)
```bash
npm install -g argonctl
```

### Option 3: Python SDK
```bash
# Install SDK with CLI wrapper
pip install argon-mongodb

# With ML integrations
pip install argon-mongodb[ml]
```

### Option 4: From Source
```bash
git clone https://github.com/argon-lab/argon
cd argon/cli && go build -o argon
```

## 🚀 **First Steps**

### 1. Enable WAL and Check Status
```bash
export ENABLE_WAL=true
argon status
```
**Expected Output:**
```
🚀 Argon System Status:
   Time Travel: ✅ Enabled
   Instant Branching: ✅ Enabled
   Performance Mode: ✅ WAL Architecture
```

### 2. Create Your First Project
```bash
argon projects create my-app
```
**Expected Output:**
```
✅ Created project 'my-app' with time travel enabled
   Project ID: 6a7c9e12c395913d7800d91f
   Default branch: main
```

### 3. List Projects
```bash
argon projects list
```
**Expected Output:**
```
📁 Projects with Time Travel:
  - my-app (ID: 6a7c9e12c395913d7800d91f)
    Branches: 1
```

### 4. View Time Travel Information
```bash
argon time-travel info -p 6a7c9e12c395913d7800d91f -b main
```
**Expected Output:**
```
⏰ Time Travel Info for branch 'main':
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
- **Go SDK**: `go get github.com/argon-lab/argon/pkg/walcli`
- [CLI Reference](./CLI_REFERENCE.md) - Full command documentation

### For Data Scientists
- **Python SDK**: `pip install argon-mongodb[ml]`
- [Jupyter Integration](./ML_INTEGRATIONS.md) - Notebook experiments

### For Production
- [Deployment Guide](./PRODUCTION_DEPLOYMENT_GUIDE.md) - Enterprise setup
- [Security Guide](./SECURITY.md) - Production security

## ❓ **Need Help?**

- [FAQ](./FAQ.md) - Common questions
- [Troubleshooting](./TROUBLESHOOTING.md) - Common issues
- [GitHub Issues](https://github.com/argon-lab/argon/issues) - Report bugs
- [Support](./SUPPORT.md) - Get community help