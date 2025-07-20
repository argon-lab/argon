# Simple WAL Deletion Strategy

## Overview
Instead of complex garbage collection, we can use a much simpler approach that still maintains data integrity.

## Key Insight
In a WAL-based system, branches are just pointers to LSN positions. The actual data (WAL entries) can be shared between branches.

## Simple Deletion Strategies

### Option 1: Delete Branch Pointer Only (Simplest)
```go
func (s *BranchService) DeleteBranch(branchID string) error {
    // Just delete the branch record, keep all WAL entries
    _, err := s.collection.DeleteOne(bson.M{"_id": branchID})
    return err
}
```

**Pros:**
- Dead simple
- No data loss risk
- No complex logic
- Branches can be "recreated" by knowing LSN

**Cons:**
- WAL grows forever
- Need periodic manual cleanup

### Option 2: Delete Branch-Specific Entries (Recommended)
```go
func (s *BranchService) DeleteBranch(branchID string) error {
    branch := s.GetBranch(branchID)
    
    // 1. Check if branch can be deleted
    if branch.Name == "main" {
        return errors.New("cannot delete main branch")
    }
    
    // 2. Find WAL entries unique to this branch
    // These are entries where branch_id = branchID
    // AND no other branch has these in their LSN range
    
    uniqueEntries := s.wal.collection.Find(bson.M{
        "branch_id": branchID,
        "lsn": bson.M{
            "$gt": branch.BaseLSN,  // Created after branch point
        },
    })
    
    // 3. Check if any other branches depend on these entries
    otherBranches := s.collection.Find(bson.M{
        "_id": bson.M{"$ne": branchID},
        "base_lsn": bson.M{"$gte": branch.BaseLSN},
    })
    
    if len(otherBranches) == 0 {
        // Safe to delete branch-specific WAL entries
        s.wal.collection.DeleteMany(bson.M{
            "branch_id": branchID,
            "lsn": bson.M{"$gt": branch.BaseLSN},
        })
    }
    
    // 4. Delete branch record
    s.collection.DeleteOne(bson.M{"_id": branchID})
    
    return nil
}
```

### Option 3: Never Delete WAL, Periodic Cleanup (Pragmatic)
```go
func (s *BranchService) DeleteBranch(branchID string) error {
    // Immediate: Just delete branch pointer
    _, err := s.collection.DeleteOne(bson.M{"_id": branchID})
    
    // WAL cleanup happens separately (cron job, manual trigger, etc.)
    return err
}

// Run periodically (daily/weekly)
func (s *WALService) CleanupOrphanedEntries(olderThan time.Duration) error {
    cutoff := time.Now().Add(-olderThan)
    
    // Find all active branches
    activeBranches := s.branches.GetAllActive()
    maxLSN := s.getMaxActiveLSN(activeBranches)
    
    // Delete WAL entries that:
    // 1. Are older than cutoff time
    // 2. Have LSN less than any active branch needs
    result, err := s.collection.DeleteMany(bson.M{
        "timestamp": bson.M{"$lt": cutoff},
        "lsn": bson.M{"$lt": maxLSN - 1000}, // Keep buffer
    })
    
    log.Printf("Cleaned up %d old WAL entries", result.DeletedCount)
    return err
}
```

## Recommended Approach for MVP

### Phase 1: Super Simple (Week 1-3)
```go
type BranchService struct {
    // ... existing fields ...
}

func (s *BranchService) DeleteBranch(branchID string) error {
    branch := s.GetBranch(branchID)
    
    // Validation
    if branch.Name == "main" {
        return errors.New("cannot delete main branch")
    }
    
    // Check children
    children := s.GetChildBranches(branchID)
    if len(children) > 0 {
        return errors.New("cannot delete branch with active children")
    }
    
    // Just delete the branch pointer
    // Keep ALL WAL entries for now
    _, err := s.collection.DeleteOne(bson.M{"_id": branchID})
    
    log.Printf("Deleted branch %s (kept WAL entries)", branchID)
    return err
}
```

### Phase 2: Add Cleanup Command (Week 4+)
```bash
# Manual cleanup when needed
argon admin wal cleanup --older-than 30d

# Show WAL statistics
argon admin wal stats
# Output:
# Total entries: 1,234,567
# Oldest entry: 2024-12-01
# Size: 1.2GB
# Orphaned entries: 234,567 (can be cleaned)
```

### Phase 3: Smart Cleanup (Future)
- Add the more complex logic
- Only if storage becomes an issue
- Can always add GC later

## Why This Works

1. **WAL is Append-Only**: Designed to grow
2. **Branches are Cheap**: Just metadata pointers
3. **Storage is Cheap**: 1GB of WAL = ~10M operations
4. **Time to Market**: Ship faster, optimize later

## Comparison

| Approach | Complexity | Safety | Storage | Recovery |
|----------|------------|--------|---------|----------|
| GC System | High | High | Optimal | Yes |
| Delete Branch Only | Low | High | Grows | No |
| Delete Unique Entries | Medium | Medium | Good | No |
| Manual Cleanup | Low | High | Good | No |

## Decision for 3-Week MVP

**Recommendation: Delete Branch Pointer Only**

```go
// Week 1-3: Ship this
func (s *BranchService) DeleteBranch(branchID string) error {
    // 1. Validate
    if branchID == "main" {
        return errors.New("cannot delete main branch")
    }
    
    // 2. Delete branch record
    _, err := s.collection.DeleteOne(bson.M{"_id": branchID})
    
    // That's it! WAL entries remain for now
    return err
}
```

**Why:**
1. Simplest possible implementation
2. No risk of data loss
3. Can add cleanup later
4. Focuses on core WAL functionality
5. Storage growth is manageable for MVP

**Future Enhancement:**
```yaml
# config.yaml (future)
wal:
  retention_days: 30
  cleanup_mode: "manual"  # or "auto"
  cleanup_schedule: "0 2 * * *"  # 2am daily
```