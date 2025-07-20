#!/bin/bash

# Argon Practical Demos Runner
# Run real-world scenarios showing MongoDB branching with time travel

echo "🚀 Argon Practical Demos"
echo "Real-world scenarios for developers and ML engineers"
echo "====================================================="
echo

# Check if argon is available
if ! command -v argon &> /dev/null; then
    echo "❌ Argon CLI not found. Please install or build argon first:"
    echo "   cd cli && go build -o argon"
    echo "   export PATH=\$PATH:\$(pwd)"
    exit 1
fi

# Check if MongoDB is running
if ! mongosh --eval "db.adminCommand('ping')" &> /dev/null; then
    echo "❌ MongoDB not running. Please start MongoDB:"
    echo "   mongod --dbpath /path/to/data"
    echo "   Or use Docker: docker run -d -p 27017:27017 mongo"
    exit 1
fi

# Check if Python is available
if ! command -v python3 &> /dev/null; then
    echo "❌ Python 3 not found. Demos require Python 3 with pymongo:"
    echo "   pip install pymongo"
    exit 1
fi

echo "✅ Prerequisites check complete"
echo

# Menu
echo "Select demo to run:"
echo "1) Migration Disaster Recovery (Developer)"
echo "2) ML Pipeline Failure Recovery (ML Engineer)"  
echo "3) Experiment Reproducibility (ML Engineer)"
echo "4) Run all demos"
echo "5) Exit"
echo

read -p "Choose option (1-5): " choice

case $choice in
    1)
        echo
        echo "🚨 Running Migration Disaster Recovery Demo..."
        echo "Shows how Argon prevents database migration disasters"
        echo
        ./developer-demo/migration-disaster.sh
        ;;
    2)
        echo
        echo "🤖 Running ML Pipeline Failure Recovery Demo..."
        echo "Shows instant recovery from ML data pipeline corruption"
        echo
        ./ml-demo/pipeline-recovery.sh
        ;;
    3)
        echo
        echo "🔬 Running Experiment Reproducibility Demo..."
        echo "Shows how to achieve 100% reproducible ML experiments"
        echo
        ./ml-demo/experiment-reproducibility.sh
        ;;
    4)
        echo
        echo "🎬 Running all demos..."
        echo
        
        echo "================================="
        echo "Demo 1: Migration Disaster Recovery"
        echo "================================="
        ./developer-demo/migration-disaster.sh
        echo
        echo "Press Enter to continue to next demo..."
        read
        
        echo "================================="
        echo "Demo 2: ML Pipeline Failure Recovery"
        echo "================================="
        ./ml-demo/pipeline-recovery.sh
        echo
        echo "Press Enter to continue to next demo..."
        read
        
        echo "================================="
        echo "Demo 3: Experiment Reproducibility"
        echo "================================="
        ./ml-demo/experiment-reproducibility.sh
        echo
        echo "🎉 All demos complete!"
        ;;
    5)
        echo "Goodbye!"
        exit 0
        ;;
    *)
        echo "Invalid option. Please choose 1-5."
        exit 1
        ;;
esac

echo
echo "🎯 Demo complete! Key takeaways:"
echo "   • Database disasters become 30-second recoveries"
echo "   • ML pipeline failures no longer block teams"
echo "   • Research reproducibility crisis solved"
echo "   • Time travel enables safe experimentation"
echo
echo "🚀 Ready to try Argon with your own data?"
echo "   Get started: https://github.com/argon-lab/argon"