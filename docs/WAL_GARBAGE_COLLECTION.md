# WAL Garbage Collection Design

## Overview
Based on Neon's approach, we need a sophisticated garbage collection system that balances data retention with storage efficiency.

## Key Concepts

### 1. Retention Window
- Configure a retention period (e.g., 7 days)
- Keep all WAL entries within this window
- Allow point-in-time recovery within the window

### 2. Reference Counting
- Track which branches reference which WAL entries
- Only GC entries that have no active references
- Consider branch dependencies (parent-child relationships)

## Updated Delete Branch Flow

```
User: argon branch delete "feature-x"
         |
         v
┌─────────────────────┐
│  Mark Branch as     │
│  Pending Deletion   │
│  deleted_at = now() │
└──────────┬──────────┘
           |
           v
┌─────────────────────┐
│  Check Active       │
│  Child Branches     │
└──────────┬──────────┘
           |
           ├─── Has Children ───┐
           |                    |
           v                    v
┌─────────────────────┐  ┌─────────────────────┐
│  Keep WAL Entries   │  │  Schedule for GC    │
│  Referenced by      │  │  After Retention    │
│  Children           │  │  Period             │
└─────────────────────┘  └──────────┬──────────┘
                                     |
                                     v
                         ┌─────────────────────┐
                         │  Return Success     │
                         │  Branch "Deleted"    │
                         └─────────────────────┘
```

## Garbage Collection Process

### 1. Branch Deletion States
```go
type BranchStatus string
const (
    BranchActive         BranchStatus = "active"
    BranchPendingDelete  BranchStatus = "pending_delete"
    BranchDeleted        BranchStatus = "deleted"
)

type WALBranch struct {
    ID          string       `bson:"_id"`
    ProjectID   string       `bson:"project_id"`
    Name        string       `bson:"name"`
    Status      BranchStatus `bson:"status"`
    DeletedAt   *time.Time   `bson:"deleted_at,omitempty"`
    HeadLSN     int64        `bson:"head_lsn"`
    BaseLSN     int64        `bson:"base_lsn"`
    ParentID    string       `bson:"parent_id,omitempty"`
}
```

### 2. Immediate Deletion (Soft Delete)
```go
func (s *BranchService) DeleteBranch(branchID string) error {
    branch := s.GetBranch(branchID)
    
    // Check if branch has active children
    children := s.GetChildBranches(branchID)
    if len(children) > 0 {
        return fmt.Errorf("cannot delete branch with active children")
    }
    
    // Mark branch as pending deletion
    now := time.Now()
    branch.Status = BranchPendingDelete
    branch.DeletedAt = &now
    
    // Update in database
    s.collection.UpdateOne(
        bson.M{"_id": branchID},
        bson.M{"$set": bson.M{
            "status": BranchPendingDelete,
            "deleted_at": now,
        }},
    )
    
    // Schedule garbage collection
    s.scheduleGC(branchID, s.config.RetentionPeriod)
    
    return nil
}
```

### 3. Garbage Collection Service
```go
type GCService struct {
    wal          *WALService
    branches     *BranchService
    config       GCConfig
    scheduler    *cron.Cron
}

type GCConfig struct {
    RetentionPeriod   time.Duration // e.g., 7 days
    RunInterval       time.Duration // e.g., 1 hour
    BatchSize         int           // e.g., 1000 entries
    CompactionEnabled bool
}

func (gc *GCService) Start() {
    // Run GC periodically
    gc.scheduler.AddFunc("@hourly", gc.RunGC)
    gc.scheduler.Start()
}

func (gc *GCService) RunGC() error {
    // 1. Find branches ready for deletion
    cutoffTime := time.Now().Add(-gc.config.RetentionPeriod)
    
    branches, err := gc.branches.collection.Find(bson.M{
        "status": BranchPendingDelete,
        "deleted_at": bson.M{"$lt": cutoffTime},
    })
    
    for _, branch := range branches {
        if err := gc.gcBranch(branch); err != nil {
            log.Printf("GC failed for branch %s: %v", branch.ID, err)
            continue
        }
    }
    
    // 2. Clean up orphaned WAL entries
    gc.cleanOrphanedWAL()
    
    // 3. Compact WAL if enabled
    if gc.config.CompactionEnabled {
        gc.compactWAL()
    }
    
    return nil
}
```

