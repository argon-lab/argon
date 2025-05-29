# Argon CLI Tool (Refactored)
import os
import sys
import sqlite3
from dotenv import load_dotenv
import typer
from rich.console import Console
from rich.table import Table
from rich import box
from rich.panel import Panel

# --- Constants & Globals ---
ARGON_USER_DIR = os.path.expanduser("~/.argonctl")
CLI_PROJECTS_DB_NAME = "argon_cli_projects.db"
console = Console()

# --- Utility Functions ---
def ensure_user_dir():
    os.makedirs(ARGON_USER_DIR, exist_ok=True)

def print_quickstart():
    console.print("""
[green]Quickstart:[/green]
    [bold]argonctl project create <n>[/bold]   Create a new project
    [bold]argonctl project list[/bold]            List all projects
    [bold]argonctl branch create <branch_name> --project <project_name>[/bold]   Create a branch
    [bold]argonctl branch list --project <project_name>[/bold]                   List branches in a project
    [bold]argonctl branch suspend <branch_name> --project <project_name>[/bold] Suspend a branch
    [bold]argonctl branch resume <branch_name> --project <project_name>[/bold]  Resume a branch
    [bold]argonctl branch time-travel <new_branch_name> --project <project_name> --from-branch <source_branch> --timestamp <YYYY-MM-DDTHH:MM:SS>[/bold]  Time-travel to a previous state
""")

def load_environment():
    dotenv_path_cwd = os.path.join(os.getcwd(), ".env")
    dotenv_path_user = os.path.join(ARGON_USER_DIR, ".env")
    if os.path.exists(dotenv_path_cwd):
        load_dotenv(dotenv_path_cwd)
    elif os.path.exists(dotenv_path_user):
        load_dotenv(dotenv_path_user)
    return dotenv_path_cwd, dotenv_path_user

def get_cli_projects_db_path():
    return os.path.join(ARGON_USER_DIR, CLI_PROJECTS_DB_NAME)

def get_cli_db_conn():
    return sqlite3.connect(get_cli_projects_db_path())

def init_cli_projects_db():
    with get_cli_db_conn() as conn:
        conn.execute("CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY)")

def get_cli_projects():
    with get_cli_db_conn() as conn:
        return [row[0] for row in conn.execute("SELECT name FROM projects")]

def add_cli_project(name):
    with get_cli_db_conn() as conn:
        conn.execute("INSERT OR IGNORE INTO projects (name) VALUES (?)", (name,))

def delete_cli_project(name):
    with get_cli_db_conn() as conn:
        conn.execute("DELETE FROM projects WHERE name = ?", (name,))

def check_project_exists(project_name):
    if project_name not in get_cli_projects():
        console.print(f"[red]Project '{project_name}' is not registered with the CLI. "
                      f"Use 'argonctl project create {project_name}' first.[/red]")
        raise typer.Exit(code=1)

# --- Environment Setup ---
ensure_user_dir()  # This is needed early for proper initialization
dotenv_path_cwd, dotenv_path_user = load_environment()

# --- Core Imports ---
try:
    from argonctl.core.branch_manager import BranchManager
except ImportError as e:
    console.print(f"[red]Error importing core modules: {e}[/red]\n"
                  "[yellow]Ensure Argon is installed correctly and core modules are accessible.[/yellow]\n"
                  "[yellow]If running from source, ensure you are in the project root or have set PYTHONPATH.[/yellow]")
    sys.exit(1)

from argonctl.core.metadata import init_db, DB_PATH

from argonctl.core.setup_utils import check_environment

# --- Config from Env ---
ARGON_MAIN_DB_NAME = os.getenv("DB_NAME", "argon.db")
ARGON_DOCS_URL = os.getenv("ARGON_DOCS_URL", "https://github.com/argon-lab/argon")

# --- Typer App Initialization ---
app = typer.Typer(
    help="""🚀 argonctl: Serverless, Branchable MongoDB Platform CLI

[green]Quickstart:[/green]
    [bold]argonctl project create <branch_name>[/bold]                                 Create a new project
    [bold]argonctl project list[/bold]                                                 List all projects
    [bold]argonctl branch create <branch_name> --project <project_name>[/bold]         Create a branch
    [bold]argonctl branch list --project <project_name>[/bold]                         List branches in a project
    [bold]argonctl branch suspend <branch_name> --project <project_name>[/bold]        Suspend a branch
    [bold]argonctl branch resume <branch_name> --project <project_name>[/bold]         Resume a branch
    [bold]argonctl branch time-travel <new_branch_name> --project <project_name> --from-branch <source_branch> --timestamp <YYYY-MM-DDTHH:MM:SS>[/bold]
                                                                        Time-travel to a previous state
""", rich_markup_mode="rich")
branch_manager = BranchManager()

