# Argon Architecture

## Overview

Argon is a Git-like version control system for MongoDB, designed specifically for ML/AI workflows. It provides instant branching, efficient storage, and seamless integration with ML tools.

## Table of Contents

1. [System Architecture](#system-architecture)
2. [Core Components](#core-components)
3. [Data Flow](#data-flow)
4. [Storage Architecture](#storage-architecture)
5. [Branching Mechanism](#branching-mechanism)
6. [Performance Design](#performance-design)
7. [Security Architecture](#security-architecture)
8. [Scaling Strategy](#scaling-strategy)

## System Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         Client Layer                             │
├─────────────────┬──────────────────┬────────────────────────────┤
│   CLI (Go)      │   REST API       │   SDKs                     │
│   argonctl      │   (Python)       │   Python/JS/Go            │
└────────┬────────┴────────┬─────────┴──────────┬─────────────────┘
         │                 │                     │
         └─────────────────┴─────────────────────┘
                           │
┌──────────────────────────┴──────────────────────────────────────┐
│                      Service Layer                               │
├─────────────────┬──────────────────┬────────────────────────────┤
│  Branch Engine  │  Storage Engine  │   Worker Pool              │
│     (Go)        │      (Go)        │      (Go)                  │
├─────────────────┼──────────────────┼────────────────────────────┤
│ • Branch ops    │ • Compression    │ • Change processing        │
│ • Merge logic   │ • Deduplication  │ • Background tasks         │
│ • Isolation     │ • Cloud storage  │ • Garbage collection       │
└────────┬────────┴────────┬─────────┴──────────┬─────────────────┘
         │                 │                     │
┌────────┴─────────────────┴─────────────────────┴────────────────┐
│                       Data Layer                                 │
├─────────────────────────┬───────────────────────────────────────┤
│      MongoDB            │         Object Storage                │
├─────────────────────────┼───────────────────────────────────────┤
│ • Change streams        │ • S3 / GCS / Azure                    │
│ • Metadata storage      │ • Local filesystem                    │
│ • Branch tracking       │ • Compressed objects                  │
└─────────────────────────┴───────────────────────────────────────┘
```

### Technology Stack

- **Performance Layer (Go)**
  - Branch engine
  - Storage engine
  - Worker pool
  - CLI tool

- **Productivity Layer (Python)**
  - REST API (FastAPI)
  - ML integrations
  - Web dashboard (planned)

- **Data Layer**
  - MongoDB 4.4+ (change streams)
  - Object storage (S3/GCS/Azure/Local)

## Core Components

### 1. Branch Engine (Go)

Handles all branch-related operations with sub-500ms performance target.

```go
// Key interfaces
type BranchEngine interface {
    CreateBranch(name string, from string) (*Branch, error)
    MergeBranch(source, target string, strategy MergeStrategy) error
    DeleteBranch(name string) error
    ListBranches() ([]*Branch, error)
}

type Branch struct {
    ID           string
    Name         string
    Parent       string
    CreatedAt    time.Time
    LastActivity time.Time
    Status       BranchStatus
}
```

**Responsibilities:**
- Branch creation and deletion
- Merge operations
- Conflict resolution
- Branch metadata management

### 2. Storage Engine (Go)

Manages efficient storage with copy-on-write and compression.

```go
type StorageEngine interface {
    Store(key string, data []byte) error
    Retrieve(key string) ([]byte, error)
    Delete(key string) error
    Exists(key string) bool
}

type StorageBackend interface {
    Put(ctx context.Context, key string, data []byte) error
    Get(ctx context.Context, key string) ([]byte, error)
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string) ([]string, error)
}
```

**Features:**
- ZSTD compression (42%+ savings)
- Content-addressable storage
- Deduplication
- Multi-backend support

### 3. Worker Pool (Go)

Processes MongoDB change streams and background tasks.

```go
type WorkerPool struct {
    workers    []*Worker
    jobQueue   chan Job
    resultChan chan Result
    config     WorkerConfig
}

type Job interface {
    Execute(ctx context.Context) (Result, error)
    GetPriority() int
    GetTimeout() time.Duration
}
```

**Responsibilities:**
- Change stream processing
- Asynchronous operations
- Batch processing
- Garbage collection

### 4. REST API (Python)

Provides HTTP interface for all operations.

```python
# FastAPI application structure
app = FastAPI(title="Argon API", version="1.0.0")

@app.post("/branches")
async def create_branch(branch: BranchCreate) -> BranchResponse:
    """Create a new branch from source"""
    pass

@app.get("/branches/{branch_id}/changes")
async def get_changes(
    branch_id: str, 
    limit: int = 100,
    offset: int = 0
) -> ChangesResponse:
    """Get change history for a branch"""
    pass
```

## Data Flow

### Branch Creation Flow

```
1. Client Request
   └─> API validates request
       └─> Branch Engine checks permissions
           └─> Create branch metadata in MongoDB
               └─> Initialize change stream listener
                   └─> Worker pool starts processing
                       └─> Return success to client

2. Background Processing
   └─> Worker captures changes
       └─> Storage engine compresses data
           └─> Store in object storage
               └─> Update branch statistics
```

### Data Write Flow

```
1. Application writes to MongoDB
   └─> Change stream captures operation
       └─> Worker pool receives change
           └─> Determine affected branches
               └─> Apply branch isolation rules
                   └─> Compress and store change
                       └─> Update branch metadata
```

### Data Read Flow

```
1. Application queries MongoDB
   └─> Branch context determines data visibility
       └─> Apply branch-specific filters
           └─> Merge with base data if needed
               └─> Return filtered results
```

## Storage Architecture

### Object Storage Layout

```
/argon-storage/
├── branches/
│   ├── main/
│   │   ├── metadata.json
│   │   └── collections/
│   │       ├── users/
│   │       │   ├── chunk-00001.zst
│   │       │   └── chunk-00002.zst
│   │       └── products/
│   │           └── chunk-00001.zst
│   └── feature-branch/
│       ├── metadata.json
│       └── changes/
│           ├── 2025-07-18-00001.zst
│           └── 2025-07-18-00002.zst
├── snapshots/
│   └── snap-123456/
│       └── full-backup.zst
└── temp/
    └── merge-ops/
```

### Compression Strategy

```go
type CompressionConfig struct {
    Algorithm string // "zstd", "gzip", "none"
    Level     int    // 1-9 for gzip, 1-22 for zstd
    MinSize   int    // Minimum size to compress
}

// Achieved compression ratios:
// - JSON documents: 60-80% reduction
// - Binary data: 20-40% reduction
// - Already compressed: 0-5% reduction
```

### Storage Optimization

1. **Deduplication**: Content-addressable storage with SHA-256
2. **Chunking**: Large collections split into manageable chunks
3. **Tiering**: Hot/cold data separation
4. **Caching**: LRU cache for frequently accessed objects

## Branching Mechanism

### Current Architecture (v1.0) - Collection Prefixing

Each branch maintains isolated data through collection prefixing:

```javascript
// Original collection: "users"
// Branch "feature-x": "branch_feature_x_users"

db.users.insert({name: "John"})        // Goes to main
db.branch_feature_x_users.insert(...)  // Goes to feature-x
```

### Future Architecture (v2.0) - WAL-Based Branching

We're implementing a Write-Ahead Log (WAL) architecture for the open-source version:

```javascript
// All operations go through WAL
// Branches are just pointers to LSN (Log Sequence Number)
branch: {
  name: "feature-x",
  headLSN: 12345,  // Points to position in WAL
  baseLSN: 12000   // Where branch was created
}
```

**Benefits of WAL approach:**
- Branch creation: 500ms → 10ms (50x faster)
- Storage: 10GB → 1.3GB for 10 branches (87% reduction)
- True time-travel capabilities
- Complete audit trail

[See detailed WAL implementation plan →](5_WEEK_WAL_IMPLEMENTATION_PLAN.md)

### Copy-on-Write Implementation

```go
type CopyOnWrite struct {
    baseData   map[string][]byte
    branchData map[string][]byte
    tombstones map[string]bool
}

func (c *CopyOnWrite) Read(key string) ([]byte, error) {
    // Check tombstones first
    if c.tombstones[key] {
        return nil, ErrDeleted
    }
    
    // Check branch data
    if data, ok := c.branchData[key]; ok {
        return data, nil
    }
    
    // Fall back to base data
    return c.baseData[key], nil
}
```

### Merge Strategies

1. **Fast-forward**: Direct pointer update
2. **Three-way merge**: Automatic conflict resolution
3. **Manual merge**: User-guided conflict resolution

## Performance Design

### Optimization Techniques

1. **Parallel Processing**
   ```go
   func ProcessChanges(changes []Change) {
       var wg sync.WaitGroup
       sem := make(chan struct{}, runtime.NumCPU())
       
       for _, change := range changes {
           sem <- struct{}{}
           wg.Add(1)
           go func(c Change) {
               defer wg.Done()
               defer func() { <-sem }()
               processChange(c)
           }(change)
       }
       wg.Wait()
   }
   ```

2. **Batch Operations**
   - Group small changes
   - Bulk storage operations
   - Aggregated statistics updates

3. **Caching Layers**
   - Memory cache (LRU)
   - Redis cache (optional)
   - CDN for read-heavy workloads

### Performance Metrics

| Operation | Target | Achieved |
|-----------|--------|----------|
| Branch creation | < 500ms | ~200ms |
| Small merge | < 1s | ~400ms |
| 1GB branch copy | < 10s | ~6s |
| Change capture | < 100ms | ~50ms |
| Storage write | < 200ms | ~120ms |

## Security Architecture

### Authentication & Authorization

```python
# API key authentication
@app.middleware("http")
async def authenticate(request: Request, call_next):
    api_key = request.headers.get("X-API-Key")
    if not verify_api_key(api_key):
        return JSONResponse(status_code=401, content={"error": "Unauthorized"})
    return await call_next(request)
```

### Data Encryption

1. **At Rest**: Object storage encryption (SSE-S3, etc.)
2. **In Transit**: TLS 1.3 for all connections
3. **Key Management**: AWS KMS / Azure Key Vault / HashiCorp Vault

### Access Control

```yaml
# Role-based access control
roles:
  viewer:
    - branches:read
    - changes:read
  developer:
    - branches:*
    - changes:*
    - snapshots:create
  admin:
    - "*"
```

## Scaling Strategy

### Horizontal Scaling

1. **API Servers**: Stateless, behind load balancer
2. **Workers**: Distributed processing with partition assignment
3. **Storage**: Sharded by branch ID

### Vertical Scaling

1. **MongoDB**: Larger instances for metadata
2. **Workers**: More CPU cores for compression
3. **Cache**: Increased memory for hot data

### Multi-Region Deployment

```yaml
regions:
  us-east-1:
    primary: true
    mongodb: "mongodb+srv://us-east-1.cluster.mongodb.net"
    storage: "s3://argon-us-east-1"
  eu-west-1:
    primary: false
    mongodb: "mongodb+srv://eu-west-1.cluster.mongodb.net"
    storage: "s3://argon-eu-west-1"
    
replication:
  mode: "async"
  lag_threshold: "5m"
```

### Capacity Planning

| Component | Small | Medium | Large |
|-----------|-------|--------|-------|
| API Servers | 2 × 2CPU/4GB | 4 × 4CPU/8GB | 8 × 8CPU/16GB |
| Workers | 2 × 4CPU/8GB | 4 × 8CPU/16GB | 8 × 16CPU/32GB |
| MongoDB | M10 | M30 | M60 |
| Storage | 1TB | 10TB | 100TB |
| Throughput | 1K ops/s | 10K ops/s | 100K ops/s |