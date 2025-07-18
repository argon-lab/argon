#!/bin/bash

# Create GitHub Release Script
# This script helps create a GitHub release with binaries

set -e

VERSION="1.0.0"
REPO="argon-lab/argon"

echo "🚀 Creating GitHub Release v$VERSION"
echo "===================================="

# Check if gh CLI is installed
if ! command -v gh &> /dev/null; then
    echo "❌ GitHub CLI (gh) is not installed"
    echo "   Install it with: brew install gh"
    echo "   Or visit: https://cli.github.com/"
    exit 1
fi

# Check if logged into GitHub
if ! gh auth status &> /dev/null; then
    echo "🔐 Please log into GitHub CLI:"
    gh auth login
fi

# Check if binaries exist
if [ ! -d "dist" ]; then
    echo "📦 Building binaries first..."
    ./scripts/build_release.sh $VERSION
fi

echo "📝 Creating release with binaries..."

# Create release
gh release create "v$VERSION" \
    --title "Argon v$VERSION - Initial Release" \
    --notes "🎉 **Argon v$VERSION - Initial Release**

Git-like MongoDB branching for ML/AI workflows.

## What's New
- Complete CLI with Neon compatibility  
- S3 storage backend with 42% compression
- Python FastAPI service
- Docker development environment
- Production-ready hybrid Go+Python architecture

## Installation

### Quick Install (From Source)
\`\`\`bash
git clone https://github.com/argon-lab/argon.git
cd argon/cli && go build -o argon . && sudo mv argon /usr/local/bin/
\`\`\`

### npm
\`\`\`bash
npm install -g argonctl
\`\`\`

### Direct Download
Download the binary for your platform from the assets below.

## Usage
\`\`\`bash
argon --version
argon --help
argon projects list
\`\`\`

Built with ❤️ for the MongoDB and ML/AI communities." \
    dist/*

echo "✅ Release created successfully!"
echo "🌐 View at: https://github.com/$REPO/releases/tag/v$VERSION"