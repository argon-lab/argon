# Open-Source Argon: Detailed Operation Flows

## Architecture Overview

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Argon CLI     │────▶│  Argon Engine   │────▶│    MongoDB      │
└─────────────────┘     └─────────────────┘     └─────────────────┘
                                │                         │
                                ▼                         ▼
                        ┌─────────────┐           ┌─────────────┐
                        │ WAL Service │           │   Storage   │
                        └─────────────┘           └─────────────┘
                                │
                                ▼
                        ┌─────────────┐
                        │   Branch    │
                        │  Executor   │
                        └─────────────┘
```

## Flow 1: User Connects to Branch

### Current System:
```go
// User runs: argon connect feature-123

1. CLI sends request to engine
2. Engine creates BranchDatabase with prefix "feat123"
3. Returns connection string pointing to MongoDB
4. User queries go directly to prefixed collections
```

### New System:
```go
// User runs: argon connect feature-123

1. CLI sends request to engine
2. Engine creates BranchContext:
   {
     BranchID: "feature-123",
     HeadLSN: 12345,
     WAL: walService,
     Executor: branchExecutor
   }
3. Returns wrapped connection that intercepts all operations
4. User queries go through WAL system
```

## Flow 2: Insert Operation

### Current System:
```go
// User code
db.Collection("users").InsertOne(ctx, user)

// Flow:
MongoDB Driver
    ↓
BranchDatabase.Collection("users")
    ↓ (returns "feat123_users")
mongo.Collection.InsertOne()
    ↓
MongoDB writes to "feat123_users"
```

### New System:
```go
// User code (unchanged!)
db.Collection("users").InsertOne(ctx, user)

// Flow:
ArgonDriver.Collection("users")
    ↓
WALInterceptor.InsertOne()
    ↓
    ├── 1. Generate document ID
    ├── 2. Create WAL entry
    ├── 3. Append to WAL (gets LSN)
    ├── 4. Update branch HEAD
    └── 5. Return success (no MongoDB write!)

// WAL Entry:
{
  LSN: 12346,
  Timestamp: "2024-01-15T10:00:00Z",
  BranchID: "feature-123",
  Operation: "insert",
  Collection: "users",
  DocumentID: "507f1f77bcf86cd799439011",
  Changes: {
    After: { _id: "507f...", name: "Alice", email: "alice@example.com" }
  }
}
```

## Flow 3: Query Operation

### Current System:
```go
// User code
cursor, _ := db.Collection("users").Find(ctx, bson.M{"active": true})

// Flow:
MongoDB Driver
    ↓
BranchDatabase.Collection("users")
    ↓ (returns "feat123_users")
mongo.Collection.Find()
    ↓
MongoDB queries "feat123_users" directly
```

### New System:
```go
// User code (unchanged!)
cursor, _ := db.Collection("users").Find(ctx, bson.M{"active": true})

// Flow:
ArgonDriver.Collection("users")
    ↓
BranchExecutor.Find()
    ↓
    ├── 1. Get branch HEAD LSN (12346)
    ├── 2. Check cache for materialized state
    ├── 3. If miss, materialize:
    │   ├── a. Find snapshot (LSN 12000)
    │   ├── b. Load WAL entries 12001-12346
    │   ├── c. Apply entries to build state
    │   └── d. Cache result
    ├── 4. Apply filter {"active": true}
    └── 5. Return cursor over results

// Materialization Process:
Snapshot@12000: {users: [{_id: "1", name: "Bob", active: false}]}
  + WAL@12001: INSERT {_id: "2", name: "Alice", active: true}
  + WAL@12045: UPDATE {_id: "1", active: true}
  + WAL@12346: INSERT {_id: "3", name: "Charlie", active: true}
  = State@12346: [
      {_id: "1", name: "Bob", active: true},
      {_id: "2", name: "Alice", active: true},
      {_id: "3", name: "Charlie", active: true}
    ]

// After filter:
Result: [Alice, Bob, Charlie] (all active users)
```

## Flow 4: Branch Creation

### Current System:
```go
// User runs: argon branch create feature-456

// Flow:
CLI
 ↓
Engine.CreateBranch("feature-456", "main")
 ↓
 ├── 1. Create branch metadata
 ├── 2. List all collections in main
 ├── 3. For each collection:
 │   ├── a. Create new collection with prefix
 │   ├── b. Copy all documents
 │   └── c. Copy all indexes
 └── 4. Return success

// Time: 30+ seconds for large databases
```

### New System:
```go
// User runs: argon branch create feature-456

// Flow:
CLI
 ↓
Engine.CreateBranch("feature-456", "main")
 ↓
 ├── 1. Get main branch HEAD (LSN 12346)
 ├── 2. Create branch record:
 │   {
 │     ID: "feature-456",
 │     Name: "feature-456",
 │     HeadLSN: 12346,
 │     BaseLSN: 12346,
 │     ParentID: "main"
 │   }
 └── 3. Return success

// Time: <10ms (just metadata!)
```

## Flow 5: Update Operation

### Current System:
```go
// User code
db.Collection("users").UpdateOne(ctx, 
  bson.M{"_id": "123"}, 
  bson.M{"$set": bson.M{"status": "premium"}}
)

// Flow:
Direct update to "feat123_users" collection
No history preserved
```

### New System:
```go
// User code (unchanged!)
db.Collection("users").UpdateOne(ctx, 
  bson.M{"_id": "123"}, 
  bson.M{"$set": bson.M{"status": "premium"}}
)

// Flow:
ArgonDriver.UpdateOne()
 ↓
