# 5-Week WAL Implementation Plan for Open-Source Argon

## Overview
Aggressive but achievable plan to add WAL-based branching to Argon while maintaining full backward compatibility.

## Strategy: Parallel Implementation
Build WAL alongside existing system, not replacing it. This allows for gradual migration and rollback capability.

## Week 1: WAL Foundation & Dual-Mode Setup

### Day 1-2: WAL Core
```go
// internal/wal/core.go
type WALEntry struct {
    LSN        int64      `bson:"lsn"`
    Timestamp  time.Time  `bson:"timestamp"`
    BranchID   string     `bson:"branch_id"`
    Operation  string     `bson:"operation"`
    Collection string     `bson:"collection"`
    DocumentID string     `bson:"document_id"`
    Document   bson.Raw   `bson:"document"`
}

type WALService struct {
    db         *mongo.Database
    collection *mongo.Collection
    currentLSN atomic.Int64
}

// MVP: Simple append, no buffering yet
func (w *WALService) Append(entry WALEntry) (int64, error) {
    lsn := w.currentLSN.Add(1)
    entry.LSN = lsn
    entry.Timestamp = time.Now()
    
    _, err := w.collection.InsertOne(context.Background(), entry)
    return lsn, err
}
```

### Day 3-4: Branch Model Extension
```go
// internal/branch/models.go
type Branch struct {
    // Existing fields
    ID          primitive.ObjectID `bson:"_id,omitempty"`
    Name        string             `bson:"name"`
    ProjectID   primitive.ObjectID `bson:"project_id"`
    
    // NEW: WAL support
    UseWAL      bool               `bson:"use_wal,omitempty"`
    HeadLSN     int64              `bson:"head_lsn,omitempty"`
    BaseLSN     int64              `bson:"base_lsn,omitempty"`
}
```

### Day 5: Feature Flag System
```go
// internal/config/features.go
type Features struct {
    EnableWALBranches bool `env:"ENABLE_WAL_BRANCHES" default:"false"`
    WALNewBranchesOnly bool `env:"WAL_NEW_BRANCHES_ONLY" default:"true"`
}

// Check if branch should use WAL
func ShouldUseWAL(branch *Branch) bool {
    if !features.EnableWALBranches {
        return false
    }
    return branch.UseWAL || (branch.CreatedAt.After(walCutoffDate) && features.WALNewBranchesOnly)
}
```

## Week 2: MongoDB Driver Wrapper

### Day 6-7: Collection Wrapper
```go
// internal/driver/collection.go
type CollectionWrapper struct {
    *mongo.Collection
    branch    *branch.Branch
    wal       *wal.WALService
    original  *mongo.Collection
}

func (c *CollectionWrapper) InsertOne(ctx context.Context, document interface{}) (*mongo.InsertOneResult, error) {
    if !ShouldUseWAL(c.branch) {
        // Use existing prefix-based approach
        return c.original.InsertOne(ctx, document)
    }
    
    // WAL path
    docBytes, _ := bson.Marshal(document)
    lsn, err := c.wal.Append(wal.WALEntry{
        BranchID:   c.branch.ID.Hex(),
        Operation:  "insert",
        Collection: c.Collection.Name(),
        Document:   docBytes,
    })
    
    // Update branch HEAD
    c.branch.HeadLSN = lsn
    
    // Return success without writing to MongoDB
    return &mongo.InsertOneResult{InsertedID: getID(document)}, err
}
```

### Day 8-9: Update/Delete Operations
```go
func (c *CollectionWrapper) UpdateOne(ctx context.Context, filter, update interface{}) (*mongo.UpdateResult, error) {
    if !ShouldUseWAL(c.branch) {
        return c.original.UpdateOne(ctx, filter, update)
    }
    
    // For WAL, we need to materialize first to know what we're updating
    executor := NewSimpleExecutor(c.branch, c.wal)
    docs, _ := executor.Find(ctx, c.Collection.Name(), filter)
    
    if len(docs) > 0 {
        // Apply update and store in WAL
        updated := applyUpdate(docs[0], update)
        c.wal.Append(wal.WALEntry{
            BranchID:   c.branch.ID.Hex(),
            Operation:  "update",
            Collection: c.Collection.Name(),
            DocumentID: getID(docs[0]),
            Document:   bson.Marshal(updated),
        })
    }
    
    return &mongo.UpdateResult{MatchedCount: len(docs), ModifiedCount: len(docs)}, nil
}
```

