#!/bin/bash

# Clean Commit Script for Argon v2
# Ensures no AI assistant content is included in commits

set -e

echo "🧹 Argon v2 Clean Commit Script"
echo "==============================="

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

print_status() { echo -e "${BLUE}[INFO]${NC} $1"; }
print_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[WARNING]${NC} $1"; }
print_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Check for Claude-related content
print_status "Checking for AI assistant content..."

# Check if any Claude files would be committed
claude_files=$(git status --porcelain | grep -E "(claude|CLAUDE)" | wc -l | tr -d ' ')
if [ "$claude_files" -gt 0 ]; then
    print_error "Found Claude-related files in git status!"
    git status --porcelain | grep -E "(claude|CLAUDE)"
    print_error "Please remove these files or ensure .gitignore is working correctly"
    exit 1
fi

# Check for AI references in commit message
if [ $# -eq 0 ]; then
    print_error "Please provide a commit message"
    echo "Usage: $0 \"Your commit message\""
    exit 1
fi

COMMIT_MSG="$1"

# Check for AI assistant references in commit message
if echo "$COMMIT_MSG" | grep -qi -E "(claude|ai.assistant|gpt|chatgpt)"; then
    print_error "Commit message contains AI assistant references"
    print_error "Please use a clean commit message without AI references"
    exit 1
fi

# Check for AI signatures in staged files
print_status "Checking staged files for AI signatures..."
ai_signatures=0

# Check staged files for AI-generated comments
for file in $(git diff --cached --name-only); do
    if [ -f "$file" ]; then
        if grep -qi -E "(generated.by|claude|ai.assistant|gpt.*generated)" "$file"; then
            print_warning "Found potential AI signature in: $file"
            ai_signatures=$((ai_signatures + 1))
        fi
    fi
done

if [ "$ai_signatures" -gt 0 ]; then
    print_error "Found $ai_signatures file(s) with potential AI signatures"
    print_error "Please remove AI-generated comments before committing"
    exit 1
fi

# Check that we're not committing the argon binary
if git diff --cached --name-only | grep -q "cli/argon$"; then
    print_error "Attempting to commit CLI binary 'cli/argon'"
    print_error "Please remove binary files from commit"
    exit 1
fi

# Show what will be committed
print_status "Files to be committed:"
git diff --cached --name-only | sed 's/^/  ✓ /'

# Confirm commit
print_status "Commit message: \"$COMMIT_MSG\""
echo
read -p "Proceed with clean commit? (y/N) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    print_warning "Commit cancelled"
    exit 0
fi

# Create the commit
print_status "Creating clean commit..."
git commit -m "$COMMIT_MSG"

print_success "Clean commit created successfully!"
print_status "Summary:"
echo "  • No AI assistant content included"
echo "  • No binary files committed"
echo "  • Clean commit message"
echo "  • Ready for GitHub push"

echo
print_success "🎉 Commit is ready for GitHub!"
print_status "Run 'git push origin v2-rewrite' to push to GitHub"