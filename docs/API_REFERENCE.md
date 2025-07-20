# Argon API Reference

## Overview

Argon provides both a CLI interface and a REST API for managing MongoDB branches. This document covers all available commands and endpoints.

## Table of Contents

1. [CLI Commands](#cli-commands)
2. [REST API Endpoints](#rest-api-endpoints)
3. [Data Models](#data-models)
4. [Error Handling](#error-handling)

## CLI Commands

### Installation

```bash
# Using Homebrew
brew install argon-lab/tap/argonctl

# Using npm
npm install -g argonctl

# Using Go (from source)
git clone https://github.com/argon-lab/argon
cd argon/cli && go build -o argon
```

### Global Options

All commands support these global options:

- `--config` - Path to config file (default: `~/.argon/config.yaml`)
- `--mongodb-uri` - MongoDB connection URI
- `--storage-type` - Storage backend: `local`, `s3`, `gcs`, `azure` (default: `local`)
- `--storage-path` - Path for local storage or bucket name for cloud storage
- `--debug` - Enable debug logging

### Commands

#### `argon status`

Show system status and WAL configuration.

```bash
export ENABLE_WAL=true
argon status
```

Output:
```
🚀 Argon System Status:
   Time Travel: ✅ Enabled
   Instant Branching: ✅ Enabled
   Performance Mode: ✅ WAL Architecture
```

#### `argon projects create`

Create a new project with time travel enabled.

```bash
argon projects create my-app
```

Output:
```
✅ Created project 'my-app' with time travel enabled
   Project ID: 6a7c9e12c395913d7800d91f
   Default branch: main
```

#### `argon branches create`

Create a new branch within a project.

```bash
argon branches create feature-x -p my-app
```

Options:
- `-p, --project` - Project ID or name
- `--from` - Source branch (default: main)

#### `argon projects list`

List all projects with time travel enabled.

```bash
argon projects list
```

Output:
```
📁 Projects with Time Travel:
  - my-app (ID: 6a7c9e12c395913d7800d91f)
    Branches: 1
```

#### `argon branches list`

List branches in a project.

```bash
argon branches list -p my-app
```

#### `argon time-travel info`

Show time travel information for a branch.

```bash
argon time-travel info -p 6a7c9e12c395913d7800d91f -b main
```

Output:
```
⏰ Time Travel Info for branch 'main':
   Branch ID: 6a7c9e12c395913d7800d91f
   LSN Range: 0 - 4
   Total Entries: 0
```

#### `argon metrics`

Show performance metrics and system statistics.

```bash
argon metrics
```

Output includes:
- WAL write throughput
- Branch creation performance
- Storage efficiency
- System health

#### `argonctl branch delete`

Delete a branch and its associated data.

```bash
argonctl branch delete feature-branch
```

Options:
- `--force` - Skip confirmation prompt
- `--keep-backup` - Keep a backup before deletion

#### `argonctl branch status`

Show detailed status of a branch.

```bash
argonctl branch status feature-branch
```

Output includes:
- Branch metadata
- Storage statistics
- Recent activity
- Change history

#### `argonctl snapshot create`

Create a point-in-time snapshot of a branch.

```bash
argonctl snapshot create --branch main --name "before-migration"
```

Options:
- `--branch` (required) - Branch to snapshot
- `--name` - Snapshot name
- `--description` - Snapshot description

#### `argonctl snapshot restore`

Restore a branch from a snapshot.

```bash
argonctl snapshot restore snapshot-id --to restored-branch
```

Options:
- `--to` (required) - Target branch name
- `--overwrite` - Overwrite if branch exists

## REST API Endpoints

Base URL: `http://localhost:8080/api/v1`

### Authentication

All API requests require an API key in the header:

```http
Authorization: Bearer YOUR_API_KEY
```

### Endpoints

#### `GET /branches`

List all branches.

**Response:**
```json
{
  "branches": [
    {
      "id": "branch_abc123",
      "name": "main",
      "status": "active",
      "created_at": "2025-07-18T10:00:00Z",
      "last_activity": "2025-07-18T14:30:00Z",
      "stats": {
        "documents": 1500000,
        "storage_size": 1288490188,
        "indexes": 12
      }
    }
  ]
}
```

#### `POST /branches`

Create a new branch.

**Request:**
```json
{
  "name": "feature-branch",
  "from": "main",
  "description": "New feature development"
}
```

**Response:**
```json
{
  "branch": {
    "id": "branch_def456",
    "name": "feature-branch",
    "status": "creating",
    "created_at": "2025-07-18T15:00:00Z"
  }
}
```

#### `GET /branches/{branch_id}`

Get branch details.

**Response:**
```json
{
  "branch": {
    "id": "branch_def456",
    "name": "feature-branch",
    "status": "active",
    "parent": "main",
    "created_at": "2025-07-18T15:00:00Z",
    "last_activity": "2025-07-18T15:30:00Z",
    "stats": {
      "documents": 1500000,
      "storage_size": 1288490188,
      "indexes": 12,
      "collections": ["users", "orders", "products"]
    },
    "config": {
      "storage_type": "s3",
      "compression": "zstd",
      "compression_level": 3
    }
  }
}
```

#### `DELETE /branches/{branch_id}`

Delete a branch.

**Response:**
```json
{
  "message": "Branch deleted successfully",
  "deleted_at": "2025-07-18T16:00:00Z"
}
```

#### `POST /branches/{branch_id}/merge`

Merge a branch into another.

**Request:**
```json
{
  "target": "main",
  "strategy": "manual",
  "options": {
    "resolve_conflicts": "theirs"
  }
}
```

**Response:**
```json
{
  "merge": {
    "id": "merge_ghi789",
    "source": "feature-branch",
    "target": "main",
    "status": "in_progress",
    "started_at": "2025-07-18T17:00:00Z"
  }
}
```

#### `GET /branches/{branch_id}/changes`

Get change history for a branch.

**Response:**
```json
{
  "changes": [
    {
      "id": "change_123",
      "operation": "insert",
      "collection": "users",
      "document_id": "507f1f77bcf86cd799439011",
      "timestamp": "2025-07-18T15:30:00Z",
      "size_bytes": 1024
    }
  ],
  "pagination": {
    "total": 1500,
    "page": 1,
    "per_page": 100
  }
}
```

#### `POST /snapshots`

Create a snapshot.

**Request:**
```json
{
  "branch_id": "branch_abc123",
  "name": "pre-migration",
  "description": "Snapshot before database migration"
}
```

**Response:**
```json
{
  "snapshot": {
    "id": "snap_jkl012",
    "branch_id": "branch_abc123",
    "name": "pre-migration",
    "created_at": "2025-07-18T18:00:00Z",
    "size_bytes": 1288490188,
    "status": "completed"
  }
}
```

## Data Models

### Branch

```typescript
interface Branch {
  id: string;
  name: string;
  status: 'active' | 'creating' | 'merging' | 'archived' | 'error';
  parent?: string;
  created_at: string;
  last_activity: string;
  stats: BranchStats;
  config: BranchConfig;
}

interface BranchStats {
  documents: number;
  storage_size: number;
  indexes: number;
  collections: string[];
}

interface BranchConfig {
  storage_type: 'local' | 's3' | 'gcs' | 'azure';
  compression: 'none' | 'zstd' | 'gzip';
  compression_level: number;
}
```

### Change

```typescript
interface Change {
  id: string;
  operation: 'insert' | 'update' | 'delete';
  collection: string;
  document_id: string;
  timestamp: string;
  size_bytes: number;
  data?: any; // Only for detailed change endpoints
}
```

### Snapshot

```typescript
interface Snapshot {
  id: string;
  branch_id: string;
  name: string;
  description?: string;
  created_at: string;
  size_bytes: number;
  status: 'creating' | 'completed' | 'failed';
  metadata: Record<string, any>;
}
```

## Error Handling

All API errors follow this format:

```json
{
  "error": {
    "code": "BRANCH_NOT_FOUND",
    "message": "Branch 'feature-xyz' not found",
    "details": {
      "branch_name": "feature-xyz",
      "suggestion": "Use 'argonctl branch list' to see available branches"
    }
  }
}
```

### Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `BRANCH_NOT_FOUND` | 404 | Branch does not exist |
| `BRANCH_EXISTS` | 409 | Branch name already taken |
| `INVALID_BRANCH_NAME` | 400 | Branch name contains invalid characters |
| `STORAGE_ERROR` | 500 | Storage backend error |
| `MERGE_CONFLICT` | 409 | Merge conflicts require resolution |
| `QUOTA_EXCEEDED` | 429 | Storage or operation quota exceeded |
| `UNAUTHORIZED` | 401 | Missing or invalid API key |
| `FORBIDDEN` | 403 | Insufficient permissions |

## Rate Limits

API requests are rate-limited:

- **Free tier**: 1000 requests/hour
- **Pro tier**: 10000 requests/hour
- **Enterprise**: Unlimited

Rate limit headers:
```
X-RateLimit-Limit: 1000
X-RateLimit-Remaining: 950
X-RateLimit-Reset: 1626616800
```

## Webhooks

Configure webhooks for branch events:

```json
POST /webhooks
{
  "url": "https://your-app.com/webhook",
  "events": ["branch.created", "branch.merged", "branch.deleted"],
  "secret": "your-webhook-secret"
}
```

### Event Types

- `branch.created` - New branch created
- `branch.deleted` - Branch deleted
- `branch.merged` - Branch merged
- `branch.status_changed` - Branch status changed
- `snapshot.created` - Snapshot created
- `snapshot.restored` - Snapshot restored