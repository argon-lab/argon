# WAL Core Design for Argon Open-Source

## Overview
This document provides detailed flows and implementation design for WAL-based branching in Argon. We focus on core features that can be implemented in 3 weeks.

## WAL Foundation Architecture

### Core Components

```go
// 1. WAL Entry Structure
type WALEntry struct {
    LSN          int64            `bson:"lsn" json:"lsn"`
    Timestamp    time.Time        `bson:"timestamp" json:"timestamp"`
    ProjectID    string           `bson:"project_id" json:"project_id"`
    BranchID     string           `bson:"branch_id" json:"branch_id"`
    Operation    OperationType    `bson:"operation" json:"operation"`
    Collection   string           `bson:"collection" json:"collection"`
    DocumentID   string           `bson:"document_id" json:"document_id"`
    Document     bson.Raw         `bson:"document,omitempty" json:"-"`
    OldDocument  bson.Raw         `bson:"old_document,omitempty" json:"-"`
    Metadata     map[string]any   `bson:"metadata,omitempty" json:"metadata,omitempty"`
}

// 2. Operation Types
type OperationType string
const (
    OpInsert         OperationType = "insert"
    OpUpdate         OperationType = "update"
    OpDelete         OperationType = "delete"
    OpCreateBranch   OperationType = "create_branch"
    OpDeleteBranch   OperationType = "delete_branch"
    OpCreateProject  OperationType = "create_project"
    OpDeleteProject  OperationType = "delete_project"
)

// 3. Branch Structure for WAL
type WALBranch struct {
    ID          string    `bson:"_id"`
    ProjectID   string    `bson:"project_id"`
    Name        string    `bson:"name"`
    ParentID    string    `bson:"parent_id,omitempty"`
    HeadLSN     int64     `bson:"head_lsn"`
    BaseLSN     int64     `bson:"base_lsn"`
    CreatedAt   time.Time `bson:"created_at"`
    CreatedLSN  int64     `bson:"created_lsn"`
    DeletedLSN  int64     `bson:"deleted_lsn,omitempty"`
    IsDeleted   bool      `bson:"is_deleted"`
}
```

### WAL Storage Design

```
MongoDB Collections:
├── wal_log              # All WAL entries
│   └── Indexes:
│       ├── lsn (unique)
│       ├── project_id + lsn
│       └── branch_id + collection + lsn
├── wal_branches         # Branch metadata
│   └── Indexes:
│       ├── project_id + name
│       └── project_id + is_deleted
└── wal_checkpoints      # Materialized states (future)
```

## Feature Flows

### 1. Create Project Flow

```
User: argon project create "ml-experiments"
         |
         v
┌─────────────────────┐
│  Generate Project   │
│  ID & Main Branch   │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Append WAL Entry   │
│  OpCreateProject    │
│  LSN: 1             │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Create Main Branch │
│  HeadLSN: 1         │
│  BaseLSN: 0         │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Return Project ID  │
└─────────────────────┘

WAL Entry:
{
  "lsn": 1,
  "timestamp": "2025-01-19T10:00:00Z",
  "project_id": "proj_abc123",
  "branch_id": "main",
  "operation": "create_project",
  "metadata": {
    "project_name": "ml-experiments",
    "main_branch_id": "main"
  }
}
```

### 2. Create Branch Flow

```
User: argon branch create "feature-x" --from main
         |
         v
┌─────────────────────┐
│  Get Parent Branch  │
│  main @ LSN 100     │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Append WAL Entry   │
│  OpCreateBranch     │
│  LSN: 101           │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Create New Branch  │
│  HeadLSN: 100       │ ← Inherits parent's HEAD
│  BaseLSN: 100       │ ← Fork point
│  CreatedLSN: 101    │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Return Branch ID   │
└─────────────────────┘

WAL Entry:
{
  "lsn": 101,
  "timestamp": "2025-01-19T10:05:00Z",
  "project_id": "proj_abc123",
  "branch_id": "feature-x",
  "operation": "create_branch",
  "metadata": {
    "parent_branch": "main",
    "parent_lsn": 100,
    "branch_name": "feature-x"
  }
}
```

