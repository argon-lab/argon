# Argon CLI Tool

import sys
import os
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))

import typer
from core import branch_manager, metadata
from rich import print, box
from rich.table import Table
import datetime

app = typer.Typer(help="Argon: Serverless, Branchable MongoDB Platform CLI")

# Project group
project_app = typer.Typer(help="Project management commands")
app.add_typer(project_app, name="project")

# Branch group
branch_app = typer.Typer(help="Branch management commands")
app.add_typer(branch_app, name="branch")

@app.callback()
def main():
    metadata.init_db()

# --- Project commands ---
# Use the project DB path from the dashboard app for consistency
PROJECTS_DB = os.path.join(os.path.dirname(__file__), '..', 'dashboard', '..', 'cli', 'argon_projects.db') # Adjusted path

def _get_db_conn():
    """Helper to get a database connection."""
    import sqlite3 # Added import sqlite3
    # Ensure the directory for the DB exists, similar to init_db in app.py
    db_dir = os.path.dirname(PROJECTS_DB)
    if not os.path.exists(db_dir):
        os.makedirs(db_dir)
    conn = sqlite3.connect(PROJECTS_DB)
    return conn

def _init_projects_db():
    """Initialize the projects table if it doesn't exist."""
    conn = _get_db_conn()
    try:
        with conn:
            conn.execute("CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY)")
    finally:
        conn.close()

_init_projects_db() # Initialize DB on CLI startup

def _get_projects():
    conn = _get_db_conn()
    try:
        c = conn.cursor()
        # Table creation is now in _init_projects_db
        c.execute("SELECT name FROM projects")
        projects = [row[0] for row in c.fetchall()]
    finally:
        conn.close()
    return projects

def _add_project(name):
    conn = _get_db_conn()
    try:
        with conn:
            # Table creation is now in _init_projects_db
            conn.execute("INSERT OR IGNORE INTO projects (name) VALUES (?)", (name,))
    finally:
        conn.close()

def _delete_project(name):
    conn = _get_db_conn()
    try:
        with conn:
            conn.execute("DELETE FROM projects WHERE name = ?", (name,))
    finally:
        conn.close()

@project_app.command("create")
def create_project(name: str):
    """Create a new Argon project."""
    _add_project(name)
    print(f"[cyan]Project created:[/cyan] {name}")

@project_app.command("list")
def list_projects():
    """List all Argon projects."""
    projects = _get_projects()
    if not projects:
        print("[yellow]No projects found.[/yellow]")
        return
    table = Table(title="Argon Projects", box=box.SIMPLE)
    table.add_column("Project Name", style="cyan")
    for p in projects:
        table.add_row(p)
    print(table)

@project_app.command("delete")
def delete_project(name: str):
    """Delete an Argon project."""
    _delete_project(name)
    print(f"[red]Project deleted:[/red] {name}")

# --- Branch commands ---
@branch_app.command("create")
def create_branch(name: str, project: str = typer.Option(..., help="Project name"), from_branch: str = typer.Option(None, "--from", help="Source branch to clone from (default: base)")):
    """Create a new branch."""
    base = f"branches/{project}/{from_branch}/dump.archive" if from_branch else None
    branch = branch_manager.create_branch(name, project, base)
    print(f"[green]Branch created:[/green] {branch}")

@branch_app.command("list")
def list_branches(project: str = typer.Option(..., help="Project name")):
    """List all branches for a project."""
    branches = branch_manager.list_branches(project)
    if not branches:
        print(f"[yellow]No branches found in project:[/yellow] {project}")
        return
    table = Table(title=f"Argon Branches ({project})", box=box.SIMPLE)
    table.add_column("Branch Name", style="cyan")
    table.add_column("Status", style="green")
    table.add_column("Port", style="magenta")
    table.add_column("Container ID", style="green")
    table.add_column("S3 Path", style="yellow")
    table.add_column("Last Active", style="white")
    for b in branches:
        last_active = b.get('last_active', '')
        if last_active:
            last_active = last_active[:19].replace('T',' ')
        table.add_row(
            b['branch_name'],
            b.get('status', 'unknown').capitalize(),
            str(b['port']),
            b['container_id'][:8] + '...',
            b['s3_path'],
            last_active
        )
    print(table)

@branch_app.command("list-versions")
def list_branch_versions(name: str = typer.Argument(..., help="Branch name"), project: str = typer.Option(..., help="Project name")):
    """List available S3 versions (snapshots) for a branch."""
    from core.metadata import get_branch_versions
    versions = get_branch_versions(name, project)
    if not versions:
        print(f"[yellow]No versions found for branch '{name}' in project '{project}'.[/yellow]")
        return
    table = Table(title=f"Versions for {name} ({project})", box=box.SIMPLE)
    table.add_column("Timestamp (UTC)", style="cyan")
    table.add_column("S3 Path", style="yellow")
    table.add_column("S3 Version ID", style="green")
    for v in versions:
        table.add_row(
            v['timestamp'].replace('T', ' ').split('.')[0], # Format for readability
            v['s3_path'],
            v['version_id']
        )
    print(table)

