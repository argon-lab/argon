Wiki Navigation
[README](../../README.md) | [Introduction & Motivation](01_introduction.md) | [Features](02_features.md) | [Quickstart Guide](03_quickstart_guide.md) | [Demo Scenario](04_demo_scenario.md) | [How it Works](05_how_it_works.md) | [Environment Variables](06_environment_variables.md) | [Folder Structure](07_folder_structure.md) | [Project Status](08_status.md) | [Contributing](09_contributing.md)

## Table of Contents
- [Environment Variables](#⚙️-environment-variables)
- [AWS Configuration](#aws-configuration)
- [Argon Configuration](#argon-configuration)
- [Notes](#notes)

## ⚙️ Environment Variables

The `argonctl` CLI provides an interactive first-time setup that will help you configure all required environment variables. However, you can also set them up manually using a `.env` file.

### Configuration File Locations

The CLI looks for environment variables in the following locations, in order:
1. Project-level: `./.env` in your current directory
2. User-level: `~/.argon/.env` in your home directory

To manually create a configuration file:
```sh
# Project-level config
touch .env

# OR user-level config (recommended)
mkdir -p ~/.argon && touch ~/.argon/.env
```

### Required Variables
```env
# AWS Configuration (Required for S3 operations)
AWS_ACCESS_KEY_ID=your_aws_access_key_id
AWS_SECRET_ACCESS_KEY=your_aws_secret_access_key
S3_BUCKET=your_argon_s3_bucket_name # The S3 bucket Argon will use for snapshots
```

### Optional Variables
```env
# AWS Configuration (Optional)
AWS_DEFAULT_REGION=your_aws_region # e.g., us-east-1 (default)

# Argon Configuration (Optional)
ARGON_BASE_SNAPSHOT_S3_PATH=base/dump.archive # Default S3 path for the base snapshot
ARGON_AUTO_SUSPEND_ENABLED=true # Set to true to enable dashboard auto-suspend
DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES=60 # Minutes before auto-suspend (if enabled)
```

**Notes:**
*   For local development without exposing AWS keys directly in `.env`, consider using [AWS CLI profiles](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-profiles.html) or instance roles if deploying to an AWS environment (e.g., EC2). Argon's S3 utilities will attempt to use credentials in the standard AWS SDK order (env vars, shared credentials file, IAM roles, etc.).
*   `ARGON_BASE_SNAPSHOT_S3_PATH`: This is the S3 key where Argon expects to find the initial MongoDB dump archive when creating the very first branch of a project if no other source is specified.
*   `ARGON_AUTO_SUSPEND_ENABLED`: Controls the experimental auto-suspend feature in the web dashboard.
*   `DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES`: If auto-suspend is enabled, this defines the period of inactivity (no requests to the branch's MongoDB port) before a branch is automatically suspended.

[Previous: How it Works](05_how_it_works.md) | [Next: Folder Structure](07_folder_structure.md)
