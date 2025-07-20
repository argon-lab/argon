# Open-Source Argon Rewrite: Implementation Summary

## Key Changes Required

### 1. **Remove Collection Prefixing** ❌
```go
// DELETE THIS PATTERN EVERYWHERE:
collectionName := fmt.Sprintf("%s_%s", branchPrefix, baseName)

// No more:
- main_users, feature123_users, feature456_users
- Copying data between prefixed collections
- Managing collection name mappings
```

### 2. **Add WAL System** ✅
```go
// NEW CORE COMPONENT:
type WALService struct {
    collection *mongo.Collection // Single "wal_entries" collection
    currentLSN atomic.Int64      // Global sequence number
}

// Every operation goes through WAL first
InsertOne() → WAL.Append() → Return
Find()      → Materialize() → Filter → Return
```

### 3. **Change Branch Model** 🔄
```go
// OLD Branch:
type Branch struct {
    Name        string
    Collections []string // Which collections to copy
    StoragePath string   // Where to store data
}

// NEW Branch:
type Branch struct {
    Name    string
    HeadLSN int64  // Just a pointer!
    BaseLSN int64  // Where it started
}
```

### 4. **Wrap MongoDB Driver** 🎯
```go
// Create custom driver that intercepts all operations
type ArgonClient struct {
    *mongo.Client
    wal *WALService
}

// User code stays the same:
db.Collection("users").InsertOne(ctx, user)
// But now goes through our interceptor
```

### 5. **Build Materialization Engine** 🏗️
```go
// Convert WAL entries to queryable state
type Materializer struct {
    wal       *WALService
    snapshots *SnapshotService
    cache     *CacheService
}

// On-demand state reconstruction:
// Snapshot + WAL entries = Current state
```

## File Structure Changes

### Delete These Files:
```
❌ internal/branch/database.go    (prefix-based collections)
❌ internal/storage/file.go       (file-based storage)
❌ internal/storage/delta.go      (old delta system)
```

### Add These Files:
```
✅ internal/wal/
   ├── service.go      (WAL core service)
   ├── interceptor.go  (MongoDB interceptor)
   ├── models.go       (WAL entry types)
   └── storage.go      (WAL persistence)

✅ internal/executor/
   ├── branch.go       (Branch executor)
   ├── materializer.go (State reconstruction)
   └── cache.go        (Caching layer)

✅ internal/driver/
   ├── client.go       (Wrapped MongoDB client)
   ├── database.go     (Wrapped database)
   └── collection.go   (Wrapped collection)
```

## Migration Path

### Phase 1: Foundation (Weeks 1-2)
1. Implement WAL service
2. Create interceptor framework
3. Set up basic tests

### Phase 2: Integration (Weeks 3-4)
1. Wrap MongoDB driver
2. Implement materializer
3. Update branch operations

### Phase 3: Migration (Weeks 5-6)
1. Dual-write mode (both systems)
2. Migrate read path
3. Migrate write path

### Phase 4: Cleanup (Week 7-8)
1. Remove old code
2. Performance tuning
3. Documentation

## Code Examples

### Before: Creating a Branch
```go
// 30+ seconds, copies all data
func CreateBranch(name string) error {
    // List all collections
    collections := db.ListCollections()
    
    // Copy each collection
    for _, coll := range collections {
        srcName := coll
        dstName := fmt.Sprintf("%s_%s", name, coll)
        
        // Copy all documents
        CopyCollection(srcName, dstName)
        
        // Copy indexes
        CopyIndexes(srcName, dstName)
    }
    
    return nil
}
```

### After: Creating a Branch
```go
// <10ms, just metadata
func CreateBranch(name string) error {
    parentLSN := GetCurrentLSN()
    
    branch := Branch{
        Name:    name,
        HeadLSN: parentLSN,
        BaseLSN: parentLSN,
    }
    
    return SaveBranch(branch)
}
```

### Before: Querying a Branch
```go
// Direct MongoDB query
func Find(branch, collection string, filter bson.M) ([]bson.M, error) {
    collName := fmt.Sprintf("%s_%s", branch, collection)
    return db.Collection(collName).Find(ctx, filter)
}
```

### After: Querying a Branch
```go
// Through materialization
func Find(branch, collection string, filter bson.M) ([]bson.M, error) {
    // Get branch state
    b := GetBranch(branch)
    
    // Materialize collection at branch HEAD
    state := Materialize(collection, b.HeadLSN)
    
    // Apply filter
    return ApplyFilter(state.Documents, filter)
}
```

## Benefits After Rewrite

### Storage
- **Before**: 10 branches = 10x storage
- **After**: 10 branches = 1x storage + small WAL

### Performance
- **Branch Creation**: 30s → 10ms (3000x faster)
- **Time Travel**: Impossible → Instant
- **Query Speed**: Direct → +10-50ms overhead (acceptable)

### Features
- ✅ True version control
- ✅ Point-in-time recovery
- ✅ Audit trail
- ✅ Deleted data recovery
- ✅ Cross-branch comparison

## Testing Strategy

```go
// Test WAL functionality
func TestWALOperations(t *testing.T) {
    wal := NewWALService()
    
    // Test append
    lsn, err := wal.Append(entry)
    assert.NoError(t, err)
    assert.Equal(t, int64(1), lsn)
    
    // Test retrieval
    entries, err := wal.GetRange(1, 1)
    assert.Len(t, entries, 1)
}

// Test branch isolation
func TestBranchIsolation(t *testing.T) {
    engine := NewEngine()
    
    // Insert on main
    engine.WithBranch("main").Insert(doc1)
    
    // Create branch
    engine.CreateBranch("feature")
    
    // Insert on feature
    engine.WithBranch("feature").Insert(doc2)
    
    // Main sees only doc1
    mainDocs := engine.WithBranch("main").Find({})
    assert.Len(t, mainDocs, 1)
    
    // Feature sees both
    featureDocs := engine.WithBranch("feature").Find({})
    assert.Len(t, featureDocs, 2)
}
```

## Rollback Plan

If issues arise:
1. **Feature flag**: Disable WAL path, revert to prefix-based
2. **Dual mode**: Keep both systems running
3. **Data export**: Export WAL to prefixed collections
4. **Gradual rollback**: Move branches back one by one

## Success Metrics

- Branch creation time < 100ms
- Storage reduction > 50%
- Query overhead < 100ms
- Zero data loss during migration
- All existing CLI commands work unchanged