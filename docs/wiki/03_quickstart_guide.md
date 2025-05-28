## 🚀 Quickstart Guide

Get up and running with Argon in a few simple steps:

1.  **✅ Prerequisites:**
    *   Docker installed and running.
    *   AWS CLI installed and configured with credentials that have S3 access.
    *   Python 3.8+

2.  **📥 Clone the Repository:**
    ```sh
    git clone https://github.com/jakezwang/argon.git 
    cd argon
    ```

3.  **🛠️ Install Dependencies:**
    ```sh
    pip install -r requirements.txt
    ```

4.  **🔑 Set Up Environment Variables:**
    *   Copy the provided `.env.example` file to a new file named `.env`:
        ```sh
        cp .env.example .env
        ```
    *   Edit the `.env` file to provide your AWS S3 bucket name, AWS credentials, and other configurations as needed. 
        *   See [Environment Variables](./06_environment_variables.md) for details on each variable.

5.  **📦 Prepare a Base MongoDB Snapshot:**
    *   Argon needs an initial `dump.archive` in your S3 bucket at the path specified by `ARGON_BASE_SNAPSHOT_S3_PATH` (default: `base/dump.archive`).
    *   You can create this from any MongoDB database. For a quick test, use the provided `data/base_dump.archive`:
        *   Copy the sample dump to your S3 bucket (replace `<your-s3-bucket>`):
            ```sh
            aws s3 cp data/base_dump.archive s3://<your-s3-bucket>/base/dump.archive
            ```
        *   Alternatively, to create one from scratch:
            *   Ensure a local MongoDB instance is running.
            *   Insert some data if it's a new instance:
                ```sh
                mongo --eval 'db.test.insertOne({message: "Hello Argon Base!"});'
                ```
            *   Create the dump (e.g., for a database named `test`):
                ```sh
                mongodump --archive=./base_dump.archive --gzip --db test 
                ```
            *   Upload to your S3 bucket (replace `<your-s3-bucket>`):
                ```sh
                aws s3 cp ./base_dump.archive s3://<your-s3-bucket>/base/dump.archive
                ```
        *   Ensure your `.env` file has `S3_BUCKET=<your-s3-bucket>` and, if different from default, `ARGON_BASE_SNAPSHOT_S3_PATH` pointing to your base snapshot.

6.  **🏁 Initialize Argon's Metadata Database:**
    ```sh
    python3 cli/main.py
    ```
    *(Running the CLI for the first time initializes the necessary local SQLite databases.)*

Now you're ready to explore the [Demo Scenario](./04_demo_scenario.md)!
