# WAL Branch LSN Design

## Problem Statement
With a global LSN counter, how do we determine which WAL entries belong to which branch?

## Current Design Issue
```
WAL Log:
LSN | Branch    | Operation | Document
----|-----------|-----------|----------
1   | -         | create_project | -
2   | main      | create_branch | -
3   | main      | insert    | {id: 1}
4   | feature-x | create_branch | -
5   | feature-x | insert    | {id: 2}
6   | main      | insert    | {id: 3}
```

Question: When reading feature-x, which entries do we include?

## Solution: Branch Lineage + LSN Range

### Core Concept
Each branch tracks:
1. **BaseLSN**: Where it forked from parent
2. **HeadLSN**: Latest operation on this branch
3. **Branch Lineage**: Include parent branch history up to fork point

### Implementation

```go
// When materializing branch state:
func (m *Materializer) GetBranchState(branch *Branch) State {
    entries := []Entry{}
    
    // 1. Get parent branch entries up to fork point
    if branch.ParentID != "" {
        parentEntries := m.wal.GetEntries(filter{
            "branch_id": branch.ParentID,
            "lsn": {"$lte": branch.BaseLSN},
        })
        entries = append(entries, parentEntries...)
    }
    
    // 2. Get this branch's entries
    branchEntries := m.wal.GetEntries(filter{
        "branch_id": branch.ID,
        "lsn": {"$gt": branch.BaseLSN},
    })
    entries = append(entries, branchEntries...)
    
    // 3. Apply all entries in LSN order
    return buildState(entries)
}
```

### Example Walkthrough

```
Initial State:
main (BaseLSN: 0, HeadLSN: 6)
feature-x (BaseLSN: 3, HeadLSN: 5, Parent: main)

Query feature-x:
1. Get main's entries where LSN <= 3:
   - LSN 1: create_project
   - LSN 2: create_branch (main)
   - LSN 3: insert {id: 1}

2. Get feature-x's entries where LSN > 3:
   - LSN 4: create_branch (feature-x)
   - LSN 5: insert {id: 2}

Result: feature-x sees {id: 1} and {id: 2}
        main sees {id: 1} and {id: 3}
```

## Key Design Decisions

### 1. Global LSN is Correct
- Provides total ordering across system
- Simplifies time travel
- Makes debugging easier
- Standard approach (like Postgres)

### 2. Branch Isolation via Filtering
- Each entry tagged with branch_id
- Materialization filters by branch + lineage
- No LSN conflicts between branches

### 3. HeadLSN Tracking
```go
// Update branch HEAD after each operation
func (w *WALService) AppendForBranch(branch *Branch, entry *Entry) error {
    entry.BranchID = branch.ID
    lsn, err := w.Append(entry)
    if err != nil {
        return err
    }
    
    // Update branch HEAD
    branch.HeadLSN = lsn
    return w.branches.UpdateBranchHead(branch.ID, lsn)
}
```

## Alternative Designs Considered

### 1. Per-Branch LSN (Rejected)
```
main:     1 -> 2 -> 3
feature:  1 -> 2 -> 3
```
Problems:
- No global ordering
- Complex merging
- Difficult time travel

### 2. Composite LSN (Rejected)
```
main:     main-1, main-2, main-3
feature:  feature-1, feature-2
```
Problems:
- Complex comparisons
- Storage overhead
- Non-standard

### 3. Vector Clocks (Rejected)
```
main:     {main: 3, feature: 0}
feature:  {main: 3, feature: 2}
```
Problems:
- Too complex for MVP
- Overkill for our use case

## Benefits of Chosen Approach

1. **Simple**: Single incrementing counter
2. **Efficient**: Easy to query ranges
3. **Standard**: Similar to Postgres/MySQL
4. **Debuggable**: Clear event order
5. **Time Travel**: Easy to restore to any LSN

## Implementation Updates Needed

### 1. Update Materializer (Week 2)
```go
func (m *Materializer) MaterializeBranch(
    branchID string, 
    upToLSN int64,
) (State, error) {
    branch := m.branches.GetBranch(branchID)
    
    // Get full branch lineage
    lineage := m.getBranchLineage(branch)
    
    // Collect entries from all ancestors
    var allEntries []*Entry
    for _, ancestorBranch := range lineage {
        entries := m.getEntriesForBranch(
            ancestorBranch, 
            upToLSN,
        )
        allEntries = append(allEntries, entries...)
    }
    
    // Sort by LSN and apply
    sort.Slice(allEntries, func(i, j int) bool {
        return allEntries[i].LSN < allEntries[j].LSN
    })
    
    return m.applyEntries(allEntries), nil
}
```

### 2. Branch Lineage Helper
```go
func (m *Materializer) getBranchLineage(
    branch *Branch,
) []*Branch {
    lineage := []*Branch{}
    current := branch
    
    for current != nil {
        lineage = append([]*Branch{current}, lineage...)
        if current.ParentID == "" {
            break
        }
        current = m.branches.GetBranchByID(current.ParentID)
    }
    
    return lineage
}
```

## Summary

The global LSN approach is correct. We just need to:
1. Filter by branch_id when reading
2. Include parent history up to fork point
3. Track HeadLSN per branch
4. Apply entries in LSN order

This gives us:
- ✅ Branch isolation
- ✅ Simple implementation
- ✅ Efficient queries
- ✅ Time travel capability
- ✅ Standard approach