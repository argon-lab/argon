# WAL MVP Simplifications

## Key Decision: Simple > Perfect

For the 3-week MVP, we're choosing simplicity over perfection. Here's what we're simplifying and why.

## 1. Branch Deletion: Just Delete the Pointer

### Original Plan (Complex)
- Soft delete with retention period
- Background garbage collection service  
- Reference counting between branches
- Compaction strategies

### MVP Plan (Simple)
```go
func DeleteBranch(branchID string) error {
    // Validate it's not main branch
    // Delete the branch record
    // That's it!
}
```

### Why This Works
- **Branches are just pointers** - Deleting the pointer is instant
- **WAL is append-only** - Designed to grow anyway
- **Storage is cheap** - 1GB holds ~10M operations
- **Can add cleanup later** - Not critical for MVP

## 2. No Background Services

### Cut from MVP
- ❌ Garbage collection service
- ❌ WAL compaction service
- ❌ Automatic cleanup
- ❌ Reference counting

### Focus on Core
- ✅ WAL append operations
- ✅ Branch as LSN pointers
- ✅ Materialization on read
- ✅ Time travel

## 3. Simple Storage Model

### What We're NOT Doing
- Complex dependency tracking
- Shared data optimization
- Incremental snapshots
- Compression

### What We ARE Doing
- Every operation goes to WAL
- Branches point to LSN ranges
- Replay WAL to build state
- Manual cleanup if needed

## 4. Timeline Impact

### Original 5-Week Plan
- Week 1: WAL Core
- Week 2: Driver Integration
- Week 3: Query Engine
- Week 4: Performance
- Week 5: Testing
- Week 6+: GC System

### New 3-Week Plan
- Week 1: WAL Core + Branch Ops ✅
- Week 2: Data Ops + Materialization ✅
- Week 3: Time Travel + CLI ✅
- Done! Ship it!

## 5. Future Enhancements

### Phase 1: Ship MVP (Weeks 1-3)
- Core WAL functionality
- Branch operations
- Basic queries
- Time travel

### Phase 2: Optimize (Month 2)
- Add cleanup command
- Performance tuning
- Caching layer

### Phase 3: Scale (Month 3+)
- Automatic GC
- Compaction
- Snapshots

## 6. Real Numbers

### Storage Growth Estimates
- 1 operation = ~100 bytes
- 10K ops/day = 1MB/day
- 30 days = 30MB
- 1 year = 365MB

**Conclusion**: Even without cleanup, storage growth is manageable for most use cases.

## 7. Success Metrics (Adjusted)

### Must Have (Week 3)
- [x] Branch creation < 50ms
- [x] Query works correctly
- [x] Time travel works
- [x] No data loss

### Nice to Have (Later)
- [ ] Automatic cleanup
- [ ] Storage optimization
- [ ] Complex queries
- [ ] Performance caching

## The Bottom Line

**Ship a working WAL-based system in 3 weeks by:**
1. Keeping branch deletion simple
2. Skipping background services
3. Adding cleanup later if needed
4. Focusing on core functionality

**This approach:**
- ✅ Reduces implementation risk
- ✅ Simplifies testing
- ✅ Gets to market faster
- ✅ Proves the concept
- ✅ Can be enhanced later

**Remember**: You can always add complexity, but you can't remove it. Start simple, iterate based on real usage.