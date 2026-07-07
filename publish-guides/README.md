# Publishing Argon Packages

This directory contains guides for publishing Argon to various package managers.

## 📦 Package Publishing Status

| Package | Platform | Status | Install Command |
|---------|----------|---------|----------------|
| CLI | Homebrew | ✅ Ready | `brew install argon-lab/tap/argonctl` |
| CLI | NPM | ✅ Ready | `npm install -g argonctl` |
| Python (agents) | PyPI | ✅ Ready | `pip install argon-agents` |
| Go engine | Go Modules | ✅ Ready | `go get github.com/argon-lab/argon` |

The legacy `argon-mongodb` PyPI package targeted the v1 engine and is
frozen; its in-repo source was removed with v2. The v2 Python surface is
the REST-based `argon-agents` package.

## 🚀 Quick Publishing Checklist

### 1. Create GitHub Release
```bash
# Tag the version
git tag v2.0.0
git push origin v2.0.0

# Build binaries for all platforms
make build-all-platforms

# Create release on GitHub with binaries
```

### 2. Update Package Versions
- Homebrew: `homebrew-tap/argonctl.rb` - update version and SHA256
- NPM: `npm/package.json` - update version
- Python: `pyproject.toml` - update version

### 3. Publish Packages
```bash
# Homebrew (push to homebrew-tap repo)
cd homebrew-tap && git push

# NPM
cd npm && npm publish --access public

# PyPI
python -m build && python -m twine upload dist/*
```

## 📚 Detailed Guides
- [Homebrew Publishing](./homebrew.md)
- [NPM Publishing](./npm.md)
- [PyPI Publishing](./pypi.md)

## 🔑 Required Accounts
- **NPM**: Create account at https://npmjs.com
- **PyPI**: Create account at https://pypi.org
- **Homebrew**: Just need GitHub repo access

## 📧 Support
For publishing issues: support@argonlabs.tech