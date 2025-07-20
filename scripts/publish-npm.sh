#!/bin/bash

# Script to publish Argon CLI to NPM

set -e

echo "📦 Publishing Argon CLI to NPM..."
echo

# Check if we're in the right directory
if [ ! -f "npm/package.json" ]; then
    echo "❌ Error: Run this script from the argon root directory"
    exit 1
fi

# Check if npm is logged in
if ! npm whoami &> /dev/null; then
    echo "❌ Error: Not logged in to npm"
    echo "Run: npm login"
    exit 1
fi

# Update version in package.json to match git tag
CURRENT_VERSION=$(git describe --tags --abbrev=0 | sed 's/v//')
echo "📌 Current version from git tag: $CURRENT_VERSION"

cd npm

# Update package.json version
npm version $CURRENT_VERSION --no-git-tag-version

echo "🔍 Package details:"
npm pack --dry-run

echo
read -p "Ready to publish? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    npm publish --access public
    echo "✅ Published to npm successfully!"
    echo "Users can now install with: npm install -g argonctl"
else
    echo "❌ Publishing cancelled"
fi