# Open-Source Argon Rewrite Plan: Neon-Style Architecture

## Overview
Transform Argon from collection-prefix based branching to WAL-based branching with true version control.

## Current vs New Architecture

### Current (Collection Prefix)
```
Database: argon_db
├── users                    (main branch)
├── products                 (main branch)
├── feat123_users           (feature branch - full copy)
├── feat123_products        (feature branch - full copy)
└── feat456_users           (another branch - full copy)
```

### New (WAL-Based)
```
Database: argon_db
├── wal_entries             (all changes)
├── branch_metadata         (branch pointers)
├── snapshots              (periodic checkpoints)
└── change_streams         (real-time capture)
```

## Phase 1: Core Infrastructure Changes

### 1.1 Remove Collection Prefix Logic
```go
// OLD: branch/database.go
func (bd *BranchDatabase) getCollectionName(baseName string) string {
    if bd.prefix == "" {
        return baseName
    }
    return fmt.Sprintf("%s_%s", bd.prefix, baseName) // DELETE THIS
}

// NEW: Direct collection names
func (bd *BranchDatabase) getCollectionName(baseName string) string {
    return baseName // Always use base name
}
```

### 1.2 Add WAL Infrastructure
```go
// NEW: internal/wal/service.go
type WALService struct {
    db           *mongo.Database
    collection   *mongo.Collection
    currentLSN   atomic.Int64
    interceptor  *Interceptor
}

// NEW: internal/wal/models.go
type WALEntry struct {
    LSN        int64
    Timestamp  time.Time
    BranchID   string
    Operation  string
    Collection string
    DocumentID interface{}
    Changes    Changes
}
```

### 1.3 Replace Branch Model
```go
// OLD: branch/models.go
type Branch struct {
    ID           primitive.ObjectID
    Name         string
    StoragePath  string  // DELETE: No more separate storage
    IsMain       bool    // DELETE: All branches equal
}

// NEW: branch/models.go
type Branch struct {
    ID       string
    Name     string
    HeadLSN  int64    // Points to position in WAL
    BaseLSN  int64    // Where branch started
    ParentID string   // Parent branch
}
```

## Phase 2: MongoDB Driver Integration

### 2.1 Create Custom MongoDB Driver Wrapper
```go
// NEW: internal/driver/client.go
type ArgonClient struct {
    *mongo.Client
    wal         *wal.WALService
    interceptor *wal.Interceptor
}

func NewArgonClient(uri string, walService *wal.WALService) (*ArgonClient, error) {
    client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
    if err != nil {
        return nil, err
    }
    
    return &ArgonClient{
        Client:      client,
        wal:         walService,
        interceptor: wal.NewInterceptor(walService),
    }, nil
}

// Override Database method
func (c *ArgonClient) Database(name string, opts ...*options.DatabaseOptions) *ArgonDatabase {
    db := c.Client.Database(name, opts...)
    return &ArgonDatabase{
        Database:    db,
        interceptor: c.interceptor,
    }
}
```

### 2.2 Wrap Collection Operations
```go
// NEW: internal/driver/collection.go
type ArgonCollection struct {
    *mongo.Collection
    interceptor *wal.Interceptor
    branchID    string
}

func (c *ArgonCollection) InsertOne(ctx context.Context, document interface{}) (*mongo.InsertOneResult, error) {
    // 1. Capture operation in WAL
    lsn, err := c.interceptor.CaptureInsert(ctx, c.Name(), document, c.branchID)
    if err != nil {
        return nil, err
    }
    
    // 2. Update branch HEAD
    c.interceptor.UpdateBranchHead(c.branchID, lsn)
    
    // 3. Don't execute on MongoDB yet! (lazy execution)
    return &mongo.InsertOneResult{
        InsertedID: getDocumentID(document),
    }, nil
}
```

## Phase 3: Query Execution Engine

