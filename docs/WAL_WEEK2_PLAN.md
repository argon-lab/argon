# WAL Week 2 Implementation Plan: Data Operations

## Goal
Enable actual data operations through WAL - insert, update, delete, and query.

## Architecture Overview

```
MongoDB Operations
       ↓
  WAL Interceptor    ← Week 2 Focus
       ↓
   WAL Service       ← Week 1 ✅
       ↓
  Materializer       ← Week 2 Focus
       ↓
  Query Results
```

## Day 1-2: Write Operations Interceptor

### What We're Building
A MongoDB driver wrapper that intercepts operations and writes to WAL instead.

### Implementation Plan

```go
// internal/driver/wal/interceptor.go
type Interceptor struct {
    wal       *wal.Service
    branch    *wal.Branch
    branches  *branchwal.BranchService
}

// InsertOne intercepts and redirects to WAL
func (i *Interceptor) InsertOne(collection string, document interface{}) (*InsertResult, error) {
    // 1. Generate document ID if missing
    docID := generateOrExtractID(document)
    
    // 2. Marshal to BSON
    docBytes, err := bson.Marshal(document)
    
    // 3. Append to WAL
    entry := &wal.Entry{
        ProjectID:  i.branch.ProjectID,
        BranchID:   i.branch.ID,
        Operation:  wal.OpInsert,
        Collection: collection,
        DocumentID: docID,
        Document:   docBytes,
    }
    
    lsn, err := i.wal.Append(entry)
    
    // 4. Update branch HEAD
    i.branches.UpdateBranchHead(i.branch.ID, lsn)
    
    return &InsertResult{InsertedID: docID}, nil
}
```

### Key Decisions
- No actual MongoDB writes
- Document ID generation handled
- Branch HEAD updated automatically

## Day 3-4: Basic Materializer

### What We're Building
A service that replays WAL entries to build current state.

### Implementation Plan

```go
// internal/materializer/simple.go
type SimpleMaterializer struct {
    wal      *wal.Service
    branches *branchwal.BranchService
}

// MaterializeCollection builds collection state from WAL
func (m *SimpleMaterializer) MaterializeCollection(
    branch *wal.Branch,
    collection string,
) (map[string]bson.M, error) {
    // 1. Get branch lineage
    lineage := m.getBranchLineage(branch)
    
    // 2. Collect relevant WAL entries
    var entries []*wal.Entry
    
    for _, ancestorBranch := range lineage {
        // Get entries for this branch
        branchEntries := m.wal.GetBranchEntries(
            ancestorBranch.ID,
            collection,
            0, // start
            ancestorBranch.HeadLSN, // end
        )
        
        // Only include up to fork point for ancestors
        if ancestorBranch.ID != branch.ID {
            branchEntries = filterUpToLSN(branchEntries, branch.BaseLSN)
        }
        
        entries = append(entries, branchEntries...)
    }
    
    // 3. Sort by LSN
    sort.Slice(entries, func(i, j int) bool {
        return entries[i].LSN < entries[j].LSN
    })
    
    // 4. Apply entries to build state
    state := make(map[string]bson.M)
    for _, entry := range entries {
        m.applyEntry(state, entry)
    }
    
    return state, nil
}

// applyEntry applies a single WAL entry to state
func (m *SimpleMaterializer) applyEntry(
    state map[string]bson.M,
    entry *wal.Entry,
) {
    switch entry.Operation {
    case wal.OpInsert:
        var doc bson.M
        bson.Unmarshal(entry.Document, &doc)
        state[entry.DocumentID] = doc
        
    case wal.OpUpdate:
        var doc bson.M
        bson.Unmarshal(entry.Document, &doc)
        state[entry.DocumentID] = doc
        
    case wal.OpDelete:
        delete(state, entry.DocumentID)
    }
}
```

### Key Decisions
- Include parent history up to fork point
- Apply entries in LSN order
- Simple map-based state

## Day 5: Query Engine Integration

### What We're Building
Query executor that runs MongoDB-style queries on materialized state.

### Implementation Plan

```go
// internal/driver/wal/collection.go
type WALCollection struct {
    name         string
    branch       *wal.Branch
    interceptor  *Interceptor
    materializer *SimpleMaterializer
}

// Find executes a query on the materialized state
func (c *WALCollection) Find(filter bson.M) ([]bson.M, error) {
    // 1. Materialize collection state
    state, err := c.materializer.MaterializeCollection(
        c.branch,
        c.name,
    )
    
    // 2. Apply filter
    var results []bson.M
    for _, doc := range state {
        if matchesFilter(doc, filter) {
            results = append(results, doc)
        }
    }
    
    return results, nil
}

// matchesFilter implements basic MongoDB query matching
func matchesFilter(doc, filter bson.M) bool {
    for key, expected := range filter {
        actual, exists := doc[key]
        if !exists {
            return false
        }
        
        // Simple equality for MVP
        if !reflect.DeepEqual(actual, expected) {
            return false
        }
    }
    return true
}
```

### Supported Query Operations (MVP)
- Equality matching: `{name: "Alice"}`
- AND queries: `{name: "Alice", age: 30}`
- Empty filter: `{}` returns all

### NOT Supported Yet
- Operators: `$gt`, `$lt`, `$in`, etc.
- Nested fields: `{"address.city": "NYC"}`
- Regex, arrays, complex queries

## Testing Plan

### Day 1-2 Tests
```go
func TestInterceptor_InsertOne(t *testing.T) {
    // Test document insertion goes to WAL
    // Test branch HEAD updates
    // Test ID generation
}

func TestInterceptor_UpdateOne(t *testing.T) {
    // Test update operations
    // Test old document capture
}
```

### Day 3-4 Tests
```go
func TestMaterializer_BranchLineage(t *testing.T) {
    // Test parent history included correctly
    // Test fork point respected
}

func TestMaterializer_ApplyOperations(t *testing.T) {
    // Test insert/update/delete sequence
    // Test state building
}
```

### Day 5 Tests
```go
func TestWALCollection_Find(t *testing.T) {
    // Test basic queries work
    // Test branch isolation
    // Test parent data inheritance
}
```

## Success Metrics

1. **Functional**
   - All CRUD operations working
   - Branch isolation maintained
   - Parent history included correctly

2. **Performance**
   - Insert: < 50ms
   - Update: < 50ms
   - Simple query: < 200ms for 1000 docs

3. **Quality**
   - All tests passing
   - No data loss
   - Correct branch semantics

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Materialization slow | High | Start simple, optimize later |
| Complex queries | Medium | Support basic queries only |
| Memory usage | Medium | Limit collection size for MVP |

## Daily Milestones

**Monday (Day 1-2)**
- [ ] Interceptor implementation
- [ ] Insert/Update/Delete operations
- [ ] Tests passing

**Wednesday (Day 3-4)**
- [ ] Materializer implementation
- [ ] Branch lineage logic
- [ ] State building tests

**Friday (Day 5)**
- [ ] Query engine integration
- [ ] End-to-end test
- [ ] Performance validation

## Next Steps After Week 2

With data operations complete, Week 3 will add:
- Time travel (restore to any LSN)
- CLI integration
- Migration tools
- Documentation