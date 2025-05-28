<p align="center">
  <img src="dashboard/static/argon-logo.png" alt="Argon Logo" width="120"/>
</p>

<h1 align="center">Argon: Serverless, Branchable MongoDB Platform</h1>

<p align="center">
  <b>Git-style branching, stateless compute, and S3-powered time-travel for MongoDB</b><br>
  <a href="#quickstart">Quickstart</a> • <a href="#features">Features</a> • <a href="#demo-scenario">Demo Scenario</a> • <a href="#how-it-works">How it works</a> • <a href="#contributing">Contributing</a>
</p>

---

## Why Argon? (Motivation)
Modern data teams need to experiment, branch, and collaborate on databases just like they do with code. Traditional MongoDB deployments are monolithic, stateful, and hard to branch or time-travel. Argon brings Git-style workflows to MongoDB, enabling rapid prototyping, isolated development, and safe experimentation—without the cost and complexity of managing full database clones. By separating compute from storage and leveraging S3 for versioned snapshots, Argon enables stateless, serverless, and cost-efficient database environments that can be branched, suspended, resumed, or restored on demand.

## Features
- **Git-style branching for MongoDB:** Create, list, delete, suspend, and resume branches.
- **Stateless Compute:** MongoDB instances run in Docker containers, isolated from persistent storage.
- **S3-Powered Storage:** Branch data is stored as versioned snapshots in AWS S3, enabling durability and point-in-time recovery.
- **Time-Travel/Restore:** Create new branches from historical snapshots of existing branches.
- **CLI:** A command-line interface for managing projects and branches.
- **Web Dashboard (Experimental):** A basic web interface for visualizing and managing branches. Includes an experimental auto-suspend feature for idle branches.

## Quickstart

1.  **Prerequisites:**
    *   Docker installed and running.
    *   AWS CLI installed and configured with credentials that have S3 access.
    *   Python 3.8+

2.  **Clone the repository:**
    ```sh
    git clone https://github.com/your-username/argon.git # Replace with your repo path
    cd argon
    ```

3.  **Install dependencies:**
    ```sh
    pip install -r requirements.txt
    ```

4.  **Set up environment variables:**
    *   Copy the provided `.env.example` file to a new file named `.env`:
        ```sh
        cp .env.example .env
        ```
    *   Edit the `.env` file to provide your AWS S3 bucket name, AWS credentials, and other configurations as needed. See the "Environment Variables" section below for details on each variable.

5.  **Prepare a base MongoDB snapshot:**
    *   Argon needs an initial `dump.archive` in your S3 bucket at the path `base/dump.archive`.
    *   You can create this from any MongoDB database. For a quick test, you can use the provided `data/base_dump.archive`:
        *   Copy the sample dump to your S3 bucket (replace `<your-s3-bucket>`):\n            ```sh\n            aws s3 cp data/base_dump.archive s3://<your-s3-bucket>/base/dump.archive\n            ```
        *   Alternatively, to create one from scratch:
            *   Ensure a local MongoDB instance is running.
            *   Insert some data if it's a new instance:
                ```sh
                mongo --eval \'db.test.insertOne({message: "Hello Argon Base!"});\'
                ```
            *   Create the dump:
                ```sh
                mongodump --archive=./base_dump.archive --gzip --db test 
                ```
            *   Upload to your S3 bucket (replace `<your-s3-bucket>`):
                ```sh
                aws s3 cp ./base_dump.archive s3://<your-s3-bucket>/base/dump.archive
                ```
        *   Make sure your `.env` file has `S3_BUCKET=<your-s3-bucket>`.

6.  **Initialize Argon's metadata database:**
    ```sh
    python3 cli/main.py
    ```
    *(Running the CLI for the first time initializes the necessary local SQLite databases.)*


## Demo Scenario: Your First Branch

1.  **Create a project:**
    ```sh
    python3 cli/main.py project create my-first-project
    ```

2.  **Create a branch:** This will pull the `base/dump.archive` from S3 and start a new MongoDB container.
    ```sh
    python3 cli/main.py branch create dev-branch --project my-first-project
    ```

