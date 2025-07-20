# 3-Week WAL Implementation Plan (Focused)

## Overview
Simplified plan focusing only on core WAL functionality for branch operations and time travel.

## Week 1: WAL Foundation & Basic Operations

### Day 1-2: Core WAL Structure
```go
// internal/wal/models.go
package wal

import (
    "time"
    "go.mongodb.org/mongo-driver/bson"
    "go.mongodb.org/mongo-driver/bson/primitive"
)

type WALEntry struct {
    ID          primitive.ObjectID `bson:"_id,omitempty"`
    LSN         int64             `bson:"lsn"`
    Timestamp   time.Time         `bson:"timestamp"`
    ProjectID   string            `bson:"project_id"`
    BranchID    string            `bson:"branch_id"`
    Operation   string            `bson:"operation"`
    Collection  string            `bson:"collection,omitempty"`
    DocumentID  string            `bson:"document_id,omitempty"`
    Document    bson.Raw          `bson:"document,omitempty"`
    OldDocument bson.Raw          `bson:"old_document,omitempty"`
}

// internal/wal/service.go
type Service struct {
    db         *mongo.Database
    collection *mongo.Collection
    lsnCounter *atomic.Int64
}

func NewService(db *mongo.Database) *Service {
    s := &Service{
        db:         db,
        collection: db.Collection("wal_log"),
        lsnCounter: &atomic.Int64{},
    }
    
    // Create indexes
    s.collection.Indexes().CreateMany(context.Background(), []mongo.IndexModel{
        {Keys: bson.M{"lsn": 1}, Options: options.Index().SetUnique(true)},
        {Keys: bson.M{"project_id": 1, "lsn": 1}},
        {Keys: bson.M{"branch_id": 1, "collection": 1, "lsn": 1}},
    })
    
    // Initialize LSN counter
    s.initializeLSN()
    
    return s
}

func (s *Service) Append(entry *WALEntry) (int64, error) {
    entry.LSN = s.lsnCounter.Add(1)
    entry.Timestamp = time.Now()
    
    _, err := s.collection.InsertOne(context.Background(), entry)
    return entry.LSN, err
}
```

### Day 3-4: Branch Management
```go
// internal/branch/wal_branch.go
type WALBranch struct {
    ID         string    `bson:"_id"`
    ProjectID  string    `bson:"project_id"`
    Name       string    `bson:"name"`
    ParentID   string    `bson:"parent_id,omitempty"`
    HeadLSN    int64     `bson:"head_lsn"`
    BaseLSN    int64     `bson:"base_lsn"`
    CreatedAt  time.Time `bson:"created_at"`
    CreatedLSN int64     `bson:"created_lsn"`
}

// internal/branch/wal_service.go
type WALBranchService struct {
    db         *mongo.Database
    wal        *wal.Service
    collection *mongo.Collection
}

func (s *WALBranchService) CreateBranch(projectID, name, parentID string) (*WALBranch, error) {
    // Get parent branch
    var parent *WALBranch
    if parentID != "" {
        parent = s.GetBranch(parentID)
    }
    
    // Create branch entry in WAL
    lsn, _ := s.wal.Append(&wal.WALEntry{
        ProjectID: projectID,
        BranchID:  name,
        Operation: "create_branch",
    })
    
    // Create branch record
    branch := &WALBranch{
        ID:         primitive.NewObjectID().Hex(),
        ProjectID:  projectID,
        Name:       name,
        ParentID:   parentID,
        HeadLSN:    parent.HeadLSN, // Inherit parent's HEAD
        BaseLSN:    parent.HeadLSN, // Fork point
        CreatedAt:  time.Now(),
        CreatedLSN: lsn,
    }
    
    _, err := s.collection.InsertOne(context.Background(), branch)
    return branch, err
}

func (s *WALBranchService) DeleteBranch(branchID string) error {
    branch := s.GetBranch(branchID)
    
    // Validation
    if branch.Name == "main" {
        return errors.New("cannot delete main branch")
    }
    
    // Just delete the branch record
    // Keep WAL entries for simplicity (can clean up manually later)
    _, err := s.collection.DeleteOne(context.Background(), bson.M{"_id": branchID})
    return err
}
```

