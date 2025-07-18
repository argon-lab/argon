#!/bin/bash

# Argon Engine Integration Test Runner
echo "🚀 Argon Engine Integration Test Runner"
echo "========================================"

# Check if MongoDB is running
echo "📡 Checking MongoDB connection..."
if ! mongosh --eval "db.runCommand({ping: 1})" > /dev/null 2>&1; then
    echo "❌ MongoDB is not running or not accessible"
    echo "   Please start MongoDB with: brew services start mongodb-community"
    echo "   Or use Docker: docker run -d -p 27017:27017 mongo"
    exit 1
fi

echo "✅ MongoDB is accessible"

# Set environment variables for testing
export MONGO_URI="mongodb://localhost:27017/argon_test"
export STORAGE_PROVIDER="local"
export COMPRESSION_LEVEL="6"
export PORT="8080"
export ENVIRONMENT="development"

echo "⚙️  Environment configured for testing"

# Build the integration test
echo "🔨 Building integration test..."
go build -o integration_test integration_test.go

if [ $? -ne 0 ]; then
    echo "❌ Failed to build integration test"
    exit 1
fi

echo "✅ Integration test built successfully"

# Run the integration test
echo "🧪 Running integration test..."
echo ""

./integration_test

test_result=$?

# Clean up
echo ""
echo "🧹 Cleaning up..."
rm -f integration_test

if [ $test_result -eq 0 ]; then
    echo "✅ Integration test passed!"
    echo ""
    echo "🎉 Argon Engine is ready for production!"
    echo "   All core components are working correctly:"
    echo "   - MongoDB branching"
    echo "   - Storage with compression"
    echo "   - Async worker system"
    echo "   - Change stream processing"
else
    echo "❌ Integration test failed"
    echo "   Check the logs above for details"
    exit 1
fi