3.  **List branches:** You\'ll see `dev-branch` running on a specific port.
    ```sh
    python3 cli/main.py branch list --project my-first-project
    ```

4.  **Get connection string and make a change:**
    ```sh
    python3 cli/main.py connect dev-branch --project my-first-project
    # Use the output connection string with mongo shell or Compass
    # mongo "mongodb://localhost:PORT" --eval \'db.test.insertOne({change: "first change"});\'
    ```

5.  **Suspend the branch (creates version 1):** This snapshots its current state to S3 and stops the container.
    ```sh
    python3 cli/main.py branch suspend dev-branch --project my-first-project
    ```

6.  **Resume the branch and make another change:**
    ```sh
    python3 cli/main.py branch resume dev-branch --project my-first-project
    python3 cli/main.py connect dev-branch --project my-first-project
    # mongo "mongodb://localhost:PORT" --eval \'db.test.insertOne({change: "second change"});\'
    ```

7.  **Suspend the branch again (creates version 2):**
    ```sh
    python3 cli/main.py branch suspend dev-branch --project my-first-project
    ```

8.  **Time-Travel: Create a new branch from an older version:**
    *   First, list available versions for `dev-branch`. Note the `timestamp` of the *first* snapshot (after "first change").
        ```sh
        python3 cli/main.py branch list-versions dev-branch --project my-first-project
        ```
    *   Create `dev-branch-v1` from that specific snapshot using its timestamp. The `NAME` argument (`dev-branch-v1` here) is the name for the new branch. Replace `<TIMESTAMP_OF_FIRST_SNAPSHOT>` with the actual timestamp (e.g., `YYYY-MM-DDTHH:MM:SSZ`).
        ```sh
        python3 cli/main.py branch time-travel dev-branch-v1 --project my-first-project --from-branch dev-branch --timestamp <TIMESTAMP_OF_FIRST_SNAPSHOT>
        ```
    *   (Alternatively, if your S3 bucket has versioning enabled and `list-versions` shows valid S3 Version IDs, you might need a different command or the `time-travel` command would need to be enhanced to support `--version-id`. For now, timestamp is the primary method.)

9.  **Verify the time-traveled branch:**
    Connect to `dev-branch-v1` and check its data. It should only contain "first change".
    ```sh
    python3 cli/main.py connect dev-branch-v1 --project my-first-project
    # mongo "mongodb://localhost:PORT_OF_V1" --eval \'db.test.find();\' 
    # This should show {change: "first change"} but not {change: "second change"}
    ```

10. **Clean up:** Delete the branches.
    ```sh
    python3 cli/main.py branch delete dev-branch --project my-first-project
    python3 cli/main.py branch delete dev-branch-v1 --project my-first-project
    ```

## How it works
- **Create branch:** Restores MongoDB from S3 snapshot, starts a container.
- **Suspend branch:** Snapshots running container to S3, stops/removes container.
- **Resume branch:** Restores from S3, starts a new container (stateless compute).
- **Delete branch:** Snapshots to S3, removes container and metadata.
- **S3:** All branch data is versioned and stored in S3 for time-travel/restore.

## High-Level Design

Argon consists of a few key components that work together to provide branchable, serverless MongoDB environments.

### Core Components:
*   **Argon CLI (`cli/main.py`):** The primary user interface for managing projects and branches. It interacts with the `BranchManager` and `Metadata` store.
*   **Branch Manager (`core/branch_manager.py`):** Contains the core logic for branch operations (create, suspend, resume, delete, time-travel). It orchestrates interactions between Docker, S3, and the metadata store.
*   **Docker Utilities (`core/docker_utils.py`):** Manages Docker containers for running MongoDB instances. Handles starting, stopping, and interacting with containers.
*   **S3 Utilities (`core/s3_utils.py`):** Handles uploading and downloading MongoDB snapshots (dumps) to/from AWS S3.
*   **Metadata Store (`core/metadata.py`):** A SQLite database that stores information about projects, branches, their states, container details, S3 paths, and versions.
*   **Dashboard (`dashboard/app.py`):** An experimental web interface providing a GUI for some operations.

