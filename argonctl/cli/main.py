# Argon CLI Tool
import sys
import os
import typer
from dotenv import load_dotenv
from rich.console import Console
from rich.table import Table
from rich import box
import sqlite3 # For the CLI's own project list

# --- Environment & Configuration ---
# Ensure ~/.argon directory exists for config and DBs
ARGON_USER_DIR = os.path.expanduser("~/.argon")
os.makedirs(ARGON_USER_DIR, exist_ok=True)

# Load .env file: 1st from CWD, 2nd from ~/.argon/.env
dotenv_path_cwd = os.path.join(os.getcwd(), ".env")
dotenv_path_user = os.path.join(ARGON_USER_DIR, ".env")

if os.path.exists(dotenv_path_cwd):
    load_dotenv(dotenv_path_cwd)
elif os.path.exists(dotenv_path_user):
    load_dotenv(dotenv_path_user)
# If neither exists, proceed with environment variables or defaults

# Core module imports (direct, assuming correct packaging)
# These are placed after dotenv loading so modules can use env vars at import time if they need to
try:
    from argonctl.core.project_manager import ProjectManager
    from argonctl.core.db_utils import get_core_db_path # Corrected import
    from argonctl.core.branch_manager import BranchManager
    from argonctl.core.metadata import init_db as init_core_db # Removed broken imports
    # from core.utils import ensure_argon_dir # If needed
except ImportError as e:
    print(f"Error importing core modules: {e}. Ensure Argon is installed correctly and core modules are accessible.")
    print("If running from source, ensure you are in the project root or have set PYTHONPATH.")
    sys.exit(1)


# Configuration values from environment or defaults
ARGON_MAIN_DB_NAME = os.getenv("DB_NAME", "argon.db") # Main DB for branch metadata etc.
ARGON_DOCS_URL = os.getenv("ARGON_DOCS_URL", "https://github.com/argon-lab/argon")
CLI_PROJECTS_DB_NAME = "argon_cli_projects.db" # SQLite DB for CLI's project list

# --- Typer App Initialization ---
app = typer.Typer(help="Argon: Serverless, Branchable MongoDB Platform CLI")
console = Console()

# Initialize core managers (they should use os.getenv for their config)
project_manager = ProjectManager()
branch_manager = BranchManager()

# --- CLI's Own Project List DB Management (in ~/.argon/) ---
def _get_cli_projects_db_path():
    return os.path.join(ARGON_USER_DIR, CLI_PROJECTS_DB_NAME)

def _get_cli_db_conn():
    db_path = _get_cli_projects_db_path()
    return sqlite3.connect(db_path)

def _init_cli_projects_db():
    conn = _get_cli_db_conn()
    try:
        with conn:
            conn.execute("CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY)")
    finally:
        conn.close()

def _get_cli_projects():
    conn = _get_cli_db_conn()
    try:
        c = conn.cursor()
        c.execute("SELECT name FROM projects")
        return [row[0] for row in c.fetchall()]
    finally:
        conn.close()

def _add_cli_project(name):
    conn = _get_cli_db_conn()
    try:
        with conn:
            conn.execute("INSERT OR IGNORE INTO projects (name) VALUES (?)", (name,))
    finally:
        conn.close()

def _delete_cli_project(name):
    conn = _get_cli_db_conn()
    try:
        with conn:
            conn.execute("DELETE FROM projects WHERE name = ?", (name,))
    finally:
        conn.close()

# --- Typer App Callback & Entry Point ---
@app.callback()
def main_callback():
    """Initialize CLI prerequisites."""
    # ARGON_USER_DIR is already created at the top.
    # Initialize CLI's project list DB
    _init_cli_projects_db()

    # Initialize the main Argon DB (used by BranchManager, etc.)
    # core_metadata.init_db() should handle its own connection using core_db_utils
    # and place the DB file within ARGON_USER_DIR based on ARGON_MAIN_DB_NAME.
    try:
        main_db_full_path = get_core_db_path(ARGON_MAIN_DB_NAME) # Resolves to ~/.argon/argon.db by default
        init_core_db(main_db_full_path)
    except Exception as e:
        console.print(f"[red]Error initializing main Argon database ({ARGON_MAIN_DB_NAME}): {e}[/red]")
        console.print(f"[yellow]Please check your Argon setup and environment variables.[/yellow]")
        # Depending on severity, might raise typer.Exit(1)