### Day 10: Integration with BranchDatabase
```go
// internal/branch/database.go
func (bd *BranchDatabase) Collection(name string) *mongo.Collection {
    if ShouldUseWAL(bd.branch) {
        // Return wrapped collection for WAL branches
        return &CollectionWrapper{
            Collection: bd.Database().Collection(name),
            branch:     bd.branch,
            wal:        bd.walService,
            original:   bd.Database().Collection(bd.getCollectionName(name)),
        }
    }
    
    // Existing prefix-based approach
    return bd.Database().Collection(bd.getCollectionName(name))
}
```

## Week 3: Simple Query Engine

### Day 11-12: Basic Materializer (No Caching)
```go
// internal/executor/simple.go
type SimpleExecutor struct {
    branch *branch.Branch
    wal    *wal.WALService
}

func (e *SimpleExecutor) Find(ctx context.Context, collection string, filter bson.M) ([]bson.M, error) {
    // Get all WAL entries for this branch and collection
    entries, err := e.wal.GetBranchEntries(e.branch.ID.Hex(), collection, 0, e.branch.HeadLSN)
    if err != nil {
        return nil, err
    }
    
    // Build state by replaying entries
    state := make(map[string]bson.M)
    for _, entry := range entries {
        switch entry.Operation {
        case "insert", "update":
            var doc bson.M
            bson.Unmarshal(entry.Document, &doc)
            state[entry.DocumentID] = doc
        case "delete":
            delete(state, entry.DocumentID)
        }
    }
    
    // Convert to slice and filter
    results := make([]bson.M, 0, len(state))
    for _, doc := range state {
        if matchesFilter(doc, filter) {
            results = append(results, doc)
        }
    }
    
    return results, nil
}
```

### Day 13-14: Basic Filter Matching
```go
// internal/executor/filter.go
func matchesFilter(doc bson.M, filter bson.M) bool {
    for key, value := range filter {
        docValue, exists := doc[key]
        if !exists {
            return false
        }
        
        // Simple equality for MVP
        if !reflect.DeepEqual(docValue, value) {
            return false
        }
    }
    return true
}
```

### Day 15: Simple In-Memory Cache
```go
// internal/executor/cache.go
type SimpleCache struct {
    mu    sync.RWMutex
    cache map[string]*CacheEntry
}

type CacheEntry struct {
    State      map[string]bson.M
    LSN        int64
    Expiry     time.Time
}

func (c *SimpleCache) Get(branchID, collection string, lsn int64) map[string]bson.M {
    c.mu.RLock()
    defer c.mu.RUnlock()
    
    key := fmt.Sprintf("%s:%s:%d", branchID, collection, lsn)
    entry, ok := c.cache[key]
    if !ok || entry.Expiry.Before(time.Now()) {
        return nil
    }
    
    return entry.State
}
```

## Week 4: CLI Integration & Performance

### Day 16-17: Branch Creation for WAL
```go
// internal/branch/service.go
func (s *Service) CreateBranch(ctx context.Context, req *BranchCreateRequest) (*Branch, error) {
    // Determine if new branch should use WAL
    useWAL := features.EnableWALBranches && (req.UseWAL || features.WALNewBranchesOnly)
    
    if useWAL {
        // WAL branch creation - just metadata!
        parentBranch, _ := s.GetBranch(ctx, req.ParentBranch)
        branch := &Branch{
            ID:        primitive.NewObjectID(),
            Name:      req.Name,
            ProjectID: req.ProjectID,
            UseWAL:    true,
            HeadLSN:   parentBranch.HeadLSN,
            BaseLSN:   parentBranch.HeadLSN,
            CreatedAt: time.Now(),
        }
        
        _, err := s.db.Collection("branches").InsertOne(ctx, branch)
        return branch, err
    }
    
    // Existing collection copy approach
    return s.createTraditionalBranch(ctx, req)
}
```

### Day 18-19: Performance Optimizations
```go
// Parallel materialization for multiple collections
func (e *SimpleExecutor) MaterializeMultiple(collections []string) map[string]map[string]bson.M {
    results := make(map[string]map[string]bson.M)
    var wg sync.WaitGroup
    var mu sync.Mutex
    
    for _, coll := range collections {
        wg.Add(1)
        go func(collection string) {
            defer wg.Done()
            state, _ := e.materializeCollection(collection)
            mu.Lock()
            results[collection] = state
            mu.Unlock()
        }(coll)
    }
    
    wg.Wait()
    return results
}

// Batch WAL reads
func (w *WALService) GetBranchEntriesBatch(branchID string, startLSN, endLSN int64) ([]WALEntry, error) {
    return w.collection.Find(context.Background(), bson.M{
        "branch_id": branchID,
        "lsn": bson.M{
            "$gte": startLSN,
            "$lte": endLSN,
        },
    }).Sort("lsn", 1).Batch(1000).All()
}
```