WALInterceptor.UpdateOne()
 ↓
 ├── 1. Materialize current document state
 ├── 2. Apply update to compute new state
 ├── 3. Calculate delta
 ├── 4. Create WAL entry:
 │   {
 │     LSN: 12347,
 │     Operation: "update",
 │     DocumentID: "123",
 │     Changes: {
 │       Before: {_id: "123", name: "Alice", status: "basic"},
 │       After: {_id: "123", name: "Alice", status: "premium"},
 │       Delta: {status: "premium"}
 │     }
 │   }
 └── 5. Update branch HEAD

// History preserved!
```

## Flow 6: Time Travel

### Current System:
```go
// Not possible - no history
```

### New System:
```go
// User runs: argon restore feature-123 "1 hour ago"

// Flow:
CLI
 ↓
Engine.RestoreBranch("feature-123", targetTime)
 ↓
 ├── 1. Find LSN at target time:
 │   WAL.FindLSNAtTime("1 hour ago") → LSN 11000
 ├── 2. Create restore branch:
 │   {
 │     ID: "restore-feature-123-11000",
 │     Name: "restore-feature-123",
 │     HeadLSN: 11000,
 │     BaseLSN: 11000,
 │     ParentID: "feature-123"
 │   }
 └── 3. User can now query historical state!

// Queries on restore branch see data as it was 1 hour ago
```

## Flow 7: Delete Operation with Recovery

### Current System:
```go
// User accidentally deletes important data
db.Collection("users").DeleteOne(ctx, bson.M{"_id": "123"})

// Data is gone forever!
```

### New System:
```go
// User accidentally deletes important data
db.Collection("users").DeleteOne(ctx, bson.M{"_id": "123"})

// Flow:
WAL Entry created:
{
  LSN: 12348,
  Operation: "delete",
  DocumentID: "123",
  Changes: {
    Before: {_id: "123", name: "Alice", data: "important"}
  }
}

// User realizes mistake
argon restore feature-123 "5 minutes ago"

// On restore branch, document still exists!
db.Collection("users").FindOne(ctx, bson.M{"_id": "123"})
// Returns: {_id: "123", name: "Alice", data: "important"}
```

## Flow 8: Cross-Branch Queries

### New Feature - Query Multiple Branches:
```go
// Compare data across branches
engine.CompareBranches("main", "feature-123", "users", filter)

// Flow:
 ├── 1. Materialize "users" at main.HeadLSN
 ├── 2. Materialize "users" at feature-123.HeadLSN
 ├── 3. Compare documents
 └── 4. Return differences

// Result:
{
  OnlyInMain: [{_id: "456", name: "Bob"}],
  OnlyInFeature: [{_id: "789", name: "Charlie"}],
  Different: [{
    ID: "123",
    Main: {status: "basic"},
    Feature: {status: "premium"}
  }]
}
```

## Performance Optimizations

### 1. Parallel Materialization
```go
// When querying multiple collections
results := engine.QueryMultiple([]Query{
  {Collection: "users", Filter: userFilter},
  {Collection: "products", Filter: productFilter},
  {Collection: "orders", Filter: orderFilter},
})

// Materializes all three collections in parallel
```

### 2. Incremental Updates
```go
// For active branches, maintain live materialized state
type LiveBranch struct {
  BranchID string
  State    map[string]*CollectionState
  Subscriber chan WALEntry
}

// As WAL entries arrive, update state incrementally
func (lb *LiveBranch) HandleWALEntry(entry WALEntry) {
  if entry.BranchID == lb.BranchID {
    state := lb.State[entry.Collection]
    state.ApplyEntry(entry)
  }
}
```

### 3. Smart Caching
```go
// Cache hierarchy
type CacheSystem struct {
  L1 *ProcessCache     // In-process LRU
  L2 *RedisCache      // Shared Redis
  L3 *SnapshotStorage // Disk snapshots
}

// Check caches in order
func (c *CacheSystem) Get(branch, collection string, lsn int64) *State {
  if state := c.L1.Get(key); state != nil {
    return state // ~1ms
  }
  if state := c.L2.Get(key); state != nil {
    c.L1.Set(key, state)
    return state // ~10ms
  }
  if state := c.L3.Get(key); state != nil {
    c.L1.Set(key, state)
    c.L2.Set(key, state)
    return state // ~100ms
  }
  return nil // Must materialize from WAL
}
```

## Error Handling

### WAL Write Failure
```go
// If WAL write fails, operation fails
func (w *WALInterceptor) InsertOne(ctx context.Context, doc interface{}) error {
  lsn, err := w.wal.Append(entry)
  if err != nil {
    return fmt.Errorf("WAL write failed: %w", err)
    // No partial writes - atomicity preserved
  }
  // Success
}
```

### Materialization Failure
```go
// If materialization fails, fall back to snapshot
func (e *BranchExecutor) getMaterializedState(collection string) (*State, error) {
  state, err := e.materializeFromWAL(collection)
  if err != nil {
    // Try latest snapshot
    snapshot, err := e.getLatestSnapshot(collection)
    if err != nil {
      return nil, fmt.Errorf("cannot materialize: %w", err)
    }
    return snapshot.State, nil
  }
  return state, nil
}
```

## Monitoring and Metrics

```go
// Key metrics to track
type Metrics struct {
  WALAppendLatency    histogram
  MaterializeLatency  histogram
  CacheHitRate        gauge
  ActiveBranches      gauge
  WALSize             gauge
  SnapshotCount       gauge
}

// Example metric collection
func (w *WALService) Append(entry WALEntry) (int64, error) {
  start := time.Now()
  defer func() {
    metrics.WALAppendLatency.Observe(time.Since(start).Seconds())
  }()
  
  // ... append logic
}
```