@branch_app.command("delete")
def delete_branch(name: str, project: str = typer.Option(..., help="Project name")):
    """Delete a branch (dump to S3, remove container)."""
    result = branch_manager.delete_branch(name, project)
    if result:
        print(f"[red]Branch deleted:[/red] {name}")
    else:
        print(f"[yellow]Branch not found or already deleted:[/yellow] {name}")

@branch_app.command("time-travel")
def time_travel(name: str, project: str = typer.Option(..., help="Project name"), from_branch: str = typer.Option(..., help="Source branch to clone from"), timestamp: str = typer.Option(..., help="ISO timestamp to restore from")):
    """Create a branch from a parent branch at a specific timestamp (point-in-time recovery)."""
    from core.metadata import get_branch_version_by_time
    from core.branch_manager import create_branch
    vinfo = get_branch_version_by_time(from_branch, project, timestamp)
    if not vinfo:
        print(f"[red]No version found for branch '{from_branch}' at or before {timestamp} in project '{project}'.[/red]")
        raise typer.Exit(1)
    print(f"[green]Restoring '{name}' from '{from_branch}' at {vinfo['timestamp']} (version: {vinfo['version_id']})[/green]")
    branch = create_branch(name, project, vinfo['s3_path'], vinfo['version_id'])
    print(f"[green]Branch created from time travel:[/green] {branch}")

@branch_app.command("suspend")
def suspend_branch(name: str, project: str = typer.Option(..., help="Project name")):
    """Suspend a branch (stop container, mark as stopped)."""
    from core.branch_manager import suspend_branch
    result = suspend_branch(name, project)
    if result:
        print(f"[yellow]Branch suspended:[/yellow] {name}")
    else:
        print(f"[red]Branch not running or not found:[/red] {name}")

@branch_app.command("resume")
def resume_branch(name: str, project: str = typer.Option(..., help="Project name")):
    """Resume a branch (start container from S3, mark as running)."""
    from core.branch_manager import resume_branch
    ok, msg = resume_branch(name, project)
    if ok:
        print(f"[green]Branch resumed:[/green] {name}")
    else:
        print(f"[red]Failed to resume branch:[/red] {name}\n[red]{msg}[/red]")

# --- Connect command ---
@app.command()
def connect(branch: str, project: str = typer.Option(..., help="Project name")):
    """Show MongoDB connection string for a branch."""
    b = next((b for b in branch_manager.list_branches(project) if b['branch_name'] == branch), None)
    if not b:
        print(f"[yellow]Branch not found:[/yellow] {branch}")
        raise typer.Exit(1)
    port = b['port']
    print(f"[bold green]mongodb://localhost:{port}/[/bold green]")

@app.command()
def projects():
    """List all projects."""
    # This command is now redundant due to project_app.command("list")
    # and uses a slightly different way to find the DB.
    # For consistency, we'll rely on _get_projects which uses the corrected PROJECTS_DB path.
    # from pathlib import Path
    # import sqlite3
    # db_path = Path(__file__).parent.parent / 'cli' / 'argon_projects.db' # This was the old path logic
    
    # Use the consistent _get_projects
    project_list = _get_projects() 
    if not project_list:
        print("[yellow]No projects found.[/yellow]")
        return
    
    table = Table(title="Argon Projects", box=box.SIMPLE)
    table.add_column("Project Name", style="cyan")
    for p in project_list: # Changed from 'projects' to 'project_list' to avoid conflict
        table.add_row(p)
    print(table)

@app.command()
def info():
    """Show Argon CLI info and quickstart help."""
    print("[bold cyan]Argon CLI[/bold cyan] — Serverless, Branchable MongoDB Platform\n")
    print("[green]Quickstart:[/green]")
    print("  [bold]argon project create <name>[/bold]   Create a new project")
    print("  [bold]argon project list[/bold]            List all projects")
    print("  [bold]argon branch create --name <branch> --project <project>[/bold]   Create a branch")
    print("  [bold]argon branch list --project <project>[/bold]                   List branches in a project")
    print("  [bold]argon branch suspend --name <branch> --project <project>[/bold] Suspend a branch")
    print("  [bold]argon branch resume --name <branch> --project <project>[/bold]  Resume a branch")
    print("  [bold]argon branch delete --name <branch> --project <project>[/bold]  Delete a branch")
    print("  [bold]argon branch time-travel ...[/bold]    Time-travel/restore a branch")
    print("\n[green]Docs:[/green] https://github.com/your-org/argon\n")

if __name__ == "__main__":
    app()
