"""
Handles branch creation, deletion, and listing.
"""
from .docker_utils import start_mongo_container, stop_mongo_container
from .s3_utils import upload_to_s3, download_from_s3, download_from_s3_versioned
from .metadata import add_branch, remove_branch, get_all_branches, get_branch, init_db, add_branch_version, get_branch_version_by_time
import os
import tempfile
import random
from datetime import datetime

def get_free_port():
    # Naive random port selection (improve for production)
    return random.randint(30000, 40000)

def create_branch(branch_name, project_name, base_s3_path=None, version_id=None):
    """Create a new MongoDB branch from base or another branch, optionally from a specific S3 version."""
    init_db()
    port = get_free_port()
    s3_path = f"branches/{project_name}/{branch_name}/dump.archive"
    with tempfile.TemporaryDirectory() as tmpdir:
        dump_path = os.path.join(tmpdir, 'dump.archive')
        # Download base dump from S3
        if base_s3_path:
            if version_id:
                download_from_s3_versioned(base_s3_path, dump_path, version_id)
            else:
                download_from_s3(base_s3_path, dump_path)
        else:
            download_from_s3('base/dump.archive', dump_path)
        # Start MongoDB container and restore
        container_id = start_mongo_container(branch_name, dump_path, port)
        # Register branch
        from .metadata import add_branch
        add_branch(branch_name, project_name, port, container_id, s3_path, status='running')
    return {
        'branch_name': branch_name,
        'project_name': project_name,
        'port': port,
        'container_id': container_id,
        's3_path': s3_path,
        'status': 'running'
    }

def snapshot_branch_to_s3(branch_name, project_name, container_id, s3_path):
    """Snapshot a running MongoDB container to S3."""
    import tempfile, os, subprocess
    with tempfile.TemporaryDirectory() as tmpdir:
        dump_path = os.path.join(tmpdir, 'dump.archive')
        dump_cmd = [
            'docker', 'exec', container_id,
            'mongodump', '--archive=/dump.archive', '--gzip', '--db', 'test'
        ]
        print(f"[INFO] Running mongodump: {' '.join(dump_cmd)}")
        result = subprocess.run(dump_cmd, capture_output=True, text=True)
        print(f"[INFO] mongodump stdout: {result.stdout}")
        print(f"[INFO] mongodump stderr: {result.stderr}")
        if result.returncode != 0:
            print(f"[ERROR] mongodump failed: {result.stderr}")
        # Copy dump out
        cp_cmd = ['docker', 'cp', f'{container_id}:/dump.archive', dump_path]
        print(f"[INFO] Running docker cp: {' '.join(cp_cmd)}")
        cp_result = subprocess.run(cp_cmd, capture_output=True, text=True)
        print(f"[INFO] docker cp stdout: {cp_result.stdout}")
        print(f"[INFO] docker cp stderr: {cp_result.stderr}")
        # Check if dump file exists and is non-empty
        if os.path.exists(dump_path) and os.path.getsize(dump_path) > 0:
            print(f"[INFO] Dump file created: {dump_path} ({os.path.getsize(dump_path)} bytes)")
            version_id = upload_to_s3(dump_path, s3_path)
            from datetime import datetime
            add_branch_version(branch_name, project_name, s3_path, version_id, datetime.utcnow().isoformat())
            print(f"[INFO] Uploaded dump.archive to S3: {s3_path}")
        else:
            print(f"[ERROR] Dump file not created or empty: {dump_path}")

