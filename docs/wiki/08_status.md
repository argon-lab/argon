## 📊 Project Status (Initial Launch)

This outlines the current implemented features and what's planned for the near future.

### ✅ Completed:
*   **Core Functionality:**
    *   [x] Project CRUD (Create, Read, Update, Delete) via CLI.
    *   [x] Branch CRUD (Create, List, Delete, Suspend, Resume) via CLI.
    *   [x] Stateless compute using Docker for MongoDB instances.
    *   [x] AWS S3 backend for robust snapshot storage and restore.
    *   [x] Time-travel capability for branches via CLI (restore from historical snapshots).
*   **CLI:**
    *   [x] Comprehensive CLI for all core operations.
*   **Web Dashboard:**
    *   [x] Basic Web Dashboard for visualization and management (experimental).
    *   [x] Auto-suspend feature for idle branches in the dashboard (experimental).

### 🚧 Planned / To-Do:
*   [ ] **Testing:**
    *   Comprehensive automated tests (unit, integration).
*   [ ] **Documentation & Community:**
    *   Detailed contribution guidelines (`CONTRIBUTING.md`).
    *   More examples and use-cases.
*   [ ] **Features & Enhancements:**
    *   More advanced dashboard features.
    *   Support for other S3-compatible storage.
    *   Configuration for custom Docker images/MongoDB versions.
    *   Performance optimizations.

See the main dashboard or run `python3 cli/main.py --help` for more on current CLI commands.
