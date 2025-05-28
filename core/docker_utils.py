"""
Docker utility functions for MongoDB containers.
"""
import docker
import os
import subprocess

def start_mongo_container(branch_name, project_name, dump_path, port, docker_client):
    """Start a MongoDB container, restore from dump_path, expose on port, using provided docker_client."""
    # Use the provided docker_client (already connected and checked)
    container = docker_client.containers.run(
        'mongo:latest',
        name=f'argon-{project_name}-{branch_name}',
        ports={'27017/tcp': port},
        detach=True
    )
    import time; time.sleep(5)
    if dump_path:
        subprocess.run(['docker', 'cp', dump_path, f'{container.id}:/dump.archive'])
        restore_cmd = [
            'docker', 'exec', container.id,
            'mongorestore', '--drop', '--gzip', '--archive=/dump.archive'
        ]
        result = subprocess.run(restore_cmd, capture_output=True, text=True)
        if result.returncode != 0:
            print(f"[ERROR] mongorestore failed: {result.stderr}")
    return container.id

def stop_mongo_container(container_id, docker_client, remove=True):
    """Stop and optionally remove a MongoDB container by ID using provided docker_client."""
    try:
        container = docker_client.containers.get(container_id)
        container.stop()
        if remove:
            container.remove()
    except Exception:
        pass
