Wiki Navigation
[README](../../README.md) | [Introduction & Motivation](01_introduction.md) | [Features](02_features.md) | [Quickstart Guide](03_quickstart_guide.md) | [Demo Scenario](04_demo_scenario.md) | [How it Works](05_how_it_works.md) | [Environment Variables](06_environment_variables.md) | [Folder Structure](07_folder_structure.md) | [Project Status](08_status.md) | [Contributing](09_contributing.md)

## Table of Contents
- [Prerequisites](#✅-prerequisites)
- [Install Argon](#🛠️-install-argon)
- [Set Up Environment Variables](#🔑-set-up-environment-variables)
- [Prepare a Base MongoDB Snapshot](#📦-prepare-a-base-mongodb-snapshot)
- [Initialize Argon's Metadata Database](#🏁-initialize-argons-metadata-database)

## 🚀 Quickstart Guide

Get up and running with Argon in a few simple steps:

1.  **✅ Prerequisites:**
    * Docker installed and running.
    * AWS CLI installed and configured with credentials that have S3 access.
    * Python 3.8+

2.  **🛠️ Install Argon:**
    ```sh
    pip install argonctl
    ```

3.  **🔑 Set Up Environment Variables:**
    * Create a new `.env` file in your working directory:
        ```sh
        touch .env
        ```
    * Add your AWS S3 bucket name, AWS credentials, and other configurations as needed.
    * See [Environment Variables](./06_environment_variables.md) for details on each variable.

4.  **📦 Prepare a Base MongoDB Snapshot:**
    * Argon needs an initial `dump.archive` in your S3 bucket at the path specified by `ARGON_BASE_SNAPSHOT_S3_PATH` (default: `base/dump.archive`).
    * You can create this from any MongoDB database:
        * Ensure a local MongoDB instance is running.
        * Insert some data if it's a new instance:
            ```sh
            mongo --eval 'db.test.insertOne({message: "Hello Argon Base!"});'
            ```
        * Create the dump (e.g., for a database named `test`):
            ```sh
            mongodump --archive=./base_dump.archive --gzip --db test 
            ```
        * Upload to your S3 bucket (replace `<your-s3-bucket>`):
            ```sh
            aws s3 cp ./base_dump.archive s3://<your-s3-bucket>/base/dump.archive
            ```
        * Ensure your `.env` file has `S3_BUCKET=<your-s3-bucket>` and, if different from default, `ARGON_BASE_SNAPSHOT_S3_PATH` pointing to your base snapshot.

5.  **🏁 Initialize Argon's Metadata Database:**
    ```sh
    argonctl init
    ```
    *(Running this command for the first time initializes the necessary local SQLite databases.)*

Now you're ready to explore the [Demo Scenario](./04_demo_scenario.md)!

[Previous: Features](02_features.md) | [Next: Demo Scenario](04_demo_scenario.md)