### Day 5: Project Operations
```go
// internal/project/wal_service.go
func (s *ProjectService) CreateProject(name string) (*Project, error) {
    projectID := primitive.NewObjectID().Hex()
    
    // Create project in WAL
    lsn, _ := s.wal.Append(&wal.WALEntry{
        ProjectID: projectID,
        Operation: "create_project",
    })
    
    // Create main branch
    mainBranch := &WALBranch{
        ID:         "main",
        ProjectID:  projectID,
        Name:       "main",
        HeadLSN:    lsn,
        BaseLSN:    0,
        CreatedLSN: lsn,
    }
    
    // Save project and branch
    project := &Project{
        ID:           projectID,
        Name:         name,
        MainBranchID: "main",
        CreatedAt:    time.Now(),
    }
    
    return project, nil
}
```

## Week 2: Data Operations & Basic Materialization

### Day 1-2: Write Operations (Insert/Update/Delete)
```go
// internal/driver/wal_interceptor.go
type Interceptor struct {
    wal      *wal.Service
    branch   *WALBranch
}

func (i *Interceptor) InsertOne(collection string, document interface{}) error {
    // Convert to BSON
    docBytes, _ := bson.Marshal(document)
    
    // Extract/Generate ID
    docID := getDocumentID(document)
    
    // Append to WAL
    lsn, err := i.wal.Append(&wal.WALEntry{
        ProjectID:  i.branch.ProjectID,
        BranchID:   i.branch.ID,
        Operation:  "insert",
        Collection: collection,
        DocumentID: docID,
        Document:   docBytes,
    })
    
    // Update branch HEAD
    i.branch.HeadLSN = lsn
    
    return err
}

func (i *Interceptor) UpdateOne(collection string, filter, update interface{}) error {
    // First, find the document (simplified - in reality would use materializer)
    doc := i.findOne(collection, filter)
    if doc == nil {
        return nil
    }
    
    oldDocBytes, _ := bson.Marshal(doc)
    
    // Apply update
    newDoc := applyUpdate(doc, update)
    newDocBytes, _ := bson.Marshal(newDoc)
    
    // Append to WAL
    lsn, err := i.wal.Append(&wal.WALEntry{
        ProjectID:   i.branch.ProjectID,
        BranchID:    i.branch.ID,
        Operation:   "update",
        Collection:  collection,
        DocumentID:  getDocumentID(doc),
        Document:    newDocBytes,
        OldDocument: oldDocBytes,
    })
    
    i.branch.HeadLSN = lsn
    return err
}

func (i *Interceptor) DeleteOne(collection string, filter interface{}) error {
    doc := i.findOne(collection, filter)
    if doc == nil {
        return nil
    }
    
    // Append delete to WAL
    lsn, err := i.wal.Append(&wal.WALEntry{
        ProjectID:  i.branch.ProjectID,
        BranchID:   i.branch.ID,
        Operation:  "delete",
        Collection: collection,
        DocumentID: getDocumentID(doc),
    })
    
    i.branch.HeadLSN = lsn
    return err
}
```

### Day 3-4: Basic Materializer
```go
// internal/materializer/simple.go
type SimpleMaterializer struct {
    wal *wal.Service
}

func (m *SimpleMaterializer) Materialize(branchID, collection string, upToLSN int64) (map[string]bson.M, error) {
    // Get all WAL entries for this collection up to LSN
    cursor, err := m.wal.collection.Find(context.Background(), bson.M{
        "branch_id":  branchID,
        "collection": collection,
        "lsn":        bson.M{"$lte": upToLSN},
    }, options.Find().SetSort(bson.M{"lsn": 1}))
    
    if err != nil {
        return nil, err
    }
    defer cursor.Close(context.Background())
    
    // Build state
    state := make(map[string]bson.M)
    
    for cursor.Next(context.Background()) {
        var entry wal.WALEntry
        cursor.Decode(&entry)
        
        switch entry.Operation {
        case "insert":
            var doc bson.M
            bson.Unmarshal(entry.Document, &doc)
            state[entry.DocumentID] = doc
            
        case "update":
            var doc bson.M
            bson.Unmarshal(entry.Document, &doc)
            state[entry.DocumentID] = doc
            
        case "delete":
            delete(state, entry.DocumentID)
        }
    }
    
    return state, nil
}

func (m *SimpleMaterializer) Find(branchID, collection string, filter bson.M, upToLSN int64) ([]bson.M, error) {
    // Materialize collection
    state, err := m.Materialize(branchID, collection, upToLSN)
    if err != nil {
        return nil, err
    }
    
    // Apply filter
    results := []bson.M{}
    for _, doc := range state {
        if matchesFilter(doc, filter) {
            results = append(results, doc)
        }
    }
    
    return results, nil
}
```