### Day 20: CLI Feature Flags
```bash
# Add to CLI
argon branches create my-feature --use-wal
argon config set wal.enabled true
argon branches list --show-type  # Shows [WAL] or [Traditional]
```

## Week 5: Testing, Migration & Rollout

### Day 21-22: Comprehensive Testing
```go
// tests/wal_integration_test.go
func TestWALBranchOperations(t *testing.T) {
    // Test both WAL and traditional branches
    tests := []struct {
        name   string
        useWAL bool
    }{
        {"Traditional", false},
        {"WAL", true},
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            branch := createBranch(t, tt.useWAL)
            
            // Insert
            doc := bson.M{"name": "test", "value": 123}
            err := branch.Collection("test").InsertOne(ctx, doc)
            assert.NoError(t, err)
            
            // Query
            results, err := branch.Collection("test").Find(ctx, bson.M{})
            assert.NoError(t, err)
            assert.Len(t, results, 1)
            
            // Update
            err = branch.Collection("test").UpdateOne(ctx, 
                bson.M{"name": "test"}, 
                bson.M{"$set": bson.M{"value": 456}})
            assert.NoError(t, err)
            
            // Verify update
            results, err = branch.Collection("test").Find(ctx, bson.M{"name": "test"})
            assert.Equal(t, 456, results[0]["value"])
        })
    }
}
```

### Day 23: Migration Tools
```go
// tools/migrate_branch.go
func MigrateBranchToWAL(branchID string) error {
    branch := getBranch(branchID)
    if branch.UseWAL {
        return errors.New("already using WAL")
    }
    
    // Get all collections for this branch
    collections := listBranchCollections(branch)
    
    // Copy data to WAL
    for _, coll := range collections {
        docs := getAllDocuments(branch, coll)
        for _, doc := range docs {
            wal.Append(WALEntry{
                BranchID:   branchID,
                Operation:  "insert",
                Collection: coll,
                Document:   doc,
            })
        }
    }
    
    // Mark branch as WAL-enabled
    branch.UseWAL = true
    branch.HeadLSN = wal.CurrentLSN()
    updateBranch(branch)
    
    return nil
}
```

### Day 24: Performance Validation
```go
// benchmarks/wal_bench_test.go
func BenchmarkBranchCreation(b *testing.B) {
    b.Run("Traditional", func(b *testing.B) {
        for i := 0; i < b.N; i++ {
            createTraditionalBranch()
        }
    })
    
    b.Run("WAL", func(b *testing.B) {
        for i := 0; i < b.N; i++ {
            createWALBranch()
        }
    })
}

// Expected results:
// Traditional: 500-1000ms per branch
// WAL: 5-10ms per branch
```

### Day 25: Documentation & Rollout Plan
```yaml
# rollout.yaml
phases:
  - name: "Internal Testing"
    duration: "1 week"
    actions:
      - Enable WAL for test projects
      - Monitor performance metrics
      - Fix any bugs
      
  - name: "Beta Users"  
    duration: "2 weeks"
    actions:
      - Enable for 10% of new branches
      - Gather feedback
      - Performance tuning
      
  - name: "General Availability"
    duration: "ongoing"
    actions:
      - Enable for all new branches
      - Provide migration tool
      - Deprecate traditional branches
```

## Key Simplifications for 5-Week Timeline

1. **No Snapshots** - Always replay full WAL (add later)
2. **Simple Caching** - In-memory only, no Redis
3. **Basic Filters** - Equality only, no complex queries
4. **No Compression** - Raw BSON storage
5. **No Sharding** - Single WAL collection
6. **No Transactions** - Single document atomicity only

## Success Metrics

- [ ] Branch creation < 50ms for WAL branches
- [ ] Query performance < 200ms for small collections
- [ ] All existing tests pass with WAL enabled
- [ ] Zero data loss in migration
- [ ] Backward compatibility maintained

## Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Performance regression | Feature flags for rollback |
| Data loss | Dual-write during migration |
| Complexity | Keep traditional system intact |
| Bug in materialization | Comprehensive test suite |

## Next Steps After 5 Weeks

1. Add snapshot support (Week 6-7)
2. Implement compression (Week 8)
3. Add Redis caching (Week 9)
4. Complex query support (Week 10-11)
5. Production optimization (Week 12+)