### 3. Data Write Flow (Insert)

```
User: db.users.insertOne({name: "Alice", role: "ML Engineer"})
         |
         v
┌─────────────────────┐
│  Intercept MongoDB  │
│  Operation          │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Generate Doc ID    │
│  doc_123456         │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Append WAL Entry   │
│  OpInsert           │
│  LSN: 102           │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Update Branch HEAD │
│  HeadLSN: 102       │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Return Success     │
│  (No MongoDB write) │
└─────────────────────┘

WAL Entry:
{
  "lsn": 102,
  "timestamp": "2025-01-19T10:10:00Z",
  "project_id": "proj_abc123",
  "branch_id": "feature-x",
  "operation": "insert",
  "collection": "users",
  "document_id": "doc_123456",
  "document": {
    "_id": "doc_123456",
    "name": "Alice",
    "role": "ML Engineer",
    "created_at": "2025-01-19T10:10:00Z"
  }
}
```

### 4. Data Read Flow (Query)

```
User: db.users.find({role: "ML Engineer"})
         |
         v
┌─────────────────────┐
│  Get Branch Info    │
│  HeadLSN: 150       │
│  BaseLSN: 100       │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Fetch WAL Entries  │
│  LSN: 0 to 150      │ ← All entries up to HEAD
│  Filter: users      │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Build State Map    │
│  Apply Operations   │
│  Insert → Add       │
│  Update → Modify    │
│  Delete → Remove    │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Apply Query Filter │
│  role="ML Engineer" │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Return Results     │
└─────────────────────┘

Materialization Process:
1. Start with empty state: {}
2. LSN 102: Insert doc_123456 → {doc_123456: {name: "Alice", role: "ML Engineer"}}
3. LSN 125: Update doc_123456 → {doc_123456: {name: "Alice", role: "Senior ML Engineer"}}
4. LSN 140: Insert doc_789012 → {doc_123456: {...}, doc_789012: {name: "Bob", role: "ML Engineer"}}
5. Apply filter → Return matching documents
```

### 5. Delete Branch Flow (Simplified for MVP)

```
User: argon branch delete "feature-x"
         |
         v
┌─────────────────────┐
│  Validate Branch    │
│  - Not main branch  │
│  - No children      │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Delete Branch      │
│  Record Only        │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Return Success     │
│  (Keep WAL entries) │
└─────────────────────┘

Note: For MVP, we just delete the branch pointer.
WAL entries remain for data integrity and can be
cleaned up manually later if needed.

Future enhancement: Add cleanup command
`argon admin wal cleanup --older-than 30d`
```

### 6. Time Travel Flow

```
User: argon branch restore "main" --to "2025-01-19 10:30:00"
         |
         v
┌─────────────────────┐
│  Convert Timestamp  │
│  to LSN Range       │
│  Target: LSN ≤ 120  │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Create New Branch  │
│  "main-restore-120" │
│  HeadLSN: 120       │
│  BaseLSN: 0         │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Append WAL Entry   │
│  OpCreateBranch     │
│  with restore meta  │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Return New Branch  │
└─────────────────────┘

Time Travel Options:
1. Point-in-time: Create branch at specific LSN
2. Rollback: Reset existing branch HEAD to earlier LSN
3. Fork: Create new branch from historical point
```

## Implementation Details

### 1. WAL Service Core (Week 1)

```go
// internal/wal/service.go
type WALService struct {
    db          *mongo.Database
    walColl     *mongo.Collection
    branchColl  *mongo.Collection
    currentLSN  atomic.Int64
    mu          sync.RWMutex
}

func (w *WALService) AppendEntry(entry *WALEntry) (int64, error) {
    // 1. Generate LSN atomically
    lsn := w.currentLSN.Add(1)
    entry.LSN = lsn
    entry.Timestamp = time.Now()
    
    // 2. Insert to WAL
    _, err := w.walColl.InsertOne(context.Background(), entry)
    if err != nil {
        w.currentLSN.Add(-1) // Rollback LSN
        return 0, err
    }
    
    // 3. Update branch HEAD if data operation
    if entry.BranchID != "" && isDataOperation(entry.Operation) {
        w.updateBranchHead(entry.BranchID, lsn)
    }
    
    return lsn, nil
}
```

