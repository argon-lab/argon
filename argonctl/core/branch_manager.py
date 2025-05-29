from rich.console import Console  # type: ignore
from rich.text import Text  # type: ignore
from rich.panel import Panel  # type: ignore
from rich.align import Align  # type: ignore
"""
Handles branch creation, deletion, and listing.
"""
from .docker_utils import start_mongo_container, stop_mongo_container
from .s3_utils import upload_to_s3, download_from_s3, download_from_s3_versioned, delete_from_s3
from .metadata import (
    add_branch,
    remove_branch,
    get_all_branches,
    get_branch,
    init_db,
    add_branch_version,
    update_branch_status
)
from .db_utils import get_project_db_path, get_core_db_path # Import from db_utils
import os
import random
import tempfile
import subprocess
import sqlite3
from datetime import datetime
import docker

class BranchManager:
    def __init__(self):
        core_db_path = get_core_db_path() 
        init_db(core_db_path) 
        self.console = Console()
        self.client = None
        try:
            # Try connecting with default environment first
            self.client = docker.from_env()
            self.client.ping()

        except docker.errors.DockerException:
            # If default fails, try specific socket paths silently
            socket_paths_to_try = [
                'unix://var/run/docker.sock',
                f'unix://{os.path.join(os.path.expanduser("~"), ".docker", "run", "docker.sock")}'
            ]
            
            for sock_path in socket_paths_to_try:
                try:
                    self.client = docker.DockerClient(base_url=sock_path)
                    self.client.ping()
                    break
                except docker.errors.DockerException:
                    self.client = None
            
            # Only log if all connection attempts fail
            if not self.client:
                self.console.print(Text("ERROR", style="bold red").append(" Docker is not available. Docker-dependent features will be unavailable.", style="white"))

        except Exception as e_general:
            self.console.print(Text("ERROR", style="bold red").append(" Docker is not available due to an unexpected error.", style="white"))
            self.client = None

    def _get_free_port(self):
        return random.randint(30000, 40000)

    def create_branch(self, branch_name, project_name, base_s3_path=None, version_id=None):
        console = Console()
        if not self.client:
            console.print(Text("Docker client is not available. Cannot create branch.", style="bold red"))
            return False, "Docker client is not available. Cannot create branch."

        project_db_path = get_project_db_path(project_name)
        init_db(project_db_path)

        port = self._get_free_port()
        s3_path = f"projects/{project_name}/branches/{branch_name}/dump.archive"

        with tempfile.TemporaryDirectory() as tmpdir:
            dump_path = os.path.join(tmpdir, 'dump.archive')
            if base_s3_path:
                msg = Text()
                msg.append("INFO", style="bold green")
                msg.append(f" Creating branch '", style="white")
                msg.append(branch_name, style="bold")
                msg.append("' from base S3 path: ", style="white")
                msg.append(base_s3_path, style="green")
                msg.append(", version: ", style="white")
                msg.append(str(version_id if version_id else 'latest'), style="yellow")
                console.print(msg)
                try:
                    if version_id:
                        download_from_s3_versioned(base_s3_path, dump_path, version_id)
                    else:
                        download_from_s3(base_s3_path, dump_path)
                    if not os.path.exists(dump_path) or os.path.getsize(dump_path) == 0:
                        console.print(Text().append("WARN", style="bold yellow").append(f" Download from {base_s3_path} resulted in an empty/missing file. Branch will be empty.", style="white"))
                        open(dump_path, 'a').close()
                except Exception as e:
                    console.print(Text().append("ERROR", style="bold red").append(f" Failed to download from {base_s3_path}: {e}. Branch will be created empty.", style="white"))
                    open(dump_path, 'a').close()
            else:
                console.print(Text().append("INFO", style="bold green").append(f" Creating new empty branch '", style="white").append(branch_name, style="bold").append("'.", style="white"))
                open(dump_path, 'a').close()

            effective_dump_path = dump_path if os.path.exists(dump_path) and os.path.getsize(dump_path) > 0 else None
            
            try:
                container_id = start_mongo_container(branch_name, project_name, effective_dump_path, port, self.client)
            except Exception as e:
                console.print(Text().append("ERROR", style="bold red").append(f" Failed to start MongoDB container for branch '", style="white").append(branch_name, style="bold").append(f"': {e}", style="white"))
                return False, f"Failed to start MongoDB container for branch '{branch_name}': {e}"

            if not container_id:
                console.print(Text().append("ERROR", style="bold red").append(f" Failed to obtain container ID for branch '", style="white").append(branch_name, style="bold").append("'.", style="white"))
                return False, f"Failed to obtain container ID for branch '{branch_name}'."

            try:
                add_branch(branch_name, project_name, port, container_id, s3_path, status='running', db_path=project_db_path)
                msg = Text()
                msg.append("SUCCESS", style="bold green")
                msg.append(f" Branch '", style="white")
                msg.append(branch_name, style="bold")
                msg.append("' created successfully. ", style="white")
                msg.append("Container:", style="cyan")
                msg.append(f" {container_id}", style="yellow")
                msg.append(", Port:", style="magenta")
                msg.append(f" {port}", style="yellow")
                msg.append(", S3 Path:", style="green")
                msg.append(f" {s3_path}", style="white")
                console.print(msg)
                # Highlighted state update info box
                state_msg = f"Branch created: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]'."
                console.print(Panel(Align(state_msg, align='center'), title="[bold green]SUCCESS[/bold green]", border_style="green", style="white"))
                return True, {
                    'branch_name': branch_name,
                    'project_name': project_name,
                    'port': port,
                    'container_id': container_id,
                    's3_path': s3_path,
                    'status': 'running'
                }
            except Exception as e:
                console.print(Text().append("ERROR", style="bold red").append(f" Failed to add branch metadata for '", style="white").append(branch_name, style="bold").append(f"'. Cleaning up container. Error: {e}", style="white"))
                if self.client and container_id:
                    try:
                        stop_mongo_container(container_id, self.client, remove=True)
                    except Exception as cleanup_e:
                        console.print(Text().append("ERROR", style="bold red").append(f" Failed to cleanup container {container_id}: {cleanup_e}", style="white"))
                return False, f"Failed to add branch metadata: {e}"

    def snapshot_branch_to_s3(self, branch_name, project_name, container_id, s3_path_target):
        """Take a snapshot of a branch and upload it to S3."""
        console = Console()
        try:
            console.print(f"[bold green][INFO][/bold green] Attempting to snapshot branch '[bold]{branch_name}[/bold]' to S3 path '[green]{s3_path_target}[/green]' before stopping.")
            
            # Create snapshot and save locally first
            with tempfile.TemporaryDirectory() as temp_dir:
                dump_path = os.path.join(temp_dir, "dump.archive")
                
                # Run mongodump inside container
                mongodump_cmd = f"docker exec {container_id} mongodump --archive=/tmp/dump.archive --gzip"
                console.print(f"[green][INFO][/green] Running mongodump: {mongodump_cmd}")
                result = subprocess.run(mongodump_cmd, shell=True, capture_output=True, text=True)
                if result.returncode != 0:
                    console.print(f"[bold red][ERROR][/bold red] Failed to run mongodump: {result.stderr}")
                    return None

                # Copy dump file from container
                cp_cmd = f"docker cp {container_id}:/tmp/dump.archive {dump_path}"
                console.print(f"[green][INFO][/green] Running docker cp: {cp_cmd}")
                result = subprocess.run(cp_cmd, shell=True, capture_output=True, text=True)
                if result.returncode != 0:
                    console.print(f"[bold red][ERROR][/bold red] Failed to copy dump file from container: {result.stderr}")
                    return None

                if os.path.exists(dump_path):
                    file_size = os.path.getsize(dump_path)
                    console.print(f"[green][INFO][/green] Dump file created: {dump_path} ({file_size} bytes)")
                    
                    # Upload to S3
                    version_id = upload_to_s3(dump_path, s3_path_target)
                    if version_id:
                        console.print(f"[bold green][SUCCESS][/bold green] Snapshot uploaded to S3: {s3_path_target} (Version: {version_id})")
                        
                        # Record version in database
                        project_db_path = get_project_db_path(project_name)
                        add_branch_version(branch_name, project_name, s3_path_target, version_id, datetime.now().isoformat(), db_path=project_db_path)
                        
                        return version_id
                    else:
                        console.print(f"[bold red][ERROR][/bold red] Failed to upload snapshot to S3")
                        return None
                else:
                    console.print(f"[bold red][ERROR][/bold red] No dump file created at {dump_path}")
                    return None

        except Exception as e:
            console.print(f"[bold red][ERROR][/bold red] Failed to take snapshot: {e}")
            return None

    def delete_branch(self, branch_name, project_name):
        console = Console()
        project_db_path = get_project_db_path(project_name)
        branch = get_branch(branch_name, project_name, db_path=project_db_path)
        
        if not self.client:
            console.print("[bold yellow][WARN][/bold yellow] Docker client not available. Proceeding with metadata removal only.")
        
        if not branch:
            return False, f"Branch '{branch_name}' not found in project '{project_name}'."
        
        container_id = branch.get('container_id')
        s3_path = branch.get('s3_path')

        if self.client and container_id:
            try:
                container = self.client.containers.get(container_id)
                if container.status == 'running':
                    console.print(f"[bold green][INFO][/bold green] Branch '[bold]{branch_name}[/bold]' is running. It will be stopped and removed without a final snapshot.")
                    stop_mongo_container(container_id, self.client, remove=True)
                else:
                    console.print(f"[bold green][INFO][/bold green] Container {container_id} for branch '[bold]{branch_name}[/bold]' was not running. Attempting removal.")
                    stop_mongo_container(container_id, self.client, remove=True)
            except docker.errors.NotFound:
                console.print(f"[bold yellow][WARN][/bold yellow] Container {container_id} not found during delete. It might have been removed manually.")
            except Exception as e:
                console.print(f"[bold yellow][WARN][/bold yellow] Error managing container {container_id} during delete: {e}. Proceeding with metadata removal.")
        elif container_id:
            console.print(f"[bold yellow][WARN][/bold yellow] Docker client not available, but container {container_id} is listed for branch {branch_name}. Cannot manage container. Please remove manually if needed.")
        else:
            console.print(f"[bold green][INFO][/bold green] No container ID found for branch {branch_name}. Skipping Docker operations for delete.")

        # Delete all versions of this branch from S3
        if s3_path:
            console.print(f"[bold green][INFO][/bold green] Deleting S3 data for branch '[bold]{branch_name}[/bold]' at path '[green]{s3_path}[/green]'")
            if delete_from_s3(s3_path):
                console.print(f"[bold green][INFO][/bold green] Successfully deleted S3 data for branch '[bold]{branch_name}[/bold]'")
            else:
                console.print(f"[bold yellow][WARN][/bold yellow] Failed to delete S3 data for branch '[bold]{branch_name}[/bold]'. Manual cleanup may be needed.")

        try:
            remove_branch(branch_name, project_name, db_path=project_db_path)
            console.print(f"[bold green][INFO][/bold green] Branch metadata removed: {branch_name} in project {project_name}")
            # Highlighted state update info box
            state_msg = f"Branch deleted: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]'."
            console.print(Panel(Align(state_msg, align='center'), title="[bold red]DELETED[/bold red]", border_style="red", style="white"))
            return True, f"Branch '{branch_name}' deleted successfully."
        except Exception as e:
            console.print(f"[bold red][ERROR][/bold red] Failed to remove branch metadata for '[bold]{branch_name}[/bold]': {e}")
            return False, f"Failed to remove branch metadata for '{branch_name}': {e}"

    def suspend_branch(self, branch_name, project_name):
        console = Console()
        if not self.client:
            console.print("[bold red][ERROR][/bold red] Docker client is not available. Cannot suspend branch.")
            return False, "Docker client is not available. Cannot suspend branch."

        project_db_path = get_project_db_path(project_name)
        
        # Ensure the project's database directory exists
        db_dir = os.path.dirname(project_db_path)
        if not os.path.exists(db_dir):
            os.makedirs(db_dir, exist_ok=True)
        
        init_db(project_db_path) # Initialize DB to ensure tables exist

        branch = get_branch(branch_name, project_name, db_path=project_db_path)
        
        if not branch:
            console.print(f"[bold red][ERROR][/bold red] Branch '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]' not found.")
            return False, f"Branch '{branch_name}' in project '{project_name}' not found."
        if branch.get('status') == 'stopped':
            console.print(f"[bold yellow][WARN][/bold yellow] Branch '[bold]{branch_name}[/bold]' is already stopped.")
            return True, f"Branch '{branch_name}' is already stopped."

        container_id = branch.get('container_id')
        s3_path_target = branch.get('s3_path')

        if not container_id:
            console.print(f"[bold yellow][WARN][/bold yellow] No container ID for branch '[bold]{branch_name}[/bold]'. Cannot take snapshot. Marking as stopped.")
            update_branch_status(branch_name, project_name, 'stopped', db_path=project_db_path)
            # Highlighted state update info box
            state_msg = f"Branch suspended: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]' (no container found)."
            console.print(Panel(Align(state_msg, align='center'), title="[bold yellow]SUSPENDED[/bold yellow]", border_style="yellow", style="white"))
            return True, f"Branch '{branch_name}' marked as stopped (no container found)."

        if not s3_path_target:
            console.print(f"[bold yellow][WARN][/bold yellow] No S3 path defined for branch '[bold]{branch_name}[/bold]'. Cannot snapshot. Stopping container only.")
            try:
                stop_mongo_container(container_id, self.client, remove=False)
                update_branch_status(branch_name, project_name, 'stopped', db_path=project_db_path)
                state_msg = f"Branch suspended: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]' (no S3 path for snapshot)."
                console.print(Panel(Align(state_msg, align='center'), title="[bold yellow]SUSPENDED[/bold yellow]", border_style="yellow", style="white"))
                return True, f"Branch '{branch_name}' stopped (no S3 path for snapshot)."
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to stop container for branch '[bold]{branch_name}[/bold]': {e}")
                return False, f"Failed to stop container for branch '{branch_name}': {e}"
        
        console.print(f"[bold green][INFO][/bold green] Attempting to snapshot branch '[bold]{branch_name}[/bold]' to S3 path '[green]{s3_path_target}[/green]' before stopping.")
        snapshot_version = self.snapshot_branch_to_s3(branch_name, project_name, container_id, s3_path_target)
        
        if snapshot_version:
            console.print(f"[bold green][INFO][/bold green] Snapshot successful (Version: [yellow]{snapshot_version}[/yellow]). Stopping container [yellow]{container_id}[/yellow].")
        else:
            console.print(f"[bold yellow][WARN][/bold yellow] Snapshot failed for branch '[bold]{branch_name}[/bold]'. Container will be stopped without a new snapshot.")

        try:
            stop_mongo_container(container_id, self.client, remove=False)
            update_branch_status(branch_name, project_name, 'stopped', db_path=project_db_path)
            status_message = f"Branch '{branch_name}' suspended."
            if snapshot_version:
                status_message += f" Snapshot version: {snapshot_version}."
            else:
                status_message += " Snapshot failed or was skipped."
            # Highlighted state update info box
            state_msg = f"Branch suspended: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]'. {status_message}"
            console.print(Panel(Align(state_msg, align='center'), title="[bold yellow]SUSPENDED[/bold yellow]", border_style="yellow", style="white"))
            return True, status_message
        except Exception as e:
            update_branch_status(branch_name, project_name, 'stopped', db_path=project_db_path)
            console.print(f"[bold red][ERROR][/bold red] Failed to stop container for branch '[bold]{branch_name}[/bold]': {e}. Status updated to 'stopped'.")
            return False, f"Failed to stop container for branch '{branch_name}': {e}. Status updated to 'stopped'."

    def resume_branch(self, branch_name, project_name):
        console = Console()
        if not self.client:
            console.print("[bold red][ERROR][/bold red] Docker client is not available. Cannot resume branch.")
            return False, "Docker client is not available. Cannot resume branch."
        project_db_path = get_project_db_path(project_name)
        
        if not os.path.exists(os.path.dirname(project_db_path)):
            console.print(f"[bold red][ERROR][/bold red] Project '[bold]{project_name}[/bold]' directory not found. Cannot resume branch.")
            return False, f"Project '{project_name}' directory not found. Cannot resume branch."
        init_db(project_db_path)

        branch = get_branch(branch_name, project_name, db_path=project_db_path)
        if not branch:
            console.print(f"[bold red][ERROR][/bold red] Branch '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]' not found.")
            return False, f"Branch '{branch_name}' in project '{project_name}' not found."

        if branch.get('status') == 'running':
            if branch.get('container_id'):
                try:
                    container = self.client.containers.get(branch['container_id'])
                    if container.status == 'running':
                        console.print(f"[bold green][INFO][/bold green] Branch '[bold]{branch_name}[/bold]' is already running.")
                        # Highlighted state update info box
                        state_msg = f"Branch resumed: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]' (already running)."
                        console.print(Panel(Align(state_msg, align='center'), title="[bold green]RESUMED[/bold green]", border_style="green", style="white"))
                        return True, {
                            'message': f"Branch '{branch_name}' is already running.",
                            'branch_name': branch_name, 'project_name': project_name, 'port': branch.get('port'),
                            'container_id': branch['container_id'], 's3_path': branch.get('s3_path'), 'status': 'running'
                        }
                    else:
                        console.print(f"[bold yellow][WARN][/bold yellow] Branch '[bold]{branch_name}[/bold]' status is 'running' in DB, but container [yellow]{branch['container_id']}[/yellow] is {container.status}. Proceeding to resume.")
                except docker.errors.NotFound:
                    console.print(f"[bold yellow][WARN][/bold yellow] Branch '[bold]{branch_name}[/bold]' status is 'running' in DB, but container [yellow]{branch.get('container_id')}[/yellow] not found. Proceeding to resume.")
                except Exception as e:
                    console.print(f"[bold yellow][WARN][/bold yellow] Could not verify running status of container for branch '[bold]{branch_name}[/bold]': {e}. Proceeding to resume.")
            else:
                 console.print(f"[bold yellow][WARN][/bold yellow] Branch '[bold]{branch_name}[/bold]' status is 'running' but no container ID in DB. Proceeding to resume.")

        s3_path_to_restore_from = branch.get('s3_path')
        if not s3_path_to_restore_from:
            console.print(f"[bold yellow][WARN][/bold yellow] S3 path not found for branch '[bold]{branch_name}[/bold]'. Branch will be started fresh if possible.")

        old_container_id = branch.get('container_id')
        if self.client and old_container_id:
            try:
                console.print(f"[bold green][INFO][/bold green] Attempting to stop/remove existing container [yellow]{old_container_id}[/yellow] before resuming '[bold]{branch_name}[/bold]'.")
                stop_mongo_container(old_container_id, self.client, remove=True)
            except docker.errors.NotFound:
                console.print(f"[bold yellow][WARN][/bold yellow] Existing container [yellow]{old_container_id}[/yellow] not found, proceeding with resume.")
            except Exception as e:
                console.print(f"[bold yellow][WARN][/bold yellow] Error stopping/removing old container [yellow]{old_container_id}[/yellow]: {e}")
        
        new_port = self._get_free_port()
        new_container_id = None
        
        with tempfile.TemporaryDirectory() as tmpdir:
            dump_path = os.path.join(tmpdir, 'dump.archive')
            effective_dump_path = None

            if s3_path_to_restore_from:
                console.print(f"[bold green][INFO][/bold green] Attempting to download S3 dump from '[green]{s3_path_to_restore_from}[/green]' to '[cyan]{dump_path}[/cyan]' for branch '[bold]{branch_name}[/bold]'.")
                try:
                    download_from_s3(s3_path_to_restore_from, dump_path)
                    if os.path.exists(dump_path) and os.path.getsize(dump_path) > 0:
                        effective_dump_path = dump_path
                        console.print(f"[bold green][INFO][/bold green] S3 dump downloaded successfully from '[green]{s3_path_to_restore_from}[/green]'.")
                    else:
                        console.print(f"[bold yellow][WARN][/bold yellow] S3 dump '[green]{s3_path_to_restore_from}[/green]' resulted in an empty or missing file. Branch will be started fresh.")
                except Exception as e:
                    console.print(f"[bold red][ERROR][/bold red] Failed to download S3 dump from '[green]{s3_path_to_restore_from}[/green]': {e}. Branch will be started fresh.")
            else:
                console.print(f"[bold yellow][WARN][/bold yellow] No S3 path for branch '[bold]{branch_name}[/bold]'. Starting fresh.")

            console.print(f"[bold green][INFO][/bold green] Starting new container for branch '[bold]{branch_name}[/bold]' on port [yellow]{new_port}[/yellow].")
            try:
                new_container_id = start_mongo_container(branch_name, project_name, effective_dump_path, new_port, self.client)
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to start MongoDB container for branch '[bold]{branch_name}[/bold]': {e}")
                return False, f"Failed to start MongoDB container for branch '{branch_name}': {e}"

            if not new_container_id:
                 console.print(f"[bold red][ERROR][/bold red] Failed to obtain container ID for branch '[bold]{branch_name}[/bold]'.")
                 return False, f"Failed to obtain container ID for branch '{branch_name}'."

            try:
                from .metadata import update_branch_status
                import sqlite3
                from datetime import datetime
                conn = sqlite3.connect(project_db_path)
                c = conn.cursor()
                c.execute("""
                    UPDATE branches SET port = ?, container_id = ?, s3_path = ?, status = ?, last_active = ?
                    WHERE branch_name = ? AND project_name = ?
                """, (new_port, new_container_id, s3_path_to_restore_from, 'running', datetime.utcnow().isoformat(), branch_name, project_name))
                conn.commit()
                conn.close()
                console.print(f"[bold green][INFO][/bold green] Branch '[bold]{branch_name}[/bold]' resumed successfully. Container ID: [yellow]{new_container_id}[/yellow], Port: [yellow]{new_port}[/yellow].")
                # Highlighted state update info box
                state_msg = f"Branch resumed: '[bold]{branch_name}[/bold]' in project '[bold]{project_name}[/bold]'."
                console.print(Panel(Align(state_msg, align='center'), title="[bold green]RESUMED[/bold green]", border_style="green", style="white"))
                return True, {
                    'branch_name': branch_name,
                    'project_name': project_name,
                    'port': new_port,
                    'container_id': new_container_id,
                    's3_path': s3_path_to_restore_from,
                    'status': 'running'
                }
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to update branch metadata for '[bold]{branch_name}[/bold]' after starting container. Attempting cleanup. Error: {e}")
                if self.client and new_container_id:
                    try:
                        stop_mongo_container(new_container_id, self.client, remove=True)
                    except Exception as cleanup_e:
                        console.print(f"[bold red][ERROR][/bold red] Failed to cleanup container [yellow]{new_container_id}[/yellow] after DB error: {cleanup_e}")
                return False, f"Failed to update branch metadata for '{branch_name}': {e}"

    def get_branch_status(self, branch_name, project_name):
        project_db_path = get_project_db_path(project_name)
        branch = get_branch(branch_name, project_name, db_path=project_db_path)
        
        if not branch:
            return None

        status_info = {
            'branch_name': branch_name,
            'project_name': project_name,
            'metadata_status': branch.get('status'),
            'port': branch.get('port'),
            'container_id': branch.get('container_id'),
            's3_path': branch.get('s3_path'),
            'container_actual_status': 'unknown',
            'last_snapshotted_version': None # Placeholder, can be enhanced later
        }
        
        if self.client and branch.get('container_id'):
            try:
                container = self.client.containers.get(branch['container_id'])
                status_info['container_actual_status'] = container.status
            except docker.errors.NotFound:
                status_info['container_actual_status'] = 'not_found'
            except Exception as e:
                print(f"[WARN] Could not get actual container status for {branch['container_id']}: {e}")
                status_info['container_actual_status'] = 'error_checking_status'
        elif not self.client:
            status_info['container_actual_status'] = 'docker_client_unavailable'
            
        return status_info

    def get_branch_info(self, project_name, branch_name):
        """Return metadata for a single branch in a project."""
        project_db_path = get_project_db_path(project_name)
        branch = get_branch(branch_name, project_name, db_path=project_db_path)
        return branch

    def list_branches(self, project_name):
        project_db_path = get_project_db_path(project_name)
        # Ensure the project's database directory exists and init db if needed
        db_dir = os.path.dirname(project_db_path)
        if not os.path.exists(db_dir):
            os.makedirs(db_dir, exist_ok=True)
            init_db(project_db_path) # Initialize DB if directory was just created
        elif not os.path.exists(project_db_path):
            init_db(project_db_path) # Initialize DB if file doesn't exist in existing dir

        branches = get_all_branches(project_name, db_path=project_db_path)
        return branches

    def get_all_branch_versions(self, branch_name, project_name):
        project_db_path = get_project_db_path(project_name)
        # Ensure the project's database directory exists and init db if needed
        db_dir = os.path.dirname(project_db_path)
        if not os.path.exists(db_dir):
            os.makedirs(db_dir, exist_ok=True)
            init_db(project_db_path)
        elif not os.path.exists(project_db_path):
            init_db(project_db_path)
            
        from .metadata import get_branch_versions # Local import to avoid circular dependency if any
        versions = get_branch_versions(branch_name, project_name, db_path=project_db_path)
        return versions

    def time_travel_branch(self, branch_name, project_name, version_id):
        console = Console()
        project_db_path = get_project_db_path(project_name)
        init_db(project_db_path)

        branch = get_branch(branch_name, project_name, db_path=project_db_path)
        if not branch:
            console.print(f"[bold red][ERROR][/bold red] Branch \'[bold]{branch_name}[/bold]\' in project \'[bold]{project_name}[/bold]\' not found for time travel.")
            return False, f"Branch \'{branch_name}\' not found."

        # Find the specific version from metadata
        from .metadata import get_branch_versions # Local import
        versions = get_branch_versions(branch_name, project_name, db_path=project_db_path)
        target_version_info = next((v for v in versions if v['version_id'] == version_id), None)

        if not target_version_info:
            console.print(f"[bold red][ERROR][/bold red] Version ID \'[yellow]{version_id}[/yellow]\' not found for branch \'[bold]{branch_name}[/bold]\'.")
            return False, f"Version ID \'{version_id}\' not found."

        s3_path_to_restore = target_version_info['s3_path']
        actual_version_id_to_restore = target_version_info['version_id'] # This is the S3 version ID

        console.print(f"[bold green][INFO][/bold green] Initiating time travel for branch \'[bold]{branch_name}[/bold]\' to version \'[yellow]{version_id}[/yellow]\' (S3 Version: \'[yellow]{actual_version_id_to_restore}[/yellow]\').")

        # 1. Stop and remove current container if running
        current_container_id = branch.get('container_id')
        if self.client and current_container_id:
            try:
                container = self.client.containers.get(current_container_id)
                if container.status == 'running':
                    console.print(f"[bold green][INFO][/bold green] Stopping current container [yellow]{current_container_id}[/yellow] for branch \'[bold]{branch_name}[/bold]\'.")
                    self.snapshot_branch_to_s3(branch_name, project_name, current_container_id, branch['s3_path']) # Snapshot before stopping
                    stop_mongo_container(current_container_id, self.client, remove=True)
                else:
                    stop_mongo_container(current_container_id, self.client, remove=True) # Remove if not running
            except docker.errors.NotFound:
                console.print(f"[bold yellow][WARN][/bold yellow] Container [yellow]{current_container_id}[/yellow] not found. Proceeding with time travel restore.")
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to stop/remove current container [yellow]{current_container_id}[/yellow]: {e}. Aborting time travel.")
                return False, f"Failed to stop/remove current container: {e}"
        
        # 2. Download the specific version from S3
        new_port = self._get_free_port()
        new_container_id = None

        with tempfile.TemporaryDirectory() as tmpdir:
            dump_path = os.path.join(tmpdir, 'dump.archive')
            effective_dump_path = None
            console.print(f"[bold green][INFO][/bold green] Downloading version \'[yellow]{actual_version_id_to_restore}[/yellow]\' from S3 path \'[green]{s3_path_to_restore}[/green]\'.")
            try:
                download_from_s3_versioned(s3_path_to_restore, dump_path, actual_version_id_to_restore)
                if os.path.exists(dump_path) and os.path.getsize(dump_path) > 0:
                    effective_dump_path = dump_path
                    console.print(f"[bold green][INFO][/bold green] Successfully downloaded version \'[yellow]{actual_version_id_to_restore}[/yellow]\' ({os.path.getsize(dump_path)} bytes).")
                else:
                    console.print(f"[bold red][ERROR][/bold red] Downloaded file is empty or missing for version \'[yellow]{actual_version_id_to_restore}[/yellow]\'.")
                    return False, "Downloaded file is empty or missing."
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to download version \'[yellow]{actual_version_id_to_restore}[/yellow]\': {e}")
                return False, f"Failed to download version: {e}"

            # 3. Start a new container with the downloaded dump
            console.print(f"[bold green][INFO][/bold green] Starting new container for branch \'[bold]{branch_name}[/bold]\' (time travel) on port [yellow]{new_port}[/yellow].")
            try:
                new_container_id = start_mongo_container(branch_name, project_name, effective_dump_path, new_port, self.client)
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to start container with version \'[yellow]{actual_version_id_to_restore}[/yellow]\': {e}")
                return False, f"Failed to start container: {e}"

            if not new_container_id:
                console.print(f"[bold red][ERROR][/bold red] Failed to obtain container ID for branch '[bold]{branch_name}[/bold]'.")
                return False, f"Failed to obtain container ID for branch '{branch_name}'."

            # 4. Update branch metadata
            try:
                conn = sqlite3.connect(project_db_path)
                c = conn.cursor()
                c.execute("""
                    UPDATE branches SET port = ?, container_id = ?, status = ?, last_active = ?
                    WHERE branch_name = ? AND project_name = ?
                """, (new_port, new_container_id, 'running', datetime.utcnow().isoformat(), branch_name, project_name))
                conn.commit()
                conn.close()
                
                state_msg = f"Branch time-traveled: '[bold]{branch_name}[/bold]' to version '[yellow]{actual_version_id_to_restore}[/yellow]'."
                console.print(Panel(Align(state_msg, align='center'), title="[bold green]TIME TRAVEL COMPLETE[/bold green]", border_style="green", style="white"))
                
                return True, {
                    'branch_name': branch_name,
                    'project_name': project_name,
                    'port': new_port,
                    'container_id': new_container_id,
                    's3_path': s3_path_to_restore,
                    'version_id': actual_version_id_to_restore,
                    'status': 'running'
                }
            except Exception as e:
                console.print(f"[bold red][ERROR][/bold red] Failed to update branch metadata after time travel: {e}. Cleaning up.")
                if self.client and new_container_id:
                    try:
                        stop_mongo_container(new_container_id, self.client, remove=True)
                    except Exception as cleanup_e:
                        console.print(f"[bold red][ERROR][/bold red] Failed to cleanup container after metadata error: {cleanup_e}")
                return False, f"Failed to update branch metadata: {e}"
