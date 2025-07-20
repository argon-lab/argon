# Publishing to NPM

## Prerequisites
- NPM account with access to publish
- GitHub releases with platform binaries

## Initial Setup
```bash
# Login to npm
npm login

# Add npm organization (if needed)
npm org create argon-lab
```

## Steps to Publish

### 1. Build Binaries for All Platforms
```bash
# Build for all platforms
GOOS=darwin GOARCH=amd64 go build -o dist/argon-darwin-amd64 ./cli
GOOS=darwin GOARCH=arm64 go build -o dist/argon-darwin-arm64 ./cli
GOOS=linux GOARCH=amd64 go build -o dist/argon-linux-amd64 ./cli
GOOS=windows GOARCH=amd64 go build -o dist/argon-windows-amd64.exe ./cli

# Upload to GitHub release
```

### 2. Update Package Version
```bash
cd npm
npm version 1.0.0
```

### 3. Test Locally
```bash
# Pack the package
npm pack

# Test installation
npm install -g argonctl-1.0.0.tgz
argon --version
argonctl --version

# Uninstall test
npm uninstall -g argonctl
```

### 4. Publish to NPM
```bash
# Dry run first
npm publish --dry-run

# Publish (public package)
npm publish --access public
```

### 5. Users Install
```bash
npm install -g argonctl
```

## Important Notes
- The install script downloads platform-specific binary from GitHub releases
- Ensure GitHub release is created before npm publish
- Update download URLs in scripts/install.js if needed
- Consider using npm organization scope: @argon-lab/argon