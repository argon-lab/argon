## 📁 Folder Structure

Here's a brief overview of the project's layout:

```
argon/
├── .env.example            # Example environment variables
├── .gitignore              # Specifies intentionally untracked files that Git should ignore
├── CONTRIBUTING.md         # Guidelines for contributing to Argon
├── LICENSE                 # Project license information
├── README.md               # The main README file you see on GitHub
├── requirements.txt        # Python dependencies
│
├── cli/                    # Command Line Interface (CLI) tool
│   └── main.py             # Main script for the CLI
│
├── core/                   # Core logic for Argon
│   ├── __init__.py
│   ├── branch_manager.py   # Manages branch operations (create, suspend, resume, etc.)
│   ├── docker_utils.py     # Utilities for Docker interaction
│   ├── metadata.py         # Handles metadata storage (SQLite)
│   └── s3_utils.py         # Utilities for AWS S3 interaction
│
├── dashboard/              # Minimal web dashboard
│   ├── app.py              # Flask application for the dashboard
│   ├── static/             # Static assets (CSS, images)
│   └── templates/          # HTML templates for the dashboard
│
├── data/                   # Sample data
│   └── base_dump.archive   # A sample MongoDB dump archive for quickstart
│
├── docs/                   # Documentation files
│   └── wiki/               # Detailed wiki pages
│       ├── 01_introduction.md
│       ├── 02_features.md
│       └── ... (other wiki pages)
│
└── metadata.db             # Local SQLite database for Argon metadata (auto-generated)
```

*   `core/`: Contains all the backend logic for branching, Docker interactions, S3 operations, and metadata management.
*   `cli/`: Houses the command-line interface that users interact with.
*   `dashboard/`: Contains the files for the experimental web dashboard.
*   `data/`: Provides sample data, like a base MongoDB dump, to help users get started quickly.
*   `docs/wiki/`: This is where detailed documentation and guides are stored.