# --- Project Commands (operate on CLI's project list) ---
project_app = typer.Typer(help="Project management commands (tracks projects known to CLI)")
app.add_typer(project_app, name="project")

@project_app.command("create")
def create_project(name: str):
    _add_cli_project(name)
    console.print(f"[cyan]Project '{name}' registered with CLI.[/cyan]")

@project_app.command("list")
def list_projects_cmd():
    projects_list = _get_cli_projects()
    if not projects_list:
        console.print("[yellow]No projects registered with CLI.[/yellow]")
        return
    table = Table(title="Argon Projects (CLI List)", box=box.SIMPLE)
    table.add_column("Project Name", style="cyan")
    for p in projects_list:
        table.add_row(p)
    console.print(table)

@project_app.command("delete")
def delete_project_cmd(name: str):
    _delete_cli_project(name)
    console.print(f"[red]Project '{name}' deregistered from CLI.[/red]")

# --- Branch Commands (interact with BranchManager) ---
branch_app = typer.Typer(help="Branch management commands")
app.add_typer(branch_app, name="branch")

def _check_project_exists(project_name: str):
    if project_name not in _get_cli_projects():
        console.print(f"[red]Project '{project_name}' is not registered with the CLI. "
                      f"Use 'argon project create {project_name}' first.[/red]")
        raise typer.Exit(code=1)

@branch_app.command("create")
def create_branch(
    name: str = typer.Argument(..., help="Name for the new branch"),
    project: str = typer.Option(..., help="Project name"),
    from_branch: str = typer.Option(None, "--from", help="Source branch to clone from (default: base, creates empty)")
):
    _check_project_exists(project)
    base_s3_path = f"branches/{project}/{from_branch}/dump.archive" if from_branch else None
    try:
        # BranchManager should use loaded env vars (AWS keys, S3_BUCKET)
        success, result = branch_manager.create_branch(name, project, base_s3_path)
        if success and isinstance(result, dict):
            console.print(f"[green]Branch created:[/green] '{result.get('branch_name', name)}' in project '{project}'.")
        else:
            console.print(f"[red]Error creating branch '{name}' in project '{project}': {result}[/red]")
            raise typer.Exit(code=1)
    except Exception as e:
        console.print(f"[red]Error creating branch '{name}' in project '{project}': {e}[/red]")
        if "S3_BUCKET" in str(e) or "AWS credentials" in str(e) or "NoneType" in str(e): # Basic check
             console.print("[yellow]Hint: Ensure AWS credentials (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY) "
                           "and S3_BUCKET are correctly set in your .env file or environment.[/yellow]")
        raise typer.Exit(code=1)

@branch_app.command("list")
def list_branches(project: str = typer.Option(..., help="Project name")):
    _check_project_exists(project)
    branches = branch_manager.list_branches(project)
    if not branches:
        console.print(f"[yellow]No branches found in project '{project}'.[/yellow]")
        return
    table = Table(title=f"Argon Branches ({project})", box=box.SIMPLE)
    # Add columns as per your BranchManager's output structure
    table.add_column("Branch Name", style="cyan")
    table.add_column("Status", style="green")
    table.add_column("Port", style="magenta")
    # ... other columns (Container ID, S3 Path, Last Active)
    for b in branches:
        table.add_row(
            b.get('branch_name', 'N/A'),
            b.get('status', 'unknown').capitalize(),
            str(b.get('port', 'N/A')),
            # ... other data for row
        )
    console.print(table)

@branch_app.command("delete")
def delete_branch(
    name: str = typer.Argument(..., help="Branch name to delete"),
    project: str = typer.Option(..., help="Project name")
):
    _check_project_exists(project)
    try:
        success, result = branch_manager.delete_branch(name, project)
        if success:
            console.print(f"[red]Branch deleted:[/red] '{name}' from project '{project}'. {result}")
        else:
            console.print(f"[red]Error deleting branch '{name}' in project '{project}': {result}[/red]")
            raise typer.Exit(code=1)
    except Exception as e:
        console.print(f"[red]Error deleting branch '{name}': {e}[/red]")
        raise typer.Exit(code=1)

