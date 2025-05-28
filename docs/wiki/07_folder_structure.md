Wiki Navigation
[README](../../README.md) | [Introduction & Motivation](01_introduction.md) | [Features](02_features.md) | [Quickstart Guide](03_quickstart_guide.md) | [Demo Scenario](04_demo_scenario.md) | [How it Works](05_how_it_works.md) | [Environment Variables](06_environment_variables.md) | [Folder Structure](07_folder_structure.md) | [Project Status](08_status.md) | [Contributing](09_contributing.md)

## 📁 Folder Structure

Here's a brief overview of the project's layout:

```
your-project/              # Your working directory
├── .env                   # Your environment variables
└── metadata.db            # Local SQLite database for Argon metadata (auto-generated)

python-packages/           # Installed via pip
└── argonctl/             # The Argon CLI package
    ├── __init__.py
    ├── cli/              # Command Line Interface (CLI) tool
    │   └── main.py       # Main script for the CLI
    ├── core/             # Core logic for Argon
    │   ├── __init__.py
    │   ├── branch_manager.py   # Manages branch operations (create, suspend, resume, etc.)
    │   ├── docker_utils.py     # Utilities for Docker interaction
    │   ├── metadata.py         # Handles metadata storage (SQLite)
    │   └── s3_utils.py        # Utilities for AWS S3 interaction
    └── dashboard/         # Minimal web dashboard
        ├── app.py         # Flask application for the dashboard
        ├── static/        # Static assets (CSS, images)
        └── templates/     # HTML templates for the dashboard
```

Your working directory just needs:
* `.env` file with your AWS S3 bucket and other configurations
* `metadata.db` which is auto-generated when you run `argonctl init`

The rest of the functionality is provided by the `argonctl` package installed via pip, which contains:
* `core/`: Contains all the backend logic for branching, Docker interactions, S3 operations, and metadata management.
* `cli/`: Houses the command-line interface that users interact with.
* `dashboard/`: Contains the files for the experimental web dashboard.

[Previous: Environment Variables](06_environment_variables.md) | [Next: Project Status](08_status.md)
