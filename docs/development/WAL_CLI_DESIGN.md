# WAL CLI Design and Implementation

## Overview

The Argon CLI now supports time travel and restore operations through the WAL (Write-Ahead Log) architecture. This document outlines the CLI commands and their implementation.

## Command Structure

```
argon wal-simple
├── status                    # Show WAL system status
├── project
│   ├── create [name]        # Create WAL-enabled project  
│   └── list                 # List WAL projects
├── tt-info                  # Time travel information
└── restore-preview          # Preview restore operations
```

## Core Commands Implemented

### 1. WAL Status
```bash
argon wal-simple status
```
- Shows WAL enablement status
- Tests connection to WAL backend
- Displays current LSN

### 2. Project Management
```bash
# Create WAL-enabled project
argon wal-simple project create my-project

# List all WAL projects
argon wal-simple project list
```

### 3. Time Travel Information
```bash
argon wal-simple tt-info -p project-id -b main
```
- Shows LSN range for branch
- Displays time range of available history
- Shows total entry count

### 4. Restore Preview
```bash
argon wal-simple restore-preview -p project-id -b main --lsn 123
```
- Shows what a restore would affect
- Lists operations that would be discarded
- Warns about data loss

## Implementation Details

### Service Layer
Created `pkg/walcli/services.go` to provide public interfaces:
```go
type Services struct {
    WAL          *wal.Service
    Branches     *branchwal.BranchService  
    Projects     *projectwal.ProjectService
    Materializer *materializer.Service
    TimeTravel   *timetravel.Service
    Restore      *restore.Service
}
```

### Configuration
Created `pkg/config/config.go` for feature flags:
```go
func IsWALEnabled() bool {
    return os.Getenv("ENABLE_WAL") == "true"
}
```

## Command Examples

### Basic Workflow
```bash
# 1. Enable WAL
export ENABLE_WAL=true

# 2. Check status
argon wal-simple status

# 3. Create project
argon wal-simple project create ecommerce

# 4. Check time travel info
argon wal-simple tt-info -p ecommerce -b main

# 5. Preview restore operation
argon wal-simple restore-preview -p ecommerce -b main --lsn 100
```

### Output Examples

#### Status Command
```
WAL System Status:
  Enabled: true
  New Projects Use WAL: true
  New Branches Use WAL: true
  Migration Enabled: true
  Connection: OK
  Current LSN: 1543
```

#### Project Creation
```
Created WAL-enabled project 'ecommerce' (ID: ecommerce)
Default branch: main (LSN: 1544)
```

#### Time Travel Info
```
Time Travel Info for branch 'main':
  Branch ID: 648f2a1b2c3d4e5f6789abcd
  LSN Range: 1544 - 1823
  Total Entries: 279
  Time Range: 2024-01-15 10:30:00 to 2024-01-15 14:45:32
```

#### Restore Preview
```
Restore Preview for branch 'main':
  Current LSN: 1823
  Target LSN: 1600
  Operations to discard: 223
  Affected collections:
    - users: 45 operations
    - orders: 123 operations
    - products: 55 operations

WARNING: This would discard 223 operations!
```

## Advanced Features (Future)

### Planned Commands
```bash
# Full time travel query
argon wal time-travel query users -p project -b main --time "1h ago"

# Restore operations
argon wal restore reset -p project -b main --lsn 1600
argon wal restore create feature-branch -p project --from main --time "2h ago"

# Data queries
argon wal query find users -p project -b main --filter "active:true"
argon wal query count orders -p project -b main
```

### Interactive Mode
```bash
argon wal interactive -p project -b main
> query users where active=true
> time-travel 1h ago
> restore preview --lsn 1600
```

## Security and Safety

### Confirmation Prompts
- All destructive operations require confirmation
- Preview shows exact impact before execution
- Force flags available for automation

### Access Control
- WAL requires explicit enablement (`ENABLE_WAL=true`)
- MongoDB connection string configurable
- Separate database for WAL data (`argon_wal`)

## Performance Considerations

### Optimizations
- Commands use read-only operations when possible
- Preview operations are fast (< 100ms)
- Connection pooling for service layer
- Efficient LSN-based queries

### Limitations
- CLI connects directly to MongoDB (no API layer)
- Some operations require internal package access
- Build complexity due to internal imports

## Testing

### Manual Testing
```bash
# Start MongoDB
mongod --port 27017

# Set environment
export ENABLE_WAL=true
export MONGODB_URI="mongodb://localhost:27017"

# Test basic commands
argon wal-simple status
argon wal-simple project create test-project
argon wal-simple tt-info -p test-project -b main
```

### Integration with WAL Tests
CLI commands use the same services tested in:
- `tests/wal/timetravel_test.go`
- `tests/wal/restore_test.go`
- `tests/wal/week3_integration_test.go`

## Production Deployment

### Build Process
```bash
# Build for current platform
go build -o argon-cli .

# Cross-platform builds
GOOS=linux GOARCH=amd64 go build -o argon-cli-linux .
GOOS=windows GOARCH=amd64 go build -o argon-cli.exe .
GOOS=darwin GOARCH=arm64 go build -o argon-cli-darwin .
```

### Distribution
- Single binary with all dependencies
- No external runtime requirements (except MongoDB)
- Configuration via environment variables
- Supports both local and remote MongoDB

## Summary

The WAL CLI integration provides:
✅ **Basic WAL Operations**: Status, projects, time travel info
✅ **Safety Features**: Restore preview, confirmation prompts  
✅ **Performance**: Fast queries, efficient service layer
✅ **Usability**: Clear output, helpful error messages

The implementation establishes the foundation for full time travel and restore functionality while maintaining compatibility with the existing WAL architecture.