### 2. Query Materializer (Week 2)

```go
// internal/wal/materializer.go
type Materializer struct {
    wal    *WALService
    cache  *StateCache
}

func (m *Materializer) Materialize(branchID, collection string, upToLSN int64) (map[string]bson.M, error) {
    // 1. Check cache
    if state := m.cache.Get(branchID, collection, upToLSN); state != nil {
        return state, nil
    }
    
    // 2. Get branch lineage (for branch inheritance)
    lineage := m.getBranchLineage(branchID)
    
    // 3. Fetch relevant WAL entries
    entries := m.wal.GetEntries(bson.M{
        "branch_id": bson.M{"$in": lineage},
        "collection": collection,
        "lsn": bson.M{"$lte": upToLSN},
    })
    
    // 4. Build state
    state := make(map[string]bson.M)
    for _, entry := range entries {
        m.applyEntry(state, entry)
    }
    
    // 5. Cache result
    m.cache.Set(branchID, collection, upToLSN, state)
    
    return state, nil
}
```

### 3. MongoDB Driver Wrapper (Week 2-3)

```go
// internal/driver/wal_collection.go
type WALCollection struct {
    name      string
    branch    *WALBranch
    wal       *WALService
    material  *Materializer
}

func (c *WALCollection) Find(ctx context.Context, filter bson.M) (*Cursor, error) {
    // 1. Materialize collection state
    state, err := c.material.Materialize(c.branch.ID, c.name, c.branch.HeadLSN)
    if err != nil {
        return nil, err
    }
    
    // 2. Apply filter to materialized state
    results := []bson.M{}
    for _, doc := range state {
        if matchesFilter(doc, filter) {
            results = append(results, doc)
        }
    }
    
    // 3. Return cursor-like interface
    return &Cursor{results: results, pos: 0}, nil
}

func (c *WALCollection) InsertOne(ctx context.Context, document interface{}) (*InsertOneResult, error) {
    // 1. Convert to BSON
    docBytes, _ := bson.Marshal(document)
    
    // 2. Generate ID if needed
    docID := getOrGenerateID(document)
    
    // 3. Append to WAL
    lsn, err := c.wal.AppendEntry(&WALEntry{
        ProjectID:  c.branch.ProjectID,
        BranchID:   c.branch.ID,
        Operation:  OpInsert,
        Collection: c.name,
        DocumentID: docID,
        Document:   docBytes,
    })
    
    // 4. Update branch HEAD
    c.branch.HeadLSN = lsn
    
    return &InsertOneResult{InsertedID: docID}, err
}
```

## 3-Week Implementation Plan

### Week 1: WAL Foundation
- Day 1-2: WAL entry structure and storage
- Day 3-4: Branch management with WAL
- Day 5: Basic append and read operations

### Week 2: Core Operations  
- Day 1-2: Create/Delete project and branch
- Day 3-4: Insert/Update/Delete operations
- Day 5: Basic query materialization

### Week 3: Query Engine & Time Travel
- Day 1-2: Driver wrapper integration
- Day 3-4: Time travel implementation
- Day 5: Testing and bug fixes

## Performance Considerations

### 1. LSN Generation
- Use atomic counter for fast generation
- Consider LSN ranges for distributed systems

### 2. Query Performance
- Materialize on-demand, not eagerly
- Cache materialized states
- Use indexes on branch_id + collection + lsn

### 3. Storage Efficiency
- Store only deltas (document + old_document)
- Compress BSON documents
- Periodic checkpoint creation (future)

## Migration Strategy

```bash
# New branch in git
git checkout -b feature/wal-implementation

# Keep existing code intact
# Add WAL as parallel system
# Use feature flags for gradual rollout
```

## Success Metrics

1. **Branch Creation**: < 10ms (from 500ms+)
2. **Query Overhead**: < 100ms for small collections
3. **Storage Reduction**: 80%+ for 10 branches
4. **Zero Data Loss**: All operations recoverable
5. **Backward Compatible**: Existing CLI works unchanged