@branch_app.command("suspend")
def suspend_branch(
    name: str = typer.Argument(..., help="Branch name to suspend"),
    project: str = typer.Option(..., help="Project name")
):
    _check_project_exists(project)
    try:
        success, result = branch_manager.suspend_branch(name, project)
        if success:
            console.print(f"[yellow]Branch suspended:[/yellow] '{name}' in project '{project}'. {result}")
        else:
            console.print(f"[red]Error suspending branch '{name}' in project '{project}': {result}[/red]")
            raise typer.Exit(code=1)
    except Exception as e:
        console.print(f"[red]Error suspending branch '{name}' in project '{project}': {e}[/red]")
        raise typer.Exit(code=1)

@branch_app.command("resume")
def resume_branch(
    name: str = typer.Argument(..., help="Branch name to resume"),
    project: str = typer.Option(..., help="Project name")
):
    _check_project_exists(project)
    try:
        success, result = branch_manager.resume_branch(name, project)
        if success:
            console.print(f"[green]Branch resumed:[/green] '{name}' in project '{project}'. {result}")
        else:
            console.print(f"[red]Error resuming branch '{name}' in project '{project}': {result}[/red]")
            raise typer.Exit(code=1)
    except Exception as e:
        console.print(f"[red]Error resuming branch '{name}' in project '{project}': {e}[/red]")
        raise typer.Exit(code=1)

@branch_app.command("time-travel")
def time_travel(
    new_branch: str = typer.Argument(..., help="Name for the new branch to create (restored)"),
    project: str = typer.Option(..., help="Project name"),
    from_branch: str = typer.Option(..., help="Source branch to restore from"),
    timestamp: str = typer.Option(..., help="Restore to the snapshot at or before this ISO timestamp (e.g. 2025-05-28T00:00:00Z)")
):
    """Restore a new branch from a historical snapshot of another branch (time-travel)."""
    _check_project_exists(project)
    from argonctl.core.metadata import get_branch_version_by_time
    # Normalize timestamp: replace space with T if needed
    ts = timestamp.strip().replace(' ', 'T')
    vinfo = get_branch_version_by_time(from_branch, project, ts, db_path=None)
    if not vinfo and '.' not in ts:
        # Try with .000000 microseconds if user omitted them
        ts2 = ts + '.000000'
        vinfo = get_branch_version_by_time(from_branch, project, ts2, db_path=None)
    # Try project-specific DB if not found in default
    if not vinfo:
        from core.db_utils import get_project_db_path
        project_db_path = get_project_db_path(project)
        vinfo = get_branch_version_by_time(from_branch, project, ts, db_path=project_db_path)
        if not vinfo and '.' not in ts:
            ts2 = ts + '.000000'
            vinfo = get_branch_version_by_time(from_branch, project, ts2, db_path=project_db_path)
    if not vinfo:
        console.print(f"[red]No snapshot found for branch '{from_branch}' in project '{project}' at or before {timestamp}.[/red]")
        raise typer.Exit(code=1)
    base_s3_path = vinfo['s3_path']
    version_id = vinfo['version_id']
    try:
        success, result = branch_manager.create_branch(new_branch, project, base_s3_path, version_id)
        if success and isinstance(result, dict):
            console.print(f"[green]Time-travel branch created:[/green] '{new_branch}' in project '{project}' from '{from_branch}' at {vinfo['timestamp']} (version: {version_id})")
        else:
            console.print(f"[red]Error creating time-travel branch '{new_branch}': {result}[/red]")
            raise typer.Exit(code=1)
    except Exception as e:
        console.print(f"[red]Error during time-travel: {e}[/red]")
        raise typer.Exit(code=1)

