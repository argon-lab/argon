# WAL Implementation Master Plan for Open-Source Argon

## Executive Summary

Transform Argon from collection-prefix based branching to WAL-based architecture in 3 weeks, achieving 100x faster branch operations and 80%+ storage reduction.

## Vision

Create a Git-like database branching system where:
- Branches are instant (< 10ms)
- Storage is efficient (no data duplication)
- Time travel is possible (restore to any point)
- Compatible with existing MongoDB workflows

## Architecture Overview

```
Current (Collection Prefix):
  main_users     → Full copy of data
  feature_users  → Another full copy
  
New (WAL-Based):
  WAL Log → All changes in sequence
  Branches → Just pointers to LSN positions
```

## 3-Week Implementation Plan

### Week 1: WAL Foundation (Days 1-5)
**Goal**: Build core WAL system and branch operations

- **Day 1-2**: WAL Core
  - WAL entry structure (LSN, operation, data)
  - Append operations to WAL
  - MongoDB collections: `wal_log`, `wal_branches`

- **Day 3-4**: Branch Management
  - Create branch = create pointer
  - Delete branch = delete pointer (keep WAL)
  - Branch metadata tracking

- **Day 5**: Project Operations
  - Create/delete projects
  - Main branch initialization
  - Basic CLI integration

**Deliverables**:
- ✅ WAL service running
- ✅ Branch CRUD operations
- ✅ Project management
- ✅ LSN tracking

### Week 2: Data Operations (Days 6-10)
**Goal**: Intercept MongoDB operations and materialize data

- **Day 6-7**: Write Operations
  - Intercept insert/update/delete
  - Append to WAL instead of MongoDB
  - Update branch HEAD pointer

- **Day 8-9**: Read Operations  
  - Basic materializer (replay WAL)
  - Simple query execution
  - Filter matching

- **Day 10**: Integration
  - MongoDB driver wrapper
  - Database/Collection interfaces
  - Error handling

**Deliverables**:
- ✅ Data writes go to WAL
- ✅ Queries work on WAL data
- ✅ MongoDB-compatible API
- ✅ Basic performance

### Week 3: Time Travel & Polish (Days 11-15)
**Goal**: Add time travel and production readiness

- **Day 11-12**: Time Travel
  - Restore to timestamp
  - Restore to LSN
  - Create branches from history

- **Day 13-14**: CLI Integration
  - Feature flags for gradual rollout
  - WAL-specific commands
  - Migration tools

- **Day 15**: Testing & Documentation
  - Integration tests
  - Performance benchmarks
  - Usage documentation

**Deliverables**:
- ✅ Time travel working
- ✅ CLI fully integrated
- ✅ Tests passing
- ✅ Ready to ship

## Technical Architecture

### 1. Core Components

```go
// WAL Entry
type WALEntry struct {
    LSN        int64     // Log Sequence Number
    Timestamp  time.Time
    ProjectID  string
    BranchID   string
    Operation  string    // insert/update/delete
    Collection string
    Document   bson.Raw
}

// Branch (Just a pointer!)
type WALBranch struct {
    ID        string
    ProjectID string
    Name      string
    HeadLSN   int64  // Current position
    BaseLSN   int64  // Fork point
}
```

### 2. Key Operations

**Create Branch** (Instant)
```go
newBranch := WALBranch{
    Name:    "feature-x",
    HeadLSN: parentBranch.HeadLSN,
    BaseLSN: parentBranch.HeadLSN,
}
// Just save metadata - no data copy!
```

**Write Data** (Append-only)
```go
entry := WALEntry{
    Operation: "insert",
    Document:  bson.Marshal(doc),
}
lsn := wal.Append(entry)
branch.HeadLSN = lsn
```

**Read Data** (Materialize)
```go
// Replay WAL to build current state
entries := wal.GetEntries(0, branch.HeadLSN)
state := materialize(entries)
results := filter(state, query)
```

### 3. Storage Model

```
MongoDB Collections:
├── wal_log          # All operations (append-only)
├── wal_branches     # Branch metadata
└── wal_projects     # Project metadata

No collection copying!
No data duplication!
```

## Key Simplifications (MVP Focus)

1. **No Garbage Collection**: Just delete branch pointers
2. **No Background Services**: Everything synchronous
3. **No Compression**: Raw BSON storage
4. **No Snapshots**: Always replay from start
5. **Simple Queries**: Basic filter matching only

## Success Metrics

| Metric | Current | Target | Why It Matters |
|--------|---------|--------|----------------|
| Branch Creation | 500ms+ | < 10ms | 50x faster |
| Storage (10 branches) | 10GB | 1.3GB | 87% reduction |
| Query Overhead | 0ms | < 100ms | Acceptable trade-off |
| Time Travel | ❌ | ✅ | New capability |

## Implementation Strategy

### 1. Git Branch
```bash
git checkout -b feature/wal-core
```

### 2. Feature Flags
```go
if os.Getenv("ENABLE_WAL") == "true" {
    // New WAL path
} else {
    // Existing path
}
```

### 3. Parallel Development
- Keep existing system working
- Build WAL alongside
- No breaking changes

## Risk Mitigation

| Risk | Impact | Mitigation |
|------|--------|------------|
| Performance issues | High | Start simple, optimize later |
| Complex bugs | High | Extensive testing, feature flags |
| Migration complexity | Medium | Build migration tools in Week 3 |

## Future Enhancements (Post-MVP)

**Month 2**:
- Query optimization
- Caching layer
- Performance tuning

**Month 3**:
- Snapshots for faster queries
- WAL cleanup tools
- Advanced queries

**Month 4+**:
- Distributed WAL
- Compression
- Production optimizations

## The Bottom Line

In 3 weeks, we'll have:
- ✅ Working WAL-based branching
- ✅ 50x faster branch operations
- ✅ 87% storage reduction
- ✅ Time travel capability
- ✅ No breaking changes

**Philosophy**: Ship simple, iterate based on usage, optimize what matters.

## Next Steps

1. Create `feature/wal-core` branch
2. Set up basic project structure
3. Start Week 1 implementation
4. Daily progress updates

Ready to begin? 🚀