# --- Typer App Callback & Entry Point ---
@app.callback(invoke_without_command=True)
def main_callback(ctx: typer.Context):
    """Initialize CLI prerequisites."""
    ensure_user_dir()
    init_cli_projects_db()
    init_db(DB_PATH)
    
    # Check if this is a help command or version flag
    help_commands = {'--help', '-h', '--version', '-v'}
    if not any(arg in help_commands for arg in sys.argv[1:]):
        # For non-help commands, ensure environment is set up
        # Only validate AWS credentials for commands that need them
        needs_aws = {'branch', 'snapshot'}  # Commands that require AWS
        skip_aws = ctx.invoked_subcommand not in needs_aws
        check_environment(skip_aws_validation=skip_aws)

    # Only show quickstart for the root command
    if ctx.invoked_subcommand is None:
        print_quickstart()

# --- Project Commands ---
project_app = typer.Typer(help="Project management commands (tracks projects known to CLI)")
app.add_typer(project_app, name="project")

@project_app.command("create")
def create_project(name: str):
    add_cli_project(name)
    console.print(f"[cyan]Project '{name}' registered with CLI.[/cyan]")

@project_app.command("list")
def list_projects_cmd():
    projects_list = get_cli_projects()
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
    delete_cli_project(name)
    console.print(f"[red]Project '{name}' deregistered from CLI.[/red]")

# --- Branch Commands ---
branch_app = typer.Typer(help="Branch management commands")
app.add_typer(branch_app, name="branch")

@branch_app.command("create")
def create_branch(
    name: str = typer.Argument(..., help="Name for the new branch"),
    project: str = typer.Option(..., help="Project name"),
    from_branch: str = typer.Option(None, "--from", help="Source branch to clone from (default: base, creates empty)")
):
    check_project_exists(project)
    base_s3_path = f"branches/{project}/{from_branch}/dump.archive" if from_branch else None
    try:
        success, result = branch_manager.create_branch(name, project, base_s3_path)
        if success and isinstance(result, dict):
            console.print(f"[green]Branch created:[/green] '{result.get('branch_name', name)}' in project '{project}'.")
        else:
            console.print(f"[red]Error creating branch '{name}' in project '{project}': {result}[/red]")
            raise typer.Exit(code=1)
    except Exception as e:
        console.print(f"[red]Error creating branch '{name}' in project '{project}': {e}[/red]")
        if any(x in str(e) for x in ["S3_BUCKET", "AWS credentials", "NoneType"]):
            console.print("[yellow]Hint: Ensure AWS credentials (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY) "
                          "and S3_BUCKET are correctly set in your .env file or environment.[/yellow]")
        raise typer.Exit(code=1)

@branch_app.command("list")
def list_branches(project: str = typer.Option(..., help="Project name")):
    check_project_exists(project)
    branches = branch_manager.list_branches(project)
    if not branches:
        console.print(f"[yellow]No branches found in project '{project}'.[/yellow]")
        return

    console.print(f"\n[bold cyan]Project: {project}[/bold cyan]\n")
    from argonctl.core.metadata import get_branch_versions
    from argonctl.core.db_utils import get_project_db_path
    project_db_path = get_project_db_path(project)

    for b in branches:
        branch_name = b.get('branch_name', 'N/A')
        branch_panel = Panel(
            f"[green]Status:[/green] {b.get('status', 'unknown').capitalize()}\n"
            f"[magenta]Port:[/magenta] {str(b.get('port', 'N/A'))}\n"
            f"[yellow]Last Active:[/yellow] {b.get('last_active', 'N/A')}\n"
            f"[blue]S3 Path:[/blue] {b.get('s3_path', 'N/A')}",
            title=f"[bold cyan]Branch: {branch_name}[/bold cyan]",
            border_style="cyan"
        )
        console.print(branch_panel)
        console.print()
        versions = get_branch_versions(branch_name, project, db_path=project_db_path)
        if versions:
            version_table = Table(
                title=f"[bold yellow]Version History for {branch_name}[/bold yellow]",
                box=box.ROUNDED,
                show_header=True
            )
            version_table.add_column("Version #", style="cyan", justify="right")
            version_table.add_column("Time", style="green")
            version_table.add_column("Version ID", style="magenta")
            for i, v in enumerate(versions, 1):
                version_table.add_row(str(i), v['timestamp'], v['version_id'])
            console.print(version_table)
            console.print()

