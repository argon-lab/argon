#!/bin/bash

# Build release binaries for all platforms
# Usage: ./scripts/build_release.sh [version]

set -e

VERSION=${1:-"1.0.0"}
DIST_DIR="dist"

echo "🔨 Building Argon CLI v$VERSION for all platforms"
echo "================================================"

# Clean and create dist directory
rm -rf $DIST_DIR
mkdir -p $DIST_DIR

# Build targets
TARGETS=(
    "linux/amd64"
    "linux/arm64"
    "darwin/amd64" 
    "darwin/arm64"
    "windows/amd64"
)

cd cli

for target in "${TARGETS[@]}"; do
    GOOS=${target%/*}
    GOARCH=${target#*/}
    
    echo "Building for $GOOS/$GOARCH..."
    
    if [ "$GOOS" = "windows" ]; then
        BINARY_NAME="argon-$GOOS-$GOARCH.exe"
    else
        BINARY_NAME="argon-$GOOS-$GOARCH"
    fi
    
    env GOOS=$GOOS GOARCH=$GOARCH go build \
        -ldflags="-s -w -X main.version=$VERSION" \
        -o "../$DIST_DIR/$BINARY_NAME" \
        .
    
    echo "✅ Built $BINARY_NAME"
done

cd ..

echo
echo "🎉 Build complete! Binaries available in $DIST_DIR/"
ls -la $DIST_DIR/

echo
echo "📦 To create a GitHub release:"
echo "  git tag v$VERSION"
echo "  git push origin v$VERSION"
echo
echo "📝 To test a binary:"
echo "  ./$DIST_DIR/argon-linux-amd64 --version"