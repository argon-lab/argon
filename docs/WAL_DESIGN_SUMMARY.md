# WAL Design Summary

## Key Improvements Based on Neon Research

### 1. Branch Deletion: Soft Delete + Garbage Collection
Instead of keeping WAL entries forever (which would cause unbounded growth), we implement:

- **Soft Delete**: Mark branches as "pending deletion" with timestamp
- **Retention Period**: Keep deleted branches for configurable period (e.g., 7 days)
- **Garbage Collection**: Async process that runs hourly to clean up
- **Reference Counting**: Only delete WAL entries not referenced by other branches
- **Child Protection**: Cannot delete branches that have active children

### 2. Storage Efficiency
- **Immediate**: Branch marked deleted, hidden from UI
- **After Retention**: WAL entries physically deleted
- **Shared Data**: Keep entries referenced by other branches
- **Compaction**: Optional WAL compaction for frequently updated documents

### 3. Benefits Over Simple "Keep Forever"
- **Bounded Growth**: Storage doesn't grow infinitely
- **Recovery Window**: Can undelete within retention period  
- **Performance**: Old data doesn't slow down queries
- **Cost Efficient**: Reduces storage costs over time

### 4. Implementation Timeline
- **Week 1**: Core WAL + soft delete
- **Week 2**: Basic operations + materialization
- **Week 3**: Time travel + CLI
- **Week 4** (Future): Add GC service
- **Week 5** (Future): Add compaction

### 5. Configuration
```yaml
garbage_collection:
  retention_period: 168h  # 7 days
  run_interval: 1h        # Every hour
  
branch_deletion:
  mode: "soft"           # vs "hard"
  allow_with_children: false
```

### 6. CLI Experience
```bash
# Delete branch (soft, recoverable)
argon branch delete feature-x

# List deleted branches
argon branch list --deleted

# Recover within retention period
argon branch recover feature-x

# Force immediate deletion (admin)
argon branch delete feature-x --force
```

This approach balances:
- **Safety**: Retention period for recovery
- **Efficiency**: Eventually reclaims space
- **Simplicity**: Easier than Neon's layer-based approach
- **Flexibility**: Configurable policies