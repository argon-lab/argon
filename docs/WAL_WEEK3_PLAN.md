# WAL Implementation - Week 3 Plan

## Overview
Add time travel capabilities and integrate with Argon CLI for production use.

## Current Status
- ✅ Week 1: WAL foundation complete
- ✅ Week 2: Data operations and materialization complete
- 🚀 Week 3: Time travel and CLI integration (Starting)

## Week 3 Implementation Plan

### Day 1-2: Time Travel Core 🕐

#### Goals
- Query historical state at any LSN
- Query historical state at any timestamp
- Efficient LSN to timestamp mapping

#### Implementation Tasks

**1. Time Travel Service** (`/internal/timetravel/service.go`)
```go
type TimeTravelService struct {
    wal         *wal.Service
    materializer *materializer.Service
}

// Query at specific LSN
func (s *TimeTravelService) MaterializeAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.M, error)

// Query at specific timestamp
func (s *TimeTravelService) MaterializeAtTime(branch *wal.Branch, collection string, timestamp time.Time) (map[string]bson.M, error)

// Find LSN at timestamp
func (s *TimeTravelService) FindLSNAtTime(branch *wal.Branch, timestamp time.Time) (int64, error)
```

**2. Update Materializer**
- Add LSN range support to materialization
- Stop at target LSN instead of branch HEAD
- Handle timestamp to LSN conversion

**3. Testing**
- Query at different LSNs
- Query at different timestamps
- Verify historical accuracy

### Day 3: Restore Operations 🔄

#### Goals
- Reset branch to historical LSN
- Create new branch from historical point
- SDK support for time travel queries

#### Implementation Tasks

**1. Branch Restore** (`/internal/branch/wal/service.go`)
```go
// Reset branch to target LSN
func (s *BranchService) ResetToLSN(branchID string, targetLSN int64) error

// Create branch from historical point
func (s *BranchService) CreateBranchAtLSN(projectID, name, parentID string, targetLSN int64) (*wal.Branch, error)
```

**2. Safety Checks**
- Prevent reset below BaseLSN
- Warn about data loss
- Create restore points

**3. SDK Integration**
```javascript
// Time travel query
const historicalData = await collection
  .atTime(new Date('2024-01-01'))
  .find({ status: 'active' });

// Restore branch
await branch.resetToTime(new Date('2024-01-01'));
```

### Day 4: CLI Integration 🖥️

#### Goals
- Branch management commands
- Time travel commands
- User-friendly interface

#### Implementation Tasks

**1. Branch Commands**
```bash
# List branches
argon branch list --project myapp

# Create branch
argon branch create feature-x --from main

# Delete branch
argon branch delete feature-x

# Show branch info
argon branch info main
```

**2. Time Travel Commands**
```bash
# Query at time
argon time-travel --branch main --time "2024-01-01 12:00" --collection users

# Query at LSN
argon time-travel --branch main --lsn 1000 --collection users

# Show history
argon history --branch main --collection users --limit 10

# Restore branch
argon restore --branch main --to-time "2024-01-01 12:00"
argon restore --branch main --to-lsn 1000
```

**3. Integration with Existing CLI**
- Add commands to existing cobra structure
- Use existing auth and connection logic
- Format output nicely

### Day 5: Production Readiness 🚀

#### Goals
- Performance optimization
- Monitoring and admin tools
- Documentation and examples

#### Implementation Tasks

**1. Materialization Cache**
- Cache frequently accessed states
- Background cache warming
- Cache invalidation on writes

**2. Performance Monitoring**
```bash
# WAL statistics
argon wal stats

# Performance metrics
argon wal perf --duration 1h

# Storage usage
argon wal storage
```

**3. Admin Tools**
```bash
# Compact WAL (future)
argon wal compact --before "30 days ago"

# Export WAL
argon wal export --branch main --format json

# Verify WAL integrity
argon wal verify
```

**4. Documentation**
- Time travel usage guide
- CLI command reference
- Performance tuning guide
- Migration guide

## Success Criteria

### Functionality
- [ ] Query any historical state
- [ ] Restore branches to past states
- [ ] CLI commands work smoothly
- [ ] No data corruption

### Performance
- [ ] Historical queries < 500ms
- [ ] Restore operations < 5s
- [ ] Cache hit rate > 80%

### Usability
- [ ] Intuitive CLI interface
- [ ] Clear error messages
- [ ] Helpful documentation

## Testing Strategy

### Unit Tests
- Time travel calculations
- LSN to timestamp mapping
- Cache behavior

### Integration Tests
- End-to-end time travel
- Branch restore scenarios
- CLI command execution

### Performance Tests
- Historical query speed
- Cache effectiveness
- Concurrent time travel

## Risk Mitigation

### Data Safety
- Confirm destructive operations
- Create automatic backups before restore
- Validate LSN ranges

### Performance
- Implement caching early
- Index timestamp fields
- Limit time travel range

### User Experience
- Provide progress indicators
- Show preview before restore
- Offer undo capability

## MVP Simplifications

1. **No Continuous Time Travel**
   - Discrete LSN/timestamp points only
   - No sliding window queries

2. **Limited Cache**
   - Simple LRU cache
   - No distributed caching

3. **Basic CLI**
   - Essential commands only
   - Text output (no fancy UI)

## Next Steps After Week 3

1. **Garbage Collection**
   - Clean up old WAL entries
   - Configurable retention policies

2. **Advanced Features**
   - Continuous time travel
   - Branching from queries
   - Conflict resolution

3. **Production Hardening**
   - Distributed caching
   - WAL archival
   - Monitoring dashboard