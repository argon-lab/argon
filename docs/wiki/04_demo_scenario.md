##🧪 Demo Scenario: Your First Branch

Let's walk through a common workflow with Argon:

1.  **Create a project:**
    ```sh
    python3 cli/main.py project create my-first-project
    ```

2.  **Create a branch:** This will pull the `base/dump.archive` from S3 and start a new MongoDB container.
    ```sh
    python3 cli/main.py branch create dev-branch --project my-first-project
    ```

3.  **List branches:** You'll see `dev-branch` running on a specific port.
    ```sh
    python3 cli/main.py branch list --project my-first-project
    ```

4.  **Get connection string and make a change:**
    ```sh
    python3 cli/main.py connect dev-branch --project my-first-project
    # Use the output connection string with mongo shell or Compass
    # Example: mongo "mongodb://localhost:PORT" --eval 'db.test.insertOne({change: "first change"});'
    ```

5.  **Suspend the branch (creates version 1):** This snapshots its current state to S3 and stops the container.
    ```sh
    python3 cli/main.py branch suspend dev-branch --project my-first-project
    ```

6.  **Resume the branch and make another change:**
    ```sh
    python3 cli/main.py branch resume dev-branch --project my-first-project
    python3 cli/main.py connect dev-branch --project my-first-project
    # Example: mongo "mongodb://localhost:PORT" --eval 'db.test.insertOne({change: "second change"});'
    ```

7.  **Suspend the branch again (creates version 2):**
    ```sh
    python3 cli/main.py branch suspend dev-branch --project my-first-project
    ```

8.  **⏳ Time-Travel: Create a new branch from an older version:**
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
    # Example: mongo "mongodb://localhost:PORT_OF_V1" --eval 'db.test.find();' 
    # This should show {change: "first change"} but not {change: "second change"}
    ```

10. **🧹 Clean up:** Delete the branches.
    ```sh
    python3 cli/main.py branch delete dev-branch --project my-first-project
    python3 cli/main.py branch delete dev-branch-v1 --project my-first-project
    ```