@branch_app.command("delete")
def delete_branch(
    name: str = typer.Argument(..., help="Branch name to delete"),
    project: str = typer.Option(..., help="Project name")
):
    check_project_exists(project)
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
    check_project_exists(project)
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
    check_project_exists(project)
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
    check_project_exists(project)
    from argonctl.core.metadata import get_branch_version_by_time
    ts = timestamp.strip().replace(' ', 'T')
    vinfo = get_branch_version_by_time(from_branch, project, ts, db_path=None)
    if not vinfo and '.' not in ts:
        ts2 = ts + '.000000'
        vinfo = get_branch_version_by_time(from_branch, project, ts2, db_path=None)
    if not vinfo:
        from argonctl.core.db_utils import get_project_db_path
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
    check_project_exists(project)
    from argonctl.core.metadata import get_branch_versions
    from argonctl.core.db_utils import get_project_db_path
    project_db_path = get_project_db_path(project)
    versions = get_branch_versions(name, project, db_path=project_db_path)
    if not versions:
        console.print(f"[yellow]No versions found for branch '{name}' in project '{project}'.[/yellow]")
        return

    header = Panel(
        f"[cyan]Project:[/cyan] {project}\n[cyan]Branch:[/cyan] {name}",
        title="[bold]Branch Information[/bold]",
        border_style="blue"
    )
    console.print(header)
    console.print()

    version_table = Table(
        title=f"[bold yellow]Version History[/bold yellow]",
        box=box.ROUNDED,
        show_header=True
    )
    version_table.add_column("Version #", style="cyan", justify="right")
    version_table.add_column("Time", style="green")
    version_table.add_column("Version ID", style="magenta")
    version_table.add_column("S3 Path", style="blue")

    for i, v in enumerate(versions, 1):
        version_table.add_row(str(i), v['timestamp'], v['version_id'], v['s3_path'])
    console.print(version_table)

# --- Connect Command ---
@app.command()
def connect(
    branch: str = typer.Argument(..., help="Branch name to connect to"),
    project: str = typer.Option(..., help="Project name")
):
    check_project_exists(project)
    b_info = branch_manager.get_branch_info(project, branch)
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
    console.print(f"""
[bold cyan]argonctl[/bold cyan] — Serverless, Branchable MongoDB Platform

[green]Quickstart:[/green]
  [bold]argonctl project create <n>[/bold]   Create a new project
  [bold]argonctl project list[/bold]            List all projects
  [bold]argonctl branch create <branch_name> --project <project_name>[/bold]   Create a branch
  [bold]argonctl branch list --project <project_name>[/bold]                   List branches in a project
  [bold]argonctl branch suspend <branch_name> --project <project_name>[/bold] Suspend a branch
  [bold]argonctl branch resume <branch_name> --project <project_name>[/bold]  Resume a branch
  [bold]argonctl branch delete <branch_name> --project <project_name>[/bold]  Delete a branch
  [bold]argonctl branch time-travel <new_branch_name> --project <project_name> --from-branch <source_branch> --timestamp <YYYY-MM-DDTHH:MM:SS>[/bold]    Time-travel/restore a branch

[green]Docs:[/green] {ARGON_DOCS_URL}

[bold]Environment Variables (loaded from .env):[/bold]
  AWS_ACCESS_KEY_ID=YOUR_KEY_ID         # AWS Access Key ID
  AWS_SECRET_ACCESS_KEY=YOUR_ACCESS_KEY # AWS Secret Access Key
  S3_BUCKET=YOUR_BUCKET_NAME             # S3 Bucket for backups
  DB_NAME={ARGON_MAIN_DB_NAME}                   # Database name (e.g., metadata.db for metadata)
  # Add other relevant environment variables from your .env file

[bold]Configuration File Search Order:[/bold]
  1. Current Directory: {dotenv_path_cwd}
  2. Command Data Directory: {dotenv_path_user}
  If neither .env file is found, variables must be set in the environment.

[bold]Command Data Directory:[/bold]
""")
    argon_user_dir = os.path.expanduser("~/.argon")
    console.print(f"  {argon_user_dir} (for logs, default .env, etc.)")

# --- Entry point for pyproject.toml ---
def run_app():
    app()

if __name__ == "__main__":
    run_app()
