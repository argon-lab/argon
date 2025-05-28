<p align="center">
  <img src="dashboard/static/argon-logo.png" alt="Argon Logo" width="150"/>
</p>

<h1 align="center">🚀 Argon: Serverless, Branchable MongoDB Platform 🚀</h1>

<p align="center">
  <b>Transform your MongoDB workflows with Git-style branching, stateless compute, and S3-powered time-travel!</b>
  <br><br>
  <a href="docs/wiki/01_introduction.md">🤔 Why Argon?</a> •
  <a href="docs/wiki/02_features.md">✨ Features</a> •
  <a href="docs/wiki/05_how_it_works.md">⚙️ How it Works</a> •
  <a href="docs/wiki/03_quickstart_guide.md">🚀 Quickstart</a> •
  <a href="#📚-dive-deeper-wiki">📚 Dive Deeper (Wiki)</a> •
  <a href="docs/wiki/09_contributing.md">🤝 Contributing</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/python-3.8+-blue.svg" alt="Python 3.8+"> 
  <img src="https://img.shields.io/badge/docker-required-blue.svg" alt="Docker Required"> 
  <img src="https://img.shields.io/badge/AWS%20S3-required-orange.svg" alt="AWS S3 Required"> 
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green.svg" alt="License: MIT"></a>
</p>

---

## 🤔 Why Argon?

Ever wished you could manage your databases with the same flexibility as your code? Traditional MongoDB setups can be rigid and resource-intensive, making it challenging to:

*   🧪 **Experiment Freely:** Quickly spin up isolated environments for testing new features or data models without impacting production.
*   🌳 **Branch & Version Data:** Create independent "branches" of your database for different development tasks, just like Git.
*   ⏪ **Rollback Easily:** "Time-travel" to previous data states effortlessly if something goes wrong.
*   💰 **Optimize Costs:** Avoid paying for idle, full-scale database clones.

Argon addresses these pain points by bringing the power of **Git-like branching, stateless compute, and S3-backed versioning** to MongoDB. It empowers developers and data teams to work more agilely, collaborate effectively, and innovate faster.

👉 **[Discover the full motivation (Wiki)](./docs/wiki/01_introduction.md)**

## ✨ Features

Argon is packed with features to supercharge your database workflows:

*   **🌿 Git-style Branching:** Create, suspend, resume, and delete database branches.
*   **💨 Stateless Compute:** MongoDB runs in lightweight Docker containers, decoupled from persistent storage.
*   **💾 S3-Powered Storage:** Durable, versioned snapshots of your data are stored efficiently in AWS S3.
*   **⏳ Time-Travel:** Restore or create new branches from any historical snapshot.
*   **⌨️ Powerful CLI:** A comprehensive command-line interface to manage all aspects of Argon.
*   **🖥️ Web Dashboard (Experimental):** Visualize and manage branches, with an optional auto-suspend feature for idle instances.

👉 **[Explore all features in detail (Wiki)](./docs/wiki/02_features.md)**

## ⚙️ How it Works

Argon cleverly combines Docker for containerization, AWS S3 for persistent, versioned storage, and a local metadata database to manage your branches:

1.  **Branch Creation:** When you create a branch, Argon can start from a base snapshot (e.g., a clean database or a production dump) stored in S3. It pulls this snapshot and launches a new, isolated MongoDB instance in a Docker container.
2.  **Making Changes:** You connect to this containerized MongoDB as usual and make your changes.
3.  **Suspending a Branch:** When you suspend a branch, Argon takes a snapshot (dump) of the container's current data, uploads it to S3 (creating a new version), and then stops and removes the Docker container, freeing up local resources.
4.  **Resuming a Branch:** To resume, Argon pulls the latest (or a specified) snapshot for that branch from S3 and starts a fresh Docker container with that data.
5.  **Time-Travel:** You can create a *new* branch from any historical snapshot of an existing branch, effectively rolling back to or inspecting a previous data state in an isolated environment.

This architecture ensures that your MongoDB instances are **stateless** (compute is separate from storage), **cost-effective** (only pay for S3 storage for suspended branches and compute when running), and **highly flexible**.

```text
+-----------------+      +---------------------+      +-----------------+
|      User       |----->|      Argon CLI      |<---->| Metadata (SQLite)|
+-----------------+      +---------------------+      +-----------------+
                             |          ^
                             |          | (Snapshot/Restore)
                             V          |
                       +---------------------+      +-----------------+
                       | Docker (MongoDB     |----->|  AWS S3 Bucket  |
                       |       Containers)   |      | (Snapshots)     |
                       +---------------------+      +-----------------+
```

👉 **[Get the deep dive on architecture and state flows (Wiki)](./docs/wiki/05_how_it_works.md)**

## 🚀 Quickstart

Ready to jump in? Get Argon running in minutes!

1.  **✅ Prerequisites:** Docker, AWS CLI (configured), Python 3.8+.
2.  **📥 Clone:** `git clone https://github.com/jakezwang/argon.git && cd argon`
3.  **🛠️ Install:** `pip install -r requirements.txt`
4.  **🔑 Configure:** Copy `.env.example` to `.env` and fill in your details (especially `S3_BUCKET`).
5.  **📦 Base Snapshot:** Ensure `base/dump.archive` is in your S3 bucket (see wiki for details).
6.  **🏁 Initialize:** `python3 cli/main.py` (first run initializes local DB).

👉 **[View the full Quickstart Guide (Wiki)](./docs/wiki/03_quickstart_guide.md)**

## 🧪 Demo Scenario

See Argon in action! Follow our step-by-step demo to create, branch, modify, and time-travel your first Argon-powered MongoDB.

👉 **[Walk through the Demo Scenario (Wiki)](./docs/wiki/04_demo_scenario.md)**

## 📚 Dive Deeper (Wiki)

Want to understand the nuts and bolts? Our wiki has you covered:

*   [🤔 Introduction & Motivation](./docs/wiki/01_introduction.md)
*   [✨ Features](./docs/wiki/02_features.md)
*   [⚙️ How Argon Works & High-Level Design](./docs/wiki/05_how_it_works.md)
*   [🚀 Quickstart Guide](./docs/wiki/03_quickstart_guide.md)
*   [🧪 Demo Scenario](./docs/wiki/04_demo_scenario.md)
*   [🔑 Environment Variables](./docs/wiki/06_environment_variables.md)
*   [📁 Folder Structure](./docs/wiki/07_folder_structure.md)
*   [📊 Project Status](./docs/wiki/08_status.md)
*   [🤝 Contributing to Argon](./docs/wiki/09_contributing.md)

## 📈 Status

Argon is currently in its initial launch phase. Key features are operational, and we're actively working on improvements and new capabilities.

👉 **[Check the current Project Status (Wiki)](./docs/wiki/08_status.md)**

## 🤝 Contributing

Contributions are highly welcome! Whether it's bug reports, feature ideas, or code, let's make Argon better together.

👉 **[Learn how to Contribute (Wiki)](./docs/wiki/09_contributing.md)**

(Further details in [`CONTRIBUTING.md`](./CONTRIBUTING.md))

## 📜 License

Argon is open-source software licensed under the [MIT License](./LICENSE).
