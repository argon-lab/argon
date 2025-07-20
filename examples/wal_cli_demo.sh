#!/bin/bash

# WAL CLI Demo Script
# This script demonstrates the time travel and restore features

echo "=== Argon WAL CLI Demo ==="
echo "Make sure ENABLE_WAL=true is set in your environment"
echo ""

# Set WAL environment
export ENABLE_WAL=true

# Create a project
echo "1. Creating a project with time travel..."
argon projects create demo-project

# Get project ID (in real usage, you'd capture this from the create command)
PROJECT_ID="demo-project"

# List projects
echo -e "\n2. Listing projects..."
argon projects list

# Create some data
echo -e "\n3. Creating initial data..."
# Note: In real usage, you'd use MongoDB driver to insert data
# The WAL system transparently captures all operations
echo "   Simulating data insertion via MongoDB driver..."
echo "   - User: Alice (admin)"
echo "   - User: Bob (user)"  
echo "   - Product: Laptop ($1000)"

# Query current state
echo -e "\n4. Current state of users collection..."
argon wal query find users -p $PROJECT_ID -b main

# Get time travel info
echo -e "\n5. Time travel information..."
argon wal time-travel info -p $PROJECT_ID -b main

# Save current LSN for later
echo -e "\n6. Making some changes..."
argon wal query insert users -p $PROJECT_ID -b main --doc "name:Charlie,role:user,active:false"

# Update a document (would need update command - showing concept)
echo "   (Simulating updates and deletes...)"

# Query at earlier point
echo -e "\n7. Time travel query - viewing data from 1 minute ago..."
argon wal time-travel query users -p $PROJECT_ID -b main --time "1m ago"

# Create a backup branch
echo -e "\n8. Creating a backup of current state..."
argon wal restore backup backup-$(date +%Y%m%d) -p $PROJECT_ID --from main

# Create a feature branch from historical point
echo -e "\n9. Creating feature branch from earlier state..."
argon wal restore create feature-branch -p $PROJECT_ID --from main --time "30s ago"

# List branches
echo -e "\n10. Current branches..."
argon wal branch list -p $PROJECT_ID

# Work on feature branch
echo -e "\n11. Working on feature branch..."
argon wal query insert users -p $PROJECT_ID -b feature-branch --doc "name:Dave,role:tester"
argon wal query find users -p $PROJECT_ID -b feature-branch --limit 5

# Show main branch still has original state
echo -e "\n12. Main branch is unaffected..."
argon wal query count users -p $PROJECT_ID -b main

# Demonstrate restore preview
echo -e "\n13. Preview restore operation..."
argon wal restore reset -p $PROJECT_ID -b main --time "2m ago"

# Find modified collections
echo -e "\n14. Collections modified in the last 100 operations..."
argon wal time-travel modified -p $PROJECT_ID -b main --from-lsn 0

echo -e "\n=== Demo Complete ==="
echo "This demo showed:"
echo "  - Creating WAL projects and branches"
echo "  - Querying data with time travel"
echo "  - Creating backups and feature branches from history"
echo "  - Restoring branches to previous states"
echo ""
echo "Try these commands with your own data!"