### Component Interaction Diagram:

```text
                               +-----------------+
                               |      User       |
                               +-----------------+
                                   /          \\
                                  /            \\
                 +-----------------+      +----------------------+
                 |    Argon CLI    |      | Web Dashboard (Exp.) |
                 +-----------------+      +----------------------+
                       \\                         /
                        \\                       /
                         +---------------------+
                         |  Branch Manager API |
                         +---------------------+
                           /       |         \\
                          /        |          \\
   +------------------------+  +---------------------+  +--------------------+
   | Metadata Service (.py) |  | Docker Service (.py)|  | S3 Service (.py)   |
   +------------------------+  +---------------------+  +--------------------+
           |                            |                        |
           |                            |                        |
+------------------------+  +---------------------+  +--------------------+
|   SQLite Metadata DB   |  |    Docker Engine    |  | AWS S3 Bucket      |
+------------------------+  +---------------------+  +--------------------+
```

### Branch State Flowchart:

A branch in Argon can go through several states:

```text
+----------------+
| Does Not Exist |
+----------------+
       |
       | create
       V
+----------------+
|    Running     | ----> suspend ----> +-----------+
+----------------+ <---- resume  <---- | Suspended |
       |    ^                             +-----------+
       |    |                                  |
       | delete                              | delete
       |    |                                  |
       V    +---------------------------------+
+----------------+
|     Deleted    |
| (Snapshotted)  |
+----------------+


Running State ----> time-travel (creates new branch in Running state)
Suspended State --> time-travel (creates new branch in Running state)
```

*   **Running:** The branch has an active MongoDB container associated with it.
*   **Suspended:** The branch data is snapshotted to S3, and the container is stopped/removed.
*   **Deleted/Snapshotted:** The branch metadata is removed (or marked deleted), and its final state is snapshotted to S3. The container is removed.
*   **Time-travel:** Creates a *new* branch from a historical snapshot of an existing branch. The original branch's state doesn't change directly due to this operation on its history.

## Environment Variables
Create a `.env` file in the root of the project with the following variables:

```env
# AWS Configuration
AWS_ACCESS_KEY_ID=your_aws_access_key_id
AWS_SECRET_ACCESS_KEY=your_aws_secret_access_key
AWS_DEFAULT_REGION=your_aws_region # e.g., us-east-1
S3_BUCKET=your_argon_s3_bucket_name # The S3 bucket Argon will use

# Argon Configuration
ARGON_BASE_SNAPSHOT_S3_PATH=base/dump.archive # Default S3 path for the base snapshot
ARGON_AUTO_SUSPEND_ENABLED=true # Set to true to enable dashboard auto-suspend, false to disable
DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES=60 # Optional: Minutes of inactivity before auto-suspend (if enabled)
```
*(Note: For local development without exposing AWS keys directly in `.env`, consider using AWS CLI profiles or instance roles if deploying.)*


## Folder Structure
- `core/` - Core logic (branching, Docker, S3, metadata)
- `cli/` - CLI tool
- `dashboard/` - Minimal dashboard
- `data/` - Sample data (e.g., `base_dump.archive`).

## Status (Initial Launch)
- [x] Core: Project and Branch CRUD (create, list, delete, suspend, resume) via CLI.
- [x] Core: Stateless compute with Docker.
- [x] Core: S3 backend for snapshot/restore.
- [x] Core: Time-travel for branches via CLI.
- [x] CLI for all core operations.
- [x] Basic Web Dashboard (experimental, with auto-suspend feature).
- [ ] Comprehensive automated tests.
- [ ] Detailed contribution guidelines.

See the dashboard or run `python3 cli/main.py --help` for more.

## Contributing
Contributions are welcome! Please open an issue to discuss your ideas or report bugs.
(Further details to be added in `CONTRIBUTING.md`)
