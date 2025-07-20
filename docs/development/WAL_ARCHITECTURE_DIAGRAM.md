# WAL Architecture Diagrams

## Current vs Future Architecture

### Current: Collection Prefix Approach
```
┌─────────────────────────────────────────────────────────┐
│                     MongoDB                              │
├─────────────────────────┬───────────────────────────────┤
│ main_users              │ feature_x_users               │
│ ┌─────────────────┐     │ ┌─────────────────┐          │
│ │ {id: 1, ...}    │     │ │ {id: 1, ...}    │ COPY     │
│ │ {id: 2, ...}    │     │ │ {id: 2, ...}    │          │
│ │ {id: 3, ...}    │     │ │ {id: 3, ...}    │          │
│ └─────────────────┘     │ │ {id: 4, ...}    │ NEW      │
│                         │ └─────────────────┘          │
├─────────────────────────┼───────────────────────────────┤
│ main_products           │ feature_x_products            │
│ ┌─────────────────┐     │ ┌─────────────────┐          │
│ │ Full Copy       │     │ │ Full Copy       │          │
│ └─────────────────┘     │ └─────────────────┘          │
└─────────────────────────┴───────────────────────────────┘

Problems:
- Branch creation copies ALL data (slow, expensive)
- Storage = N branches × Data size
- No history or time travel
```

### New: WAL-Based Approach
```
┌─────────────────────────────────────────────────────────┐
│                    WAL System                            │
├─────────────────────────────────────────────────────────┤
│                    wal_log                               │
│ ┌─────────────────────────────────────────────────┐     │
│ │ LSN | Timestamp | Branch | Op | Collection | Doc │     │
│ ├─────┼───────────┼────────┼────┼────────────┼─────┤     │
│ │ 1   | 10:00:00  | main   | ins| users      | ... │     │
│ │ 2   | 10:00:01  | main   | ins| users      | ... │     │
│ │ 3   | 10:00:02  | main   | ins| products   | ... │     │
│ │ 4   | 10:00:03  | feat-x | ins| users      | ... │     │
│ │ 5   | 10:00:04  | feat-x | upd| users      | ... │     │
│ └─────────────────────────────────────────────────┘     │
├─────────────────────────────────────────────────────────┤
│                  wal_branches                            │
│ ┌─────────────────────────────────────────────────┐     │
│ │ Branch    | HeadLSN | BaseLSN | Parent         │     │
│ ├───────────┼─────────┼─────────┼────────────────┤     │
│ │ main      | 3       | 0       | -              │     │
│ │ feature-x | 5       | 3       | main           │     │
│ └─────────────────────────────────────────────────┘     │
└─────────────────────────────────────────────────────────┘

Benefits:
- Branches are just pointers (instant creation)
- Storage = 1x data + small WAL overhead
- Complete history and time travel
```

## Data Flow Comparison

### Write Operation

**Current Flow:**
```
User writes to feature branch
         │
         ▼
Write to feature_x_users collection
         │
         ▼
Data stored separately
```

**New WAL Flow:**
```
User writes to feature branch
         │
         ▼
Append to WAL with branch_id
         │
         ▼
Update branch HEAD pointer
         │
         ▼
Single source of truth
```

### Read Operation

**Current Flow:**
```
User queries feature branch
         │
         ▼
Read from feature_x_users directly
         │
         ▼
Return results
```

**New WAL Flow:**
```
User queries feature branch
         │
         ▼
Get branch HEAD LSN
         │
         ▼
Materialize state from WAL
         │
         ▼
Apply query filters
         │
         ▼
Return results
```

## Branch Operations Comparison

### Create Branch

**Current (Slow):**
```
1. List all collections in parent    [100ms]
2. For each collection:
   - Read all documents              [Varies by size]
   - Create new collection           [50ms]
   - Insert all documents            [Varies by size]
3. Update branch metadata            [10ms]

Total: 500ms - several minutes
```

**New WAL (Fast):**
```
1. Get parent branch HEAD LSN        [1ms]
2. Create branch record              [5ms]
3. Done!                            

Total: < 10ms (constant time)
```

### Time Travel

**Current:**
```
NOT POSSIBLE - No history kept
```

**New WAL:**
```
1. Find target LSN from timestamp    [5ms]
2. Create new branch at that LSN     [5ms]
3. Queries automatically see old state

Total: < 10ms to ANY point in history
```

## Storage Comparison

### 10 Branches Scenario

**Current:**
```
Base data: 1GB
10 branches × 1GB = 10GB total storage

With changes:
- Each branch stores full copy
- Small change = full collection copy
- No deduplication
```

**New WAL:**
```
Base data: 1GB (materialized)
WAL log: 0.3GB (changes only)
Total: 1.3GB for ALL branches

With changes:
- Only deltas stored
- Shared history
- Natural deduplication
```

## Implementation Phases

```
Week 1: Foundation
┌────────────┐     ┌────────────┐     ┌────────────┐
│ WAL Core   │ --> │  Branches  │ --> │  Projects  │
│ (LSN, Log) │     │ (Pointers) │     │ (Metadata) │
└────────────┘     └────────────┘     └────────────┘

Week 2: Operations  
┌────────────┐     ┌────────────┐     ┌────────────┐
│   Write    │ --> │ Materialize│ --> │   Query    │
│ (Intercept)│     │  (Replay)  │     │  (Filter)  │
└────────────┘     └────────────┘     └────────────┘

Week 3: Features
┌────────────┐     ┌────────────┐     ┌────────────┐
│Time Travel │ --> │    CLI     │ --> │   Tests    │
│ (Restore)  │     │ (Commands) │     │ (Validate) │
└────────────┘     └────────────┘     └────────────┘
```