@branch_app.command("list-versions")
def list_versions(
    name: str = typer.Argument(..., help="Branch name to list versions for"),
    project: str = typer.Option(..., help="Project name")
):
    """List all available snapshot versions for a branch."""
    _check_project_exists(project)
    from argonctl.core.metadata import get_branch_versions
    versions = get_branch_versions(name, project)
    if not versions:
        console.print(f"[yellow]No versions found for branch '{name}' in project '{project}'.[/yellow]")
        return
    table = Table(title=f"Branch Versions for {name} ({project})", box=box.SIMPLE)
    table.add_column("Timestamp", style="cyan")
    table.add_column("Version ID", style="magenta")
    table.add_column("S3 Path", style="green")
    for v in versions:
        table.add_row(v['timestamp'], v['version_id'], v['s3_path'])
    console.print(table)

# (Add other branch commands: connect using similar structure)
# Example for connect:
@app.command()
def connect(
    branch: str = typer.Argument(..., help="Branch name to connect to"),
    project: str = typer.Option(..., help="Project name")
):
    _check_project_exists(project)
    b_info = branch_manager.get_branch_info(project, branch) # Assumes BranchManager has this method
    if not b_info or b_info.get('status') != 'running':
        console.print(f"[yellow]Branch '{branch}' in project '{project}' not found or not running.[/yellow]")
        raise typer.Exit(code=1)
    port = b_info.get('port')
    if not port:
        console.print(f"[red]Could not determine port for branch '{branch}'.[/red]")
        raise typer.Exit(code=1)
    console.print(f"[bold green]mongodb://localhost:{port}/[/bold green]")


# --- Info Command ---
@app.command()
def info():
    console.print("[bold cyan]Argon CLI[/bold cyan] — Serverless, Branchable MongoDB Platform")
    console.print("[green]Quickstart:[/green]")
    console.print("  [bold]argon project create <name>[/bold]   Create a new project")
    console.print("  [bold]argon project list[/bold]            List all projects")
    console.print("  [bold]argon branch create <branch_name> --project <project_name>[/bold]   Create a branch") # Corrected syntax
    console.print("  [bold]argon branch list --project <project_name>[/bold]                   List branches in a project")
    console.print("  [bold]argon branch suspend <branch_name> --project <project_name>[/bold] Suspend a branch") # Corrected syntax
    console.print("  [bold]argon branch resume <branch_name> --project <project_name>[/bold]  Resume a branch") # Corrected syntax
    console.print("  [bold]argon branch delete <branch_name> --project <project_name>[/bold]  Delete a branch") # Corrected syntax
    console.print("  [bold]argon branch time-travel <new_branch_name> --project <project_name> --from-branch <source_branch> --timestamp <YYYY-MM-DDTHH:MM:SS>[/bold]    Time-travel/restore a branch") # Corrected syntax
    console.print(f"\n[green]Docs:[/green] {ARGON_DOCS_URL}") # Use loaded/default settings
    console.print("\n[bold]Environment Variables (loaded from .env):[/bold]")
    console.print("  AWS_ACCESS_KEY_ID=YOUR_KEY_ID         # AWS Access Key ID")
    console.print("  AWS_SECRET_ACCESS_KEY=YOUR_ACCESS_KEY # AWS Secret Access Key")
    console.print("  S3_BUCKET=YOUR_BUCKET_NAME             # S3 Bucket for backups")
    console.print(f"  DB_NAME={ARGON_MAIN_DB_NAME}                   # Database name (e.g., argon.db for metadata)")
    console.print("  # Add other relevant environment variables from your .env file")
    console.print("\n[bold]Configuration File Search Order:[/bold]")
    console.print(f"  1. Current Directory: {dotenv_path_cwd}")
    console.print(f"  2. User Argon Directory: {dotenv_path_user}")
    console.print("  If neither .env file is found, variables must be set in the environment.")
    console.print("\n[bold]Argon User Directory:[/bold]")
    argon_user_dir = os.path.expanduser("~/.argon")
    console.print(f"  {argon_user_dir} (for logs, default .env, etc.)")


# --- Entry point for pyproject.toml ---
def run_app():
    app()

if __name__ == "__main__":
    run_app()
