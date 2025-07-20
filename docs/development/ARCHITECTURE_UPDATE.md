# Argon Architecture Update: WAL-Based Branching

## Overview
This document describes the architectural evolution of Argon to support WAL-based branching alongside the existing collection-prefix approach.

## Architecture Decision

### Cloud Console (Demo/SaaS)
- **Keep Current Approach**: Collection prefixing with data copying
- **Rationale**: Limited budget, mainly for demos, simpler to maintain
- **Storage**: Accept higher storage cost for simplicity

### Open-Source Engine
- **Implement WAL**: Write-Ahead Log based branching
- **Rationale**: Production use cases need efficiency
- **Timeline**: 5-week MVP implementation

## Technical Approach

### 1. Parallel Systems
```
User Request
     ↓
Branch Check
     ↓
┌────────────────┐      ┌────────────────┐
│  UseWAL=true?  │─Yes→ │   WAL System   │
└────────────────┘      └────────────────┘
         │No                     
         ↓                      
┌────────────────┐
│ Prefix System  │
└────────────────┘
```

### 2. Feature Flags
```go
// Environment variables
ENABLE_WAL_BRANCHES=true
WAL_NEW_BRANCHES_ONLY=true

// Per-branch control
branch.UseWAL = true  // This branch uses WAL
```

### 3. Gradual Migration
- Phase 1: New branches use WAL (opt-in)
- Phase 2: New branches default to WAL
- Phase 3: Migration tool for existing branches
- Phase 4: Deprecate prefix-based system

## Implementation Strategy

### Week 1-2: Foundation
- WAL service implementation
- Dual-mode branch support
- Feature flag system

### Week 3: Driver Integration
- MongoDB driver wrapper
- Operation interception
- Query materialization

### Week 4: Performance
- Simple caching layer
- Batch operations
- Performance validation

### Week 5: Testing & Rollout
- Comprehensive testing
- Migration tools
- Documentation

## Key Design Decisions

### 1. Backward Compatibility
- All existing branches continue working
- No forced migration
- CLI commands unchanged
- API compatibility maintained

### 2. Simplifications for MVP
- No snapshots (replay full WAL)
- In-memory cache only
- Basic query filters
- No compression
- Single WAL collection

### 3. Performance Targets
- Branch creation: <50ms (vs 500ms+ current)
- Query overhead: <200ms (acceptable for dev workflows)
- Storage reduction: 80%+ for multi-branch scenarios

## Benefits

### For Users
- **Instant Branches**: Create branches in milliseconds
- **Storage Efficient**: 80% less storage for multiple branches
- **Time Travel**: Restore to any point (future feature)
- **No Breaking Changes**: Existing workflows continue

### For Maintainers
- **Gradual Migration**: No big-bang deployment
- **Proven Architecture**: Similar to Neon/Postgres
- **Future Features**: Enables advanced capabilities
- **Cost Reduction**: Lower storage costs

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Performance regression | High | Feature flags for instant rollback |
| Migration failures | High | Keep both systems, gradual migration |
| Complexity increase | Medium | Clear separation, good docs |
| Bug in materialization | High | Comprehensive test suite |

## Future Roadmap

### Phase 1 (Weeks 1-5): MVP
- Basic WAL functionality
- New branches only
- Simple operations

### Phase 2 (Weeks 6-10): Optimization
- Snapshot support
- Redis caching
- Query optimization
- Compression

### Phase 3 (Weeks 11-15): Advanced Features
- Time travel
- Branch comparison
- Audit trails
- Performance monitoring

### Phase 4 (Weeks 16-20): Production Hardening
- Sharding support
- Distributed WAL
- Advanced caching
- Operational tools

## Comparison: Cloud vs Open-Source

| Feature | Cloud Console | Open-Source |
|---------|--------------|-------------|
| Branching | Prefix + Copy | WAL-based |
| Storage | High (N × data) | Low (1 × data + WAL) |
| Branch Creation | Seconds | Milliseconds |
| Complexity | Simple | Complex |
| History | None | Full (future) |
| Target Users | Demo/Trial | Production |

## Migration Guide

### For New Users
1. Install latest Argon
2. Set `ENABLE_WAL_BRANCHES=true`
3. Create branches normally - they'll use WAL

### For Existing Users
1. Update Argon
2. Existing branches continue working
3. New branches can use WAL with `--use-wal` flag
4. Migrate branches individually when ready

### For Contributors
1. WAL code in `internal/wal/`
2. Feature flags in `internal/config/`
3. Tests cover both systems
4. Benchmarks compare performance

## Success Criteria

- [ ] 5-week MVP delivers core functionality
- [ ] No breaking changes for existing users
- [ ] 100x faster branch creation
- [ ] 80% storage reduction
- [ ] All tests pass with both systems
- [ ] Clear migration path

## Conclusion

This dual-system approach allows us to:
1. Keep cloud console simple and budget-friendly
2. Give open-source users production-ready efficiency
3. Migrate gradually without disruption
4. Enable future advanced features

The 5-week timeline is aggressive but achievable by focusing on MVP functionality and deferring optimization.