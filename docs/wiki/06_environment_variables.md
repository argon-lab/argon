## ⚙️ Environment Variables

Create a `.env` file in the root of the project. You can copy `.env.example` to get started:
```sh
cp .env.example .env
```

Then, populate it with the following variables:

```env
# AWS Configuration
AWS_ACCESS_KEY_ID=your_aws_access_key_id
AWS_SECRET_ACCESS_KEY=your_aws_secret_access_key
AWS_DEFAULT_REGION=your_aws_region # e.g., us-east-1
S3_BUCKET=your_argon_s3_bucket_name # The S3 bucket Argon will use for snapshots

# Argon Configuration
ARGON_BASE_SNAPSHOT_S3_PATH=base/dump.archive # Default S3 path for the base snapshot
ARGON_AUTO_SUSPEND_ENABLED=true # Set to true to enable dashboard auto-suspend, false to disable
DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES=60 # Optional: Minutes of inactivity before auto-suspend (if enabled)
```

**Notes:**
*   For local development without exposing AWS keys directly in `.env`, consider using [AWS CLI profiles](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-profiles.html) or instance roles if deploying to an AWS environment (e.g., EC2). Argon's S3 utilities will attempt to use credentials in the standard AWS SDK order (env vars, shared credentials file, IAM roles, etc.).
*   `ARGON_BASE_SNAPSHOT_S3_PATH`: This is the S3 key where Argon expects to find the initial MongoDB dump archive when creating the very first branch of a project if no other source is specified.
*   `ARGON_AUTO_SUSPEND_ENABLED`: Controls the experimental auto-suspend feature in the web dashboard.
*   `DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES`: If auto-suspend is enabled, this defines the period of inactivity (no requests to the branch's MongoDB port) before a branch is automatically suspended.
