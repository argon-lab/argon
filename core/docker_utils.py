"""
Docker utility functions for MongoDB containers.
"""
import docker
import os
import subprocess

def start_mongo_container(branch_name, dump_path, port):
    """Start a MongoDB container, restore from dump_path, expose on port."""
    try:
        # Try the standard path first, then the Docker Desktop for Mac path
        try:
            client = docker.DockerClient(base_url='unix://var/run/docker.sock')
            client.ping() # Check if connection is alive
        except docker.errors.DockerException:
            client = docker.DockerClient(base_url='unix:///Users/jakewang/.docker/run/docker.sock') # Replace jakewang with the actual username if needed, or use os.path.expanduser("~/.docker/run/docker.sock")
            client.ping()
    except docker.errors.DockerException as e:
        print(f"[ERROR] Could not connect to Docker. Is Docker running and socket accessible? {e}")
        raise
    container = client.containers.run(
        'mongo:latest',
        name=f'argon-{branch_name}',
        ports={'27017/tcp': port},
        detach=True
    )
    # Wait for MongoDB to be ready (simple sleep, can be improved)
    import time; time.sleep(5)
    # Copy dump into container
    import subprocess
    subprocess.run(['docker', 'cp', dump_path, f'{container.id}:/dump.archive'])
    # Restore dump (must match how mongodump was created: --archive and --gzip)
    restore_cmd = [
        'docker', 'exec', container.id,
        'mongorestore', '--drop', '--gzip', '--archive=/dump.archive'
    ]
    result = subprocess.run(restore_cmd, capture_output=True, text=True)
    if result.returncode != 0:
        print(f"[ERROR] mongorestore failed: {result.stderr}")
    return container.id

def stop_mongo_container(container_id):
    """Stop and remove a MongoDB container by ID."""
    try:
        # Try the standard path first, then the Docker Desktop for Mac path
        try:
            client = docker.DockerClient(base_url='unix://var/run/docker.sock')
            client.ping()
        except docker.errors.DockerException:
            client = docker.DockerClient(base_url='unix:///Users/jakewang/.docker/run/docker.sock') # Replace jakewang with the actual username if needed, or use os.path.expanduser("~/.docker/run/docker.sock")
            client.ping()
    except docker.errors.DockerException as e:
        print(f"[ERROR] Could not connect to Docker. Is Docker running and socket accessible? {e}")
        raise
    try:
        container = client.containers.get(container_id)
        container.stop()
        container.remove()
    except Exception:
        pass
