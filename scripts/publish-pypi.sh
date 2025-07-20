#!/bin/bash

# Script to publish Argon Python SDK to PyPI

set -e

echo "🐍 Publishing Argon Python SDK to PyPI..."
echo

# Check if we're in the right directory
if [ ! -f "pyproject.toml" ]; then
    echo "❌ Error: Run this script from the argon root directory"
    exit 1
fi

# Check required tools
if ! command -v python3 &> /dev/null; then
    echo "❌ Error: Python 3 is required"
    exit 1
fi

if ! python3 -m pip show build &> /dev/null; then
    echo "📦 Installing build tools..."
    python3 -m pip install --upgrade build twine
fi

# Clean previous builds
echo "🧹 Cleaning previous builds..."
rm -rf dist/ build/ *.egg-info

# Update version in pyproject.toml to match git tag
CURRENT_VERSION=$(git describe --tags --abbrev=0 | sed 's/v//')
echo "📌 Current version from git tag: $CURRENT_VERSION"

# Update version in pyproject.toml
sed -i.bak "s/version = \".*\"/version = \"$CURRENT_VERSION\"/" pyproject.toml
rm pyproject.toml.bak

# Build the package
echo "🔨 Building package..."
python3 -m build

echo
echo "📦 Built packages:"
ls -la dist/

# Check the package
echo
echo "🔍 Checking package with twine..."
python3 -m twine check dist/*

echo
echo "📤 Ready to upload to PyPI"
echo "This will upload:"
ls dist/

echo
read -p "Upload to PyPI? (y/n) " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    python3 -m twine upload dist/*
    echo "✅ Published to PyPI successfully!"
    echo "Users can now install with: pip install argon-mongodb"
else
    echo "❌ Upload cancelled"
    echo "To upload later: python3 -m twine upload dist/*"
fi