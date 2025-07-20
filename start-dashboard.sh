#!/bin/bash

# Argon Dashboard Development Startup Script
# This script starts both the API server and dashboard for local development

set -e

echo "🚀 Starting Argon Dashboard Development Environment"
echo ""

# Check if MongoDB is running
if ! pgrep -x "mongod" > /dev/null; then
    echo "⚠️  MongoDB is not running. Please start MongoDB first:"
    echo "   brew services start mongodb/brew/mongodb-community"
    echo "   or"
    echo "   sudo systemctl start mongod"
    echo ""
    exit 1
fi

# Check if Node.js is installed
if ! command -v node &> /dev/null; then
    echo "❌ Node.js is not installed. Please install Node.js 16+ first."
    exit 1
fi

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "❌ Go is not installed. Please install Go 1.21+ first."
    exit 1
fi

echo "✅ Prerequisites check passed"
echo ""

# Function to cleanup background processes
cleanup() {
    echo ""
    echo "🛑 Shutting down services..."
    kill $API_PID $DASHBOARD_PID 2>/dev/null || true
    wait $API_PID $DASHBOARD_PID 2>/dev/null || true
    echo "✅ Services stopped"
    exit 0
}

# Set trap to cleanup on script exit
trap cleanup SIGINT SIGTERM EXIT

# Start API server in background
echo "🔧 Starting API server..."
cd api
go run main.go &
API_PID=$!
cd ..

# Wait for API to start
echo "⏳ Waiting for API server to start..."
sleep 3

# Check if API is running
if ! kill -0 $API_PID 2>/dev/null; then
    echo "❌ Failed to start API server"
    exit 1
fi

echo "✅ API server started (PID: $API_PID)"
echo ""

# Install dashboard dependencies if needed
echo "📦 Installing dashboard dependencies..."
cd dashboard
if [ ! -d "node_modules" ]; then
    npm install
fi

# Start dashboard in background
echo "🎨 Starting dashboard..."
npm start &
DASHBOARD_PID=$!
cd ..

# Wait for dashboard to start
echo "⏳ Waiting for dashboard to start..."
sleep 5

# Check if dashboard is running
if ! kill -0 $DASHBOARD_PID 2>/dev/null; then
    echo "❌ Failed to start dashboard"
    exit 1
fi

echo "✅ Dashboard started (PID: $DASHBOARD_PID)"
echo ""
echo "🎉 Argon Dashboard is ready!"
echo ""
echo "📍 Access points:"
echo "   Dashboard: http://localhost:3000"
echo "   API:       http://localhost:8080"
echo ""
echo "🛠️  Development tips:"
echo "   • The dashboard auto-reloads on file changes"
echo "   • API server restarts manually (Ctrl+C and re-run script)"
echo "   • Check MongoDB connection: http://localhost:8080/api/health"
echo ""
echo "Press Ctrl+C to stop all services"
echo ""

# Wait for user to stop
wait $API_PID $DASHBOARD_PID