### 3.1 Replace Direct MongoDB Queries
```go
// OLD: Direct MongoDB query
func (s *Service) FindUsers(ctx context.Context, filter bson.M) ([]User, error) {
    collection := s.branchDB.Collection("users")
    cursor, err := collection.Find(ctx, filter)
    // ... direct MongoDB query
}

// NEW: Through Branch Executor
func (s *Service) FindUsers(ctx context.Context, filter bson.M) ([]User, error) {
    executor := s.GetBranchExecutor()
    documents, err := executor.Find(ctx, "users", filter)
    // ... materialized from WAL
}
```

### 3.2 Implement Materialization Engine
```go
// NEW: internal/executor/materializer.go
type Materializer struct {
    wal        *wal.WALService
    cache      cache.Cache
    snapshots  *SnapshotService
}

func (m *Materializer) MaterializeCollection(ctx context.Context, branchID string, collection string, targetLSN int64) (*CollectionState, error) {
    // 1. Check cache
    if cached := m.cache.Get(branchID, collection, targetLSN); cached != nil {
        return cached, nil
    }
    
    // 2. Find nearest snapshot
    snapshot, err := m.snapshots.FindNearest(branchID, collection, targetLSN)
    if err != nil {
        return nil, err
    }
    
    // 3. Load WAL entries
    entries, err := m.wal.GetRange(snapshot.LSN+1, targetLSN)
    if err != nil {
        return nil, err
    }
    
    // 4. Apply entries
    state := m.applyEntries(snapshot.State, entries)
    
    // 5. Cache result
    m.cache.Set(branchID, collection, targetLSN, state)
    
    return state, nil
}
```

## Phase 4: CLI Changes

### 4.1 Update Branch Commands
```go
// OLD: cli/cmd/branches.go
func createBranch(name string, parent string) error {
    // Complex logic to copy collections
    return engine.CopyBranchData(parent, name)
}

// NEW: cli/cmd/branches.go
func createBranch(name string, parent string) error {
    // Just create a pointer!
    return engine.CreateBranchPointer(name, parent)
}
```

### 4.2 Add Time-Travel Commands
```go
// NEW: cli/cmd/timetravel.go
var restoreCmd = &cobra.Command{
    Use:   "restore [branch] [time]",
    Short: "Restore branch to a point in time",
    Run: func(cmd *cobra.Command, args []string) {
        branch := args[0]
        targetTime := parseTime(args[1])
        
        restoredBranch, err := engine.RestoreToTime(branch, targetTime)
        if err != nil {
            log.Fatal(err)
        }
        
        fmt.Printf("Created restore branch: %s\n", restoredBranch.Name)
    },
}
```

## Phase 5: Storage Layer

### 5.1 Remove File-Based Storage
```go
// DELETE: internal/storage/file.go
// DELETE: internal/storage/s3.go

// NEW: internal/storage/wal_storage.go
type WALStorage struct {
    hot  Storage // Recent entries (MongoDB)
    cold Storage // Archived entries (S3)
}
```

### 5.2 Implement Page-Based Storage
```go
// NEW: internal/storage/pages.go
type Page struct {
    ID         string
    StartLSN   int64
    EndLSN     int64
    Entries    []wal.WALEntry
    Compressed []byte
}

type PageManager struct {
    pageSize     int
    currentPage  *Page
    storage      Storage
}

func (pm *PageManager) AddEntry(entry wal.WALEntry) error {
    if pm.currentPage.IsFull() {
        if err := pm.FlushPage(); err != nil {
            return err
        }
        pm.currentPage = NewPage()
    }
    
    pm.currentPage.AddEntry(entry)
    return nil
}
```

## New Flow Examples

### Flow 1: Insert Operation
```go
// User code
users := db.Collection("users")
users.InsertOne(ctx, User{Name: "Alice"})

// What happens:
1. ArgonCollection.InsertOne called
2. WAL entry created:
   {
     LSN: 12345,
     Operation: "insert",
     Collection: "users",
     Document: {Name: "Alice"},
     BranchID: "feature-123"
   }
3. Branch HEAD updated: feature-123.HeadLSN = 12345
4. Return success (no MongoDB write yet!)
```

