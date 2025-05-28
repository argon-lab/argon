Wiki Navigation
[README](../../README.md) | [Introduction & Motivation](01_introduction.md) | [Features](02_features.md) | [Quickstart Guide](03_quickstart_guide.md) | [Demo Scenario](04_demo_scenario.md) | [How it Works](05_how_it_works.md) | [Environment Variables](06_environment_variables.md) | [Folder Structure](07_folder_structure.md) | [Project Status](08_status.md) | [Contributing](09_contributing.md)

## Table of Contents
- [Core Operations Flow](#core-operations-flow)
- [High-Level Design](#🏛️-high-level-design)
  - [Core Components](#core-components)
  - [Component Interaction Diagram](#component-interaction-diagram)
- [Branch State Flowchart](#🌊-branch-state-flowchart)

## ⚙️ How Argon Works

Argon simplifies MongoDB workflows through a clever combination of Docker, S3, and a metadata store.

### Core Operations Flow:
*   **🌿 Create Branch:** Restores a MongoDB instance from an S3 snapshot and starts it within a new Docker container.
*   **⏸️ Suspend Branch:** Takes a snapshot of the running container's MongoDB data, uploads it to S3, and then stops/removes the container to save resources.
*   **▶️ Resume Branch:** Restores the latest (or a specified) snapshot for that branch from S3 and starts a new container with that data.
*   **🗑️ Delete Branch:** Snapshots the final state to S3 (optional), removes the container, and cleans up metadata.
*   **⏳ Time-Travel:** Creates a *new* branch by restoring an older, specific snapshot version from S3 into a new container.

All branch data, including multiple versions (snapshots), is stored securely and cost-effectively in your AWS S3 bucket. This enables powerful time-travel and restore capabilities.

## 🏛️ High-Level Design

Argon consists of a few key components that work together:

### Core Components:
*   **⌨️ Argon CLI (`cli/main.py`):** The primary user interface for managing projects and branches. It interacts with the `BranchManager` and `Metadata` store.
*   **🧠 Branch Manager (`core/branch_manager.py`):** Contains the core logic for all branch operations (create, suspend, resume, delete, time-travel). It orchestrates interactions between Docker, S3, and the metadata store.
*   **🐳 Docker Utilities (`core/docker_utils.py`):** Manages Docker containers for running MongoDB instances. Handles starting, stopping, and interacting with containers.
*   **📦 S3 Utilities (`core/s3_utils.py`):** Handles uploading and downloading MongoDB snapshots (dumps) to/from AWS S3.
*   **📝 Metadata Store (`core/metadata.py`):** A local SQLite database that stores information about projects, branches, their states, container details, S3 paths, and version history (snapshots).
*   **🖥️ Dashboard (`dashboard/app.py`):** An experimental web interface providing a GUI for some operations, including visualizing branches and their status.

### Component Interaction Diagram:

```text
                               +-----------------+
                               |      User       |
                               +-----------------+
                                   /          \
                                  /            \
                 +-----------------+      +----------------------+
                 |    Argon CLI    |      | Web Dashboard (Exp.) |
                 +-----------------+      +----------------------+
                       \                         /
                        \                       /
                         +---------------------+
                         |  Branch Manager API |
                         +---------------------+
                           /       |         \
                          /        |          \
   +------------------------+  +---------------------+  +--------------------+
   | Metadata Service (.py) |  | Docker Service (.py)|  | S3 Service (.py)   |
   +------------------------+  +---------------------+  +--------------------+
           |                            |                        |
           |                            |                        |
+------------------------+  +---------------------+  +--------------------+
|   SQLite Metadata DB   |  |    Docker Engine    |  | AWS S3 Bucket      |
+------------------------+  +---------------------+  +--------------------+
```

### 🌊 Branch State Flowchart:

A branch in Argon can go through several states:

```text
+----------------+
| Does Not Exist |
+----------------+
       |
       | create
       V
+----------------+      suspend      +-----------+
|    Running     | -----------------> | Suspended |
+----------------+ <----------------- +-----------+
       |    ^          resume             |
       |    |                             |
       | delete                       delete
       |    |                             |
       V    +-----------------------------+
+----------------+
|     Deleted    |
| (Snapshotted)  |
+----------------+


Running State ----> time-travel (creates new branch in Running state)
Suspended State --> time-travel (creates new branch in Running state)
```

*   **🟢 Running:** The branch has an active MongoDB container associated with it.
*   **🟠 Suspended:** The branch data is snapshotted to S3, and the container is stopped/removed.
*   **🔴 Deleted/Snapshotted:** The branch metadata is removed (or marked deleted), and its final state is snapshotted to S3. The container is removed.
*   **⏳ Time-travel:** Creates a *new* branch from a historical snapshot of an existing branch. The original branch's state doesn't change directly due to this operation on its history.

[Previous: Demo Scenario](04_demo_scenario.md) | [Next: Environment Variables](06_environment_variables.md)
