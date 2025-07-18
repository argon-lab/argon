# Argon CLI: Neon Compatibility Analysis

## Neon CLI Pattern Analysis

### Command Structure
Neon uses `neonctl` as the base command with subcommands:
```bash
neonctl <resource> <action> [options]
```

### Core Commands

#### Authentication
```bash
neonctl auth                        # Browser-based auth
neonctl --api-key <key>            # API key auth
```

#### Projects
```bash
neonctl projects list              # List all projects
neonctl projects create            # Create new project
neonctl projects get <id>          # Get project details
neonctl projects delete <id>       # Delete project
neonctl projects update <id>       # Update project
```

#### Branches
```bash
neonctl branches list              # List branches
neonctl branches create            # Create branch
neonctl branches get <id>          # Get branch details
neonctl branches delete <id>       # Delete branch
neonctl branches rename <id> <name> # Rename branch
neonctl branches set-default <id>  # Set default branch
```

#### Databases  
```bash
neonctl databases list             # List databases
neonctl databases create           # Create database
neonctl databases delete <id>      # Delete database
```

#### Connection
```bash
neonctl connection-string          # Get connection string
```

### Global Options
- `--output json|yaml|table` - Output format
- `--project-id <id>` - Target project
- `--api-key <key>` - Authentication
- `--config-dir <path>` - Config directory

### Branch Creation Options
```bash
neonctl branches create \
  --name <branch_name> \
  --parent <parent_branch> \
  --compute [true/false] \
  --type read_write|read_only \
  --cu <compute_units> \
  --psql \
  --schema-only \
  --suspend-timeout <seconds>
```

## Argon CLI Compatibility Design

To ensure zero learning curve for Neon users, Argon will use identical patterns:

### Base Command
```bash
argon <resource> <action> [options]
```

### Authentication (Identical to Neon)
```bash
argon auth                         # Browser-based auth
argon --api-key <key>             # API key auth
```

### Projects (MongoDB-adapted)
```bash
argon projects list               # List all projects
argon projects create             # Create new project  
argon projects get <id>           # Get project details
argon projects delete <id>        # Delete project
argon projects update <id>        # Update project
```

### Branches (Identical UX, MongoDB backend)
```bash
argon branches list               # List branches
argon branches create             # Create branch
argon branches get <id>           # Get branch details
argon branches delete <id>        # Delete branch
argon branches rename <id> <name> # Rename branch
argon branches set-default <id>   # Set default branch
```

### Databases → Collections (MongoDB-specific)
```bash
argon collections list            # List collections
argon collections create          # Create collection
argon collections delete <name>   # Delete collection
```

### Connection (MongoDB connection strings)
```bash
argon connection-string           # Get MongoDB URI
```

### Global Options (Identical)
- `--output json|yaml|table` - Output format
- `--project-id <id>` - Target project  
- `--api-key <key>` - Authentication
- `--config-dir <path>` - Config directory

### Branch Creation (Enhanced for MongoDB)
```bash
argon branches create \
  --name <branch_name> \
  --parent <parent_branch> \
  --compute [true/false] \
  --type active|suspended \
  --description <description> \
  --sync [true/false] \
  --suspend-timeout <seconds>
```

## ML/AI Extensions (Argon-specific)

While maintaining Neon compatibility, add ML-specific commands:

### Experiments
```bash
argon experiments list            # List ML experiments
argon experiments create          # Create experiment
argon experiments link <branch>   # Link branch to experiment
```

### Datasets
```bash
argon datasets list               # List dataset versions
argon datasets version            # Version current dataset
argon datasets stats              # Dataset statistics
```

### Artifacts
```bash
argon artifacts list              # List model artifacts
argon artifacts upload <file>     # Upload artifact
argon artifacts download <id>     # Download artifact
```

## Implementation Priority

### Phase 1: Neon Compatibility (Day 4)
- Implement exact Neon command structure
- Authentication flow
- Basic project/branch operations
- Identical output formats

### Phase 2: MongoDB Adaptation (Day 5)
- MongoDB-specific features
- Collection management
- Connection string generation

### Phase 3: ML Extensions (Day 6)
- Experiment tracking commands
- Dataset versioning
- Artifact management

## Key Compatibility Features

1. **Identical Command Names**: `branches create`, `projects list`, etc.
2. **Same Global Flags**: `--output`, `--project-id`, `--api-key`
3. **Consistent Output Formats**: JSON, YAML, table
4. **Similar Authentication**: Browser-based + API key
5. **Familiar Workflow**: Create → Switch → Develop → Merge

## Migration Story for Neon Users

```bash
# Neon workflow
neonctl auth
neonctl projects create --name my-project
neonctl branches create --name feature-branch
neonctl connection-string

# Identical Argon workflow
argon auth
argon projects create --name my-project  
argon branches create --name feature-branch
argon connection-string
```

**Result**: Neon users can switch to Argon with zero command relearning, just point to MongoDB instead of PostgreSQL.