### Flow 2: Query Operation
```go
// User code
users := db.Collection("users")
cursor, _ := users.Find(ctx, bson.M{"active": true})

// What happens:
1. ArgonCollection.Find called
2. Get current branch (feature-123) and HEAD (12345)
3. Materialize "users" collection at LSN 12345:
   - Load snapshot at LSN 12000
   - Apply WAL entries 12001-12345
   - Build in-memory state
4. Apply filter {"active": true}
5. Return cursor over materialized data
```

### Flow 3: Branch Creation
```go
// User code
argon branch create feature-456

// What happens:
1. Get current branch HEAD: main.HeadLSN = 12345
2. Create branch record:
   {
     ID: "feature-456",
     HeadLSN: 12345,
     BaseLSN: 12345,
     ParentID: "main"
   }
3. Done! (no data copy)
```

## Migration Strategy

### Step 1: Parallel Mode (2-4 weeks)
```go
// Both systems run together
type HybridEngine struct {
    oldEngine *branch.Service    // Current prefix-based
    newEngine *wal.Engine        // New WAL-based
}

func (h *HybridEngine) InsertOne(ctx context.Context, doc interface{}) error {
    // Write to both systems
    if err := h.oldEngine.InsertOne(ctx, doc); err != nil {
        return err
    }
    return h.newEngine.InsertOne(ctx, doc)
}
```

### Step 2: Read Migration (2-4 weeks)
```go
func (h *HybridEngine) Find(ctx context.Context, filter bson.M) ([]interface{}, error) {
    if FeatureFlag("use_wal_reads") {
        return h.newEngine.Find(ctx, filter)
    }
    return h.oldEngine.Find(ctx, filter)
}
```

### Step 3: Write Migration (2-4 weeks)
```go
func (h *HybridEngine) InsertOne(ctx context.Context, doc interface{}) error {
    if FeatureFlag("use_wal_writes") {
        return h.newEngine.InsertOne(ctx, doc)
    }
    return h.oldEngine.InsertOne(ctx, doc)
}
```

### Step 4: Cleanup (1-2 weeks)
- Remove old prefix-based code
- Drop prefixed collections
- Archive old data

## Testing Strategy

### 1. Unit Tests
```go
func TestWALAppend(t *testing.T) {
    wal := NewWALService()
    lsn, err := wal.Append(WALEntry{...})
    assert.NoError(t, err)
    assert.Greater(t, lsn, 0)
}
```

### 2. Integration Tests
```go
func TestBranchOperations(t *testing.T) {
    engine := NewEngine()
    
    // Create branch
    branch, _ := engine.CreateBranch("feature")
    
    // Insert on branch
    engine.WithBranch("feature").InsertOne(doc)
    
    // Query should see document
    docs, _ := engine.WithBranch("feature").Find(filter)
    assert.Len(t, docs, 1)
    
    // Main branch should not see it
    mainDocs, _ := engine.WithBranch("main").Find(filter)
    assert.Len(t, mainDocs, 0)
}
```

### 3. Performance Tests
```go
func BenchmarkBranchCreation(b *testing.B) {
    engine := NewEngine()
    
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        engine.CreateBranch(fmt.Sprintf("branch-%d", i))
    }
    // Should be <100ms per branch
}
```

## Challenges and Solutions

### Challenge 1: Query Performance
**Problem**: Materializing state adds latency
**Solution**: 
- Aggressive caching
- Background materialization
- Read replicas per branch

### Challenge 2: WAL Growth
**Problem**: WAL grows unbounded
**Solution**:
- Periodic snapshots
- WAL compaction
- Cold storage archival

### Challenge 3: Consistency
**Problem**: Eventually consistent reads
**Solution**:
- Synchronous replication for critical paths
- Read-your-writes consistency
- Conflict resolution strategies

## Timeline

1. **Weeks 1-2**: Core WAL infrastructure
2. **Weeks 3-4**: Driver integration
3. **Weeks 5-6**: Query engine
4. **Weeks 7-8**: CLI updates
5. **Weeks 9-10**: Testing & benchmarking
6. **Weeks 11-12**: Migration tooling
7. **Weeks 13-16**: Gradual rollout

Total: 4 months for complete rewrite