### 4. Branch GC Implementation
```go
func (gc *GCService) gcBranch(branch *WALBranch) error {
    // 1. Get LSN range for this branch
    startLSN := branch.BaseLSN
    endLSN := branch.HeadLSN
    
    // 2. Check if any other branches reference this LSN range
    refs := gc.findLSNReferences(branch.ProjectID, startLSN, endLSN)
    if len(refs) > 0 {
        // Other branches still need these entries
        log.Printf("Branch %s LSN range %d-%d still referenced by %v", 
            branch.ID, startLSN, endLSN, refs)
        
        // Just mark branch as deleted, keep WAL entries
        return gc.branches.markDeleted(branch.ID)
    }
    
    // 3. Delete WAL entries for this branch
    result, err := gc.wal.collection.DeleteMany(bson.M{
        "branch_id": branch.ID,
        "lsn": bson.M{
            "$gte": startLSN,
            "$lte": endLSN,
        },
    })
    
    log.Printf("Deleted %d WAL entries for branch %s", 
        result.DeletedCount, branch.ID)
    
    // 4. Delete branch record
    gc.branches.collection.DeleteOne(bson.M{"_id": branch.ID})
    
    return nil
}
```

### 5. Reference Counting
```go
func (gc *GCService) findLSNReferences(projectID string, startLSN, endLSN int64) []string {
    var references []string
    
    // Find all active branches that might reference this LSN range
    branches, _ := gc.branches.collection.Find(bson.M{
        "project_id": projectID,
        "status": BranchActive,
        "$or": []bson.M{
            // Branch created in this range
            {"base_lsn": bson.M{"$gte": startLSN, "$lte": endLSN}},
            // Branch spans this range
            {
                "base_lsn": bson.M{"$lte": startLSN},
                "head_lsn": bson.M{"$gte": startLSN},
            },
        },
    })
    
    for _, branch := range branches {
        references = append(references, branch.ID)
    }
    
    return references
}
```

### 6. WAL Compaction (Optional)
```go
func (gc *GCService) compactWAL() error {
    // Find collections with many small updates
    pipeline := []bson.M{
        {"$group": bson.M{
            "_id": bson.M{
                "project_id": "$project_id",
                "collection": "$collection",
                "document_id": "$document_id",
            },
            "count": bson.M{"$sum": 1},
            "min_lsn": bson.M{"$min": "$lsn"},
            "max_lsn": bson.M{"$max": "$lsn"},
        }},
        {"$match": bson.M{"count": bson.M{"$gt": 10}}}, // Many updates
    }
    
    compactionCandidates, _ := gc.wal.collection.Aggregate(context.Background(), pipeline)
    
    for _, candidate := range compactionCandidates {
        gc.compactDocument(candidate)
    }
    
    return nil
}

func (gc *GCService) compactDocument(info bson.M) error {
    // Get all updates for this document
    entries := gc.wal.GetEntries(bson.M{
        "project_id": info["_id"].(bson.M)["project_id"],
        "collection": info["_id"].(bson.M)["collection"],
        "document_id": info["_id"].(bson.M)["document_id"],
    })
    
    // Keep only:
    // 1. First insert
    // 2. Updates at branch points
    // 3. Final state
    // Delete intermediate updates
    
    return nil
}
```

## Configuration Options

```yaml
# config.yaml
garbage_collection:
  enabled: true
  retention_period: 168h  # 7 days
  run_interval: 1h        # Run every hour
  batch_size: 1000        # Process 1000 entries at a time
  compaction:
    enabled: false        # Start simple, add later
    min_updates: 10       # Compact if >10 updates per document

branch_deletion:
  mode: "soft"            # soft (with GC) or hard (immediate)
  allow_with_children: false
  grace_period: 24h       # Keep deleted branches accessible for 24h
```

## Benefits of This Approach

1. **Data Safety**: Retention period allows recovery
2. **Storage Efficiency**: Eventually reclaims space
3. **Performance**: GC runs async, doesn't block operations
4. **Flexibility**: Configurable retention and compaction
5. **Child Branch Protection**: Won't delete data needed by children

## CLI Commands

```bash
# Delete branch (soft delete by default)
argon branch delete feature-x

# Force immediate deletion (admin only)
argon branch delete feature-x --force --no-retention

# Show pending deletions
argon branch list --deleted

# Recover deleted branch (within retention period)
argon branch recover feature-x

# Manual GC trigger (admin)
argon admin gc run

# GC statistics
argon admin gc stats
```

## Comparison with Alternatives

### 1. Neon's Approach
- Uses layers and page servers
- Complex dependency tracking
- Retention window (PiTR)
- Compaction of layers

### 2. Our Simplified Approach
- Direct WAL entries
- Simple reference counting
- Configurable retention
- Optional compaction

### 3. Trade-offs
- Simpler implementation
- Less sophisticated compaction
- Good enough for MVP
- Can enhance later