def delete_branch(branch_name, project_name):
    """Delete a branch: dump to S3 if running, stop/remove container, remove metadata."""
    branch = get_branch(branch_name, project_name)
    if not branch:
        print(f"[ERROR] Branch not found: {branch_name} in project {project_name}")
        return False
    port = branch['port']
    container_id = branch['container_id']
    s3_path = branch['s3_path']
    # Check if container exists and is running before dump
    import docker
    try:
        # Try the standard path first, then the Docker Desktop for Mac path
        try:
            client = docker.DockerClient(base_url='unix://var/run/docker.sock')
            client.ping() # Check if connection is alive
        except docker.errors.DockerException:
            # Fallback for Docker Desktop on Mac
            client = docker.DockerClient(base_url='unix:///Users/jakewang/.docker/run/docker.sock') # Consider using os.path.expanduser(\"~/.docker/run/docker.sock\")
            client.ping()
    except docker.errors.DockerException as e:
        print(f"[ERROR] Could not connect to Docker in branch_manager. Is Docker running and socket accessible? {e}")
        raise

    container = None
    try:
        container = client.containers.get(container_id)
        is_running = container.status == 'running'
    except Exception as e:
        print(f"[WARN] Could not get container: {e}")
        is_running = False
    if is_running:
        snapshot_branch_to_s3(branch_name, project_name, container_id, s3_path)
        stop_mongo_container(container_id)
    else:
        print(f"[WARN] Container not running, skipping dump for branch {branch_name}")
    # If not running, just remove metadata (no dump)
    remove_branch(branch_name, project_name)
    print(f"[INFO] Branch metadata removed: {branch_name} in project {project_name}")
    return True

def list_branches(project_name=None):
    """Return all branch metadata, optionally filtered by project."""
    init_db()
    from .metadata import update_branch_last_active
    branches = get_all_branches(project_name)
    # Optionally update last_active on access (for demo, not for every list)
    # for b in branches:
    #     update_branch_last_active(b['branch_name'], project_name)
    return branches

def suspend_branch(branch_name, project_name):
    """Suspend a branch: snapshot to S3, stop container, set status to stopped."""
    from .metadata import get_branch, update_branch_status
    branch = get_branch(branch_name, project_name)
    if not branch or branch['status'] == 'stopped':
        print(f"[WARN] Branch not running or not found: {branch_name}")
        return False
    container_id = branch['container_id']
    s3_path = branch['s3_path']
    # Check if container exists and is running before dump
    import docker
    try:
        # Try the standard path first, then the Docker Desktop for Mac path
        try:
            client = docker.DockerClient(base_url='unix://var/run/docker.sock')
            client.ping() # Check if connection is alive
        except docker.errors.DockerException:
            # Fallback for Docker Desktop on Mac
            client = docker.DockerClient(base_url='unix:///Users/jakewang/.docker/run/docker.sock') # Consider using os.path.expanduser(\"~/.docker/run/docker.sock\")
            client.ping()
    except docker.errors.DockerException as e:
        print(f"[ERROR] Could not connect to Docker in branch_manager. Is Docker running and socket accessible? {e}")
        raise
    try:
        container = client.containers.get(container_id)
        is_running = container.status == 'running'
    except Exception as e:
        print(f"[WARN] Could not get container: {e}")
        is_running = False
    if is_running:
        snapshot_branch_to_s3(branch_name, project_name, container_id, s3_path)
        stop_mongo_container(container_id)
    else:
        print(f"[WARN] Container not running, skipping snapshot for suspend: {branch_name}")
    update_branch_status(branch_name, project_name, 'stopped')
    print(f"[INFO] Branch suspended: {branch_name}")
    return True

def resume_branch(branch_name, project_name):
    """Resume a branch: always restore from S3, set status to running. Robust to missing containers or system restarts."""
    from .metadata import get_branch, update_branch_status
    branch = get_branch(branch_name, project_name)
    if not branch:
        return False, "Branch not found."
    if branch['status'] == 'running':
        return False, "Branch is already running."
    port = branch['port']
    s3_path = branch['s3_path']
    try:
        with tempfile.TemporaryDirectory() as tmpdir:
            dump_path = os.path.join(tmpdir, 'dump.archive')
            # Always download from S3 (cold start)
            from .s3_utils import download_from_s3
            download_from_s3(s3_path, dump_path)
            container_id = start_mongo_container(branch_name, dump_path, port)
            # Update container_id and status
            import sqlite3
            from datetime import datetime
            DB_PATH = os.path.join(os.path.dirname(__file__), '..', 'metadata.db')
            conn = sqlite3.connect(DB_PATH)
            c = conn.cursor()
            c.execute("UPDATE branches SET container_id = ?, status = ?, last_active = ? WHERE branch_name = ? AND project_name = ?", (container_id, 'running', datetime.utcnow().isoformat(), branch_name, project_name))
            conn.commit()
            conn.close()
        return True, "Branch resumed from S3."
    except Exception as e:
        return False, f"Failed to resume branch from S3: {e}"
