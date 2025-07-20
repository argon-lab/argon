#!/bin/bash

# MongoDB Branching Test Runner
echo "🧪 MongoDB Branching Functionality Test Runner"
echo "=============================================="

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
export MONGO_URI="mongodb://localhost:27017/argon_branching_test"
export STORAGE_PROVIDER="local"
export COMPRESSION_LEVEL="6"
export PORT="8080"
export ENVIRONMENT="development"

echo "⚙️  Environment configured for MongoDB branching test"

# Build the branching test
echo "🔨 Building MongoDB branching test..."
go build -o mongodb_branching_test mongodb_branching_test.go

if [ $? -ne 0 ]; then
    echo "❌ Failed to build MongoDB branching test"
    exit 1
fi

echo "✅ MongoDB branching test built successfully"

# Run the branching test
echo "🧪 Running MongoDB branching test..."
echo ""

./mongodb_branching_test

test_result=$?

# Clean up
echo ""
echo "🧹 Cleaning up..."
rm -f mongodb_branching_test

if [ $test_result -eq 0 ]; then
    echo "✅ MongoDB branching test passed!"
    echo ""
    echo "🎉 Core MongoDB branching functionality is working!"
    echo "   Key features validated:"
    echo "   ✅ Branch creation with data copying"
    echo "   ✅ Data isolation between branches"
    echo "   ✅ Branch switching and validation"
    echo "   ✅ Collection management and cleanup"
    echo "   ✅ Performance with bulk operations"
    echo ""
    echo "🚀 Ready for investor demo!"
else
    echo "❌ MongoDB branching test failed"
    echo "   Check the logs above for details"
    exit 1
fi