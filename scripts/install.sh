#!/bin/bash

# Argon CLI Installation Script
# Usage: curl -sSL https://raw.githubusercontent.com/argon-lab/argon/main/scripts/install.sh | bash

set -e

REPO="argon-lab/argon"
INSTALL_DIR="/usr/local/bin"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_status() { echo -e "${BLUE}[INFO]${NC} $1"; }
print_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[WARNING]${NC} $1"; }
print_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Detect platform
detect_platform() {
    local platform=$(uname -s | tr '[:upper:]' '[:lower:]')
    local arch=$(uname -m)
    
    case $platform in
        linux*)
            platform="linux"
            ;;
        darwin*)
            platform="darwin"
            ;;
        *)
            print_error "Unsupported platform: $platform"
            exit 1
            ;;
    esac
    
    case $arch in
        x86_64|amd64)
            arch="amd64"
            ;;
        arm64|aarch64)
            arch="arm64"
            ;;
        *)
            print_error "Unsupported architecture: $arch"
            exit 1
            ;;
    esac
    
    echo "${platform}-${arch}"
}

# Get latest release version
get_latest_version() {
    curl -s "https://api.github.com/repos/$REPO/releases/latest" | \
        grep '"tag_name":' | \
        cut -d'"' -f4 | \
        sed 's/^v//'
}

# Download and install
install_argon() {
    print_status "Installing Argon CLI..."
    
    local platform=$(detect_platform)
    local version=$(get_latest_version)
    
    if [ -z "$version" ]; then
        print_error "Could not determine latest version"
        exit 1
    fi
    
    print_status "Latest version: $version"
    print_status "Platform: $platform"
    
    local binary_name="argon-$platform"
    local download_url="https://github.com/$REPO/releases/download/v$version/$binary_name"
    local temp_file="/tmp/argon"
    
    print_status "Downloading from: $download_url"
    
    if ! curl -L -o "$temp_file" "$download_url"; then
        print_error "Failed to download Argon CLI"
        exit 1
    fi
    
    # Make executable
    chmod +x "$temp_file"
    
    # Check if we can write to install directory
    if [ -w "$INSTALL_DIR" ]; then
        mv "$temp_file" "$INSTALL_DIR/argon"
    else
        print_status "Installing to $INSTALL_DIR (requires sudo)..."
        sudo mv "$temp_file" "$INSTALL_DIR/argon"
    fi
    
    print_success "Argon CLI installed successfully!"
    
    # Verify installation
    if command -v argon >/dev/null 2>&1; then
        local installed_version=$(argon --version | cut -d' ' -f3)
        print_success "Verified: argon version $installed_version"
    else
        print_warning "Installation complete, but 'argon' not found in PATH"
        print_warning "You may need to restart your shell or add $INSTALL_DIR to your PATH"
    fi
    
    echo
    print_status "Get started with:"
    echo "  argon --help"
    echo "  argon projects create --name my-project"
}

# Check dependencies
check_dependencies() {
    if ! command -v curl >/dev/null 2>&1; then
        print_error "curl is required but not installed"
        exit 1
    fi
}

# Main
main() {
    echo "🚀 Argon CLI Installer"
    echo "======================"
    echo
    
    check_dependencies
    install_argon
    
    echo
    print_success "🎉 Installation complete!"
    print_status "Visit https://github.com/argon-lab/argon for documentation"
}

main "$@"