### Day 5: Query Interface
```go
// internal/driver/wal_database.go
type WALDatabase struct {
    branch       *WALBranch
    wal          *wal.Service
    materializer *SimpleMaterializer
}

func (d *WALDatabase) Collection(name string) *WALCollection {
    return &WALCollection{
        name:         name,
        branch:       d.branch,
        wal:          d.wal,
        materializer: d.materializer,
    }
}

// internal/driver/wal_collection.go
type WALCollection struct {
    name         string
    branch       *WALBranch
    wal          *wal.Service
    materializer *SimpleMaterializer
}

func (c *WALCollection) Find(filter bson.M) ([]bson.M, error) {
    return c.materializer.Find(c.branch.ID, c.name, filter, c.branch.HeadLSN)
}

func (c *WALCollection) InsertOne(document interface{}) error {
    interceptor := &Interceptor{wal: c.wal, branch: c.branch}
    return interceptor.InsertOne(c.name, document)
}
```

## Week 3: Time Travel & CLI Integration

### Day 1-2: Time Travel Implementation
```go
// internal/timetravel/service.go
type TimeTravelService struct {
    wal      *wal.Service
    branches *WALBranchService
}

func (t *TimeTravelService) RestoreToTime(branchID string, targetTime time.Time) (*WALBranch, error) {
    // Find LSN at target time
    var entry wal.WALEntry
    err := t.wal.collection.FindOne(context.Background(), bson.M{
        "branch_id": branchID,
        "timestamp": bson.M{"$lte": targetTime},
    }, options.FindOne().SetSort(bson.M{"lsn": -1})).Decode(&entry)
    
    if err != nil {
        return nil, err
    }
    
    targetLSN := entry.LSN
    
    // Create new branch at that LSN
    originalBranch := t.branches.GetBranch(branchID)
    restoredBranch := &WALBranch{
        ID:         primitive.NewObjectID().Hex(),
        ProjectID:  originalBranch.ProjectID,
        Name:       fmt.Sprintf("%s-restore-%d", originalBranch.Name, targetLSN),
        ParentID:   branchID,
        HeadLSN:    targetLSN,
        BaseLSN:    targetLSN,
        CreatedAt:  time.Now(),
    }
    
    // Record in WAL
    t.wal.Append(&wal.WALEntry{
        ProjectID: originalBranch.ProjectID,
        BranchID:  restoredBranch.ID,
        Operation: "restore_branch",
    })
    
    return restoredBranch, t.branches.SaveBranch(restoredBranch)
}

func (t *TimeTravelService) RestoreToLSN(branchID string, targetLSN int64) (*WALBranch, error) {
    branch := t.branches.GetBranch(branchID)
    
    // Option 1: Create new branch
    restoredBranch := &WALBranch{
        ID:        primitive.NewObjectID().Hex(),
        ProjectID: branch.ProjectID,
        Name:      fmt.Sprintf("%s-lsn-%d", branch.Name, targetLSN),
        ParentID:  branchID,
        HeadLSN:   targetLSN,
        BaseLSN:   targetLSN,
    }
    
    return restoredBranch, t.branches.SaveBranch(restoredBranch)
}
```

### Day 3-4: CLI Integration
```go
// cli/cmd/branch_wal.go
func newWALBranchCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "branch",
        Short: "Manage WAL-based branches",
    }
    
    // Create branch
    createCmd := &cobra.Command{
        Use:   "create [name]",
        Short: "Create a new WAL branch",
        RunE: func(cmd *cobra.Command, args []string) error {
            client := getWALClient()
            branch, err := client.CreateBranch(projectID, args[0], parentBranch)
            if err != nil {
                return err
            }
            
            fmt.Printf("Created WAL branch '%s' (LSN: %d)\n", branch.Name, branch.HeadLSN)
            return nil
        },
    }
    
    // Time travel
    restoreCmd := &cobra.Command{
        Use:   "restore [branch]",
        Short: "Restore branch to point in time",
        RunE: func(cmd *cobra.Command, args []string) error {
            client := getWALClient()
            
            if timestamp != "" {
                targetTime, _ := time.Parse(time.RFC3339, timestamp)
                branch, err := client.RestoreToTime(args[0], targetTime)
                if err != nil {
                    return err
                }
                fmt.Printf("Restored to new branch '%s' at %s\n", branch.Name, timestamp)
            } else if lsn > 0 {
                branch, err := client.RestoreToLSN(args[0], lsn)
                if err != nil {
                    return err
                }
                fmt.Printf("Restored to new branch '%s' at LSN %d\n", branch.Name, lsn)
            }
            
            return nil
        },
    }
    
    cmd.AddCommand(createCmd, restoreCmd)
    return cmd
}
```

### Day 5: Testing & Integration
```go
// tests/wal_integration_test.go
func TestWALBranchOperations(t *testing.T) {
    // Setup
    walService := setupWALService(t)
    branchService := setupBranchService(t, walService)
    
    // Test 1: Create project and branch
    project, _ := branchService.CreateProject("test-project")
    branch, _ := branchService.CreateBranch(project.ID, "feature-1", "main")
    
    assert.Equal(t, branch.BaseLSN, branch.HeadLSN)
    
    // Test 2: Write data
    db := NewWALDatabase(branch, walService)
    coll := db.Collection("users")
    
    err := coll.InsertOne(bson.M{"name": "Alice", "age": 30})
    assert.NoError(t, err)
    
    // Test 3: Read data
    results, _ := coll.Find(bson.M{"name": "Alice"})
    assert.Len(t, results, 1)
    
    // Test 4: Time travel
    timeTravelService := NewTimeTravelService(walService, branchService)
    restoredBranch, _ := timeTravelService.RestoreToLSN(branch.ID, branch.HeadLSN-1)
    
    // Verify restored branch has no data
    restoredDB := NewWALDatabase(restoredBranch, walService)
    results, _ = restoredDB.Collection("users").Find(bson.M{})
    assert.Len(t, results, 0)
}

func TestWALBranchDeletion(t *testing.T) {
    // Test soft delete with retention
    branch, _ := branchService.CreateBranch(project.ID, "temp-feature", "main")
    
    // Add some data
    db := NewWALDatabase(branch, walService)
    db.Collection("test").InsertOne(bson.M{"data": "test"})
    
    // Delete branch
    err := branchService.DeleteBranch(branch.ID)
    assert.NoError(t, err)
    
    // Branch should be marked as pending delete
    deletedBranch := branchService.GetBranch(branch.ID)
    assert.Equal(t, BranchPendingDelete, deletedBranch.Status)
    assert.NotNil(t, deletedBranch.DeletedAt)
    
    // WAL entries should still exist
    entries := walService.GetBranchEntries(branch.ID)
    assert.NotEmpty(t, entries)
}

// Garbage Collection Service (Add to Week 3 or as future enhancement)
func setupGCService(walService *WALService, branchService *BranchService) *GCService {
    return &GCService{
        wal:      walService,
        branches: branchService,
        config: GCConfig{
            RetentionPeriod: 7 * 24 * time.Hour,
            RunInterval:     1 * time.Hour,
            BatchSize:       1000,
        },
    }
}
```

## Implementation Strategy

### Git Branch Setup
```bash
# Create new branch for WAL implementation
git checkout -b feature/wal-core

# Directory structure
argon/
├── internal/
│   ├── wal/           # WAL core implementation
│   ├── branch/        # Branch management
│   ├── materializer/  # Query materialization
│   ├── driver/        # MongoDB driver wrapper
│   └── timetravel/    # Time travel features
└── cli/
    └── cmd/
        └── wal/       # WAL-specific CLI commands
```

### Feature Flags
```yaml
# config/features.yaml
features:
  wal_enabled: false          # Master switch
  wal_create_branch: false    # Use WAL for new branches
  wal_data_operations: false  # Use WAL for data ops
  wal_time_travel: false      # Enable time travel
```

### Parallel Testing
```bash
# Run existing tests (should still pass)
go test ./...

# Run WAL-specific tests
go test ./internal/wal/... -tags=wal
go test ./tests/wal/... -tags=wal
```

## Success Criteria

1. **Core Features Working**
   - [x] Create/Delete projects with WAL
   - [x] Create/Delete branches instantly
   - [x] Insert/Update/Delete through WAL
   - [x] Basic queries on WAL data
   - [x] Time travel to any point

2. **Performance Targets**
   - Branch creation: < 10ms
   - Simple queries: < 100ms
   - Write operations: < 50ms

3. **No Breaking Changes**
   - Existing CLI works unchanged
   - Traditional branches still supported
   - Gradual migration path

## Post 3-Week Enhancements

1. **Performance** (Week 4)
   - Add caching layer
   - Optimize materialization
   - Batch WAL writes

2. **Advanced Queries** (Week 5)
   - Complex filters
   - Aggregation support
   - Index simulation

3. **Production Hardening** (Week 6+)
   - Checkpoint creation
   - WAL compaction
   - Distributed WAL