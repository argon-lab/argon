# Publishing to Homebrew

## Prerequisites
- GitHub releases with tagged versions
- Built binaries for macOS (Intel and Apple Silicon)

## Steps to Publish

### 1. Create a New Release
```bash
# Tag the release
git tag v1.0.0
git push origin v1.0.0

# Create GitHub release with binaries
# Upload argon-darwin-amd64 and argon-darwin-arm64
```

### 2. Update Formula
```bash
# Get the SHA256 of the release tarball
curl -L https://api.github.com/repos/argon-lab/argon/tarball/v1.0.0 | shasum -a 256

# Update homebrew-tap/argonctl.rb with new version and SHA256
```

### 3. Test Locally
```bash
# Test the formula
brew install --build-from-source ./homebrew-tap/argonctl.rb
brew test argonctl
brew audit --new --formula ./homebrew-tap/argonctl.rb
```

### 4. Publish
```bash
cd homebrew-tap
git add argonctl.rb
git commit -m "Update argonctl to v1.0.0"
git push
```

### 5. Users Install
```bash
brew install argon-lab/tap/argonctl
```

## Maintenance
- Update formula for each new release
- Consider using GitHub Actions for automated updates
- Add bottle (pre-compiled binary) support for faster installs