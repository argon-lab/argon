from fastapi import FastAPI, Request, Form, status
from fastapi.responses import HTMLResponse, RedirectResponse, StreamingResponse, PlainTextResponse, JSONResponse
from fastapi.templating import Jinja2Templates
from fastapi.staticfiles import StaticFiles
from argonctl.core import branch_manager, metadata
from argonctl.core.metadata import get_all_branches, update_branch_status, get_branch_versions, init_db # Added init_db
from argonctl.core.db_utils import get_project_db_path, get_core_db_path
from rich import print
import uvicorn
import os
import sqlite3
import threading
import time
from datetime import datetime
import queue
from dotenv import load_dotenv
import threading # Ensure threading is imported if Thread is used directly

app = FastAPI(title="Argon Dashboard")
load_dotenv() # Load .env file

# Start auto-suspend thread after all imports
# Global in-memory log queue for dashboard console
log_queue = queue.Queue(maxsize=100)

def dashboard_log(msg):
    print(msg)
    try:
        log_queue.put_nowait(f"[{datetime.utcnow().isoformat()}] {msg}")
    except queue.Full:
        log_queue.get_nowait()
        log_queue.put_nowait(f"[{datetime.utcnow().isoformat()}] {msg}")

# Patch core.branch_manager and dashboard actions to log to dashboard_log
from argonctl.core import branch_manager
branch_manager.dashboard_log = dashboard_log

def get_all_projects():
    import sqlite3, os
    PROJECTS_DB = os.path.join(os.path.dirname(__file__), '..', 'cli', 'argon_projects.db')
    if not os.path.exists(PROJECTS_DB):
        return []
    conn = sqlite3.connect(PROJECTS_DB)
    c = conn.cursor()
    c.execute("CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY)")
    c.execute("SELECT name FROM projects")
    projects = [row[0] for row in c.fetchall()]
    conn.close()
    return projects

def auto_suspend_loop(idle_minutes=10, check_interval=60):
    while True:
        now = datetime.utcnow()
        for project in get_all_projects():
            branches = get_all_branches(project)
            for b in branches:
                if b['status'] == 'running' and b['last_active']:
                    try:
                        last = datetime.fromisoformat(b['last_active'])
                        idle = (now - last).total_seconds() / 60
                        # Use DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES from env
                        configured_idle_minutes = int(os.getenv('DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES', str(idle_minutes))) # Ensure default is string for getenv
                        if idle > configured_idle_minutes:
                            dashboard_log(f"[AutoSuspend] Branch {b['branch_name']} in project {project} idle for {idle:.2f} mins (threshold: {configured_idle_minutes}). Suspending.")
                            # Create BranchManager instance for suspend operation
                            bm = branch_manager.BranchManager()
                            bm.suspend_branch(b['branch_name'], project)
                    except Exception as e:
                        dashboard_log(f"[AutoSuspend] Error processing branch {b['branch_name']} for auto-suspend: {e}")
                        pass # Keep loop running
        import time; time.sleep(check_interval) # Consider moving time import to top

# Only start auto-suspend if enabled in .env
if os.getenv('ARGON_AUTO_SUSPEND_ENABLED', 'true').lower() == 'true':
    # Ensure Thread is used from the imported threading module
    auto_suspend_thread = threading.Thread(target=auto_suspend_loop, daemon=True) # Corrected
    auto_suspend_thread.start()
    dashboard_log("[AutoSuspend] Auto-suspend feature is ENABLED.")
else:
    dashboard_log("[AutoSuspend] Auto-suspend feature is DISABLED via .env.")


templates = Jinja2Templates(directory=os.path.join(os.path.dirname(__file__), "templates"))

# Serve static files (for CSS, etc.)
static_dir = os.path.join(os.path.dirname(__file__), "static")
if not os.path.exists(static_dir):
    os.makedirs(static_dir)
app.mount("/static", StaticFiles(directory=static_dir), name="static")

# Helper to access the CLI projects DB
PROJECTS_DB = os.path.join(os.path.dirname(__file__), '..', 'cli', 'argon_projects.db')
def get_projects():
    import sqlite3
    if not os.path.exists(PROJECTS_DB):
        return []
    conn = sqlite3.connect(PROJECTS_DB)
    c = conn.cursor()
    c.execute("CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY)")
    c.execute("SELECT name FROM projects")
    projects = [row[0] for row in c.fetchall()]
    conn.close()
    return projects

def add_project(name):
    import sqlite3
    conn = sqlite3.connect(PROJECTS_DB)
    c = conn.cursor()
    c.execute("CREATE TABLE IF NOT EXISTS projects (name TEXT PRIMARY KEY)")
    c.execute("INSERT OR IGNORE INTO projects (name) VALUES (?)", (name,))
    conn.commit()
    conn.close()

def delete_project(name):
    import sqlite3
    conn = sqlite3.connect(PROJECTS_DB)
    c = conn.cursor()
    c.execute("DELETE FROM projects WHERE name = ?", (name,))
    conn.commit()
    conn.close()

@app.get("/logs")
def get_logs():
    # Always return the last N logs as JSON (no SSE)
    logs = list(log_queue.queue)
    return JSONResponse({"logs": logs})

@app.post("/create-project", response_class=HTMLResponse)
def create_project(request: Request, project_name: str = Form(...)):
    dashboard_log(f"[Dashboard] Creating project: {project_name}")
    add_project(project_name)
    return RedirectResponse(url=f"/?project={project_name}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/delete-project", response_class=HTMLResponse)
def delete_project_route(request: Request, project_name: str = Form(...)):
    dashboard_log(f"[Dashboard] Deleting project: {project_name}")
    delete_project(project_name)
    # Pick a new project if any remain
    projects = get_projects()
    next_project = projects[0] if projects else ''
    return RedirectResponse(url=f"/?project={next_project}", status_code=status.HTTP_303_SEE_OTHER)

@app.get("/", response_class=HTMLResponse)
def home(request: Request, project: str = None):
    projects = get_projects()
    selected_project = project or (projects[0] if projects else None)
    
    # Create BranchManager instance to list branches
    bm = branch_manager.BranchManager()
    branches_data = bm.list_branches(selected_project) if selected_project else []
    
    branches_with_versions = []
    if selected_project:
        project_db_path = get_project_db_path(selected_project)
        # Ensure project database exists and is initialized
        db_dir = os.path.dirname(project_db_path)
        if not os.path.exists(db_dir):
            os.makedirs(db_dir, exist_ok=True)
            init_db(project_db_path)
            dashboard_log(f"[Dashboard] Created and initialized new project database at {project_db_path}")
        elif not os.path.exists(project_db_path):
            init_db(project_db_path)
            dashboard_log(f"[Dashboard] Initialized missing project database at {project_db_path}")
            
        for branch in branches_data:
            try:
                # Get versions using the project-specific database
                versions = get_branch_versions(branch['branch_name'], selected_project, db_path=project_db_path)
                if versions:
                    dashboard_log(f"[Dashboard] Found {len(versions)} versions for branch {branch['branch_name']}")
                    # Convert version timestamps to consistent ISO format
                    for v in versions:
                        if 'timestamp' in v:
                            ts = v['timestamp'].replace(' ', 'T')
                            if not ts.endswith('Z'):
                                v['timestamp'] = ts + 'Z'
                branch['versions'] = versions
            except Exception as e:
                dashboard_log(f"[Dashboard] Error fetching versions for branch {branch['branch_name']}: {e}")
                branch['versions'] = []
            branches_with_versions.append(branch)
    
    return templates.TemplateResponse("index.html", {
        "request": request, 
        "branches": branches_with_versions, 
        "projects": projects, 
        "selected_project": selected_project
    })

@app.post("/select-project", response_class=HTMLResponse)
def select_project(request: Request, project: str = Form(...)):
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/create-branch", response_class=HTMLResponse)
def create_branch(request: Request, branch_name: str = Form(...), from_branch: str = Form(None), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Creating branch: {branch_name} (from: {from_branch}) in project: {project}")
    base = f"branches/{project}/{from_branch}/dump.archive" if from_branch else None
    bm = branch_manager.BranchManager()
    success, result = bm.create_branch(branch_name, project, base)
    if not success:
        dashboard_log(f"[Dashboard] Branch creation failed: {result}")
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/delete-branch", response_class=HTMLResponse)
def delete_branch(request: Request, branch_name: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Deleting branch: {branch_name} in project: {project}")
    bm = branch_manager.BranchManager()
    try:
        success, result = bm.delete_branch(branch_name, project)
        if success:
            dashboard_log(f"[Dashboard] Successfully deleted branch {branch_name} with all its S3 data and containers")
        else:
            error_msg = f"Failed to delete branch: {result}"
            dashboard_log(f"[Dashboard] {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
    except Exception as e:
        error_msg = f"Error deleting branch {branch_name}: {str(e)}"
        dashboard_log(f"[Dashboard] {error_msg}")
        return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/time-travel", response_class=HTMLResponse)
def time_travel(request: Request, new_branch: str = Form(...), from_branch: str = Form(...), timestamp: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Starting time-travel: Creating {new_branch} from {from_branch} at {timestamp} in project: {project}")
    try:
        # Create BranchManager instance for the operation
        bm = branch_manager.BranchManager()
        
        # First get all versions to check if any exist
        try:
            versions = bm.get_all_branch_versions(from_branch, project)
            if not versions:
                error_msg = f"No versions found for branch {from_branch} in project {project}"
                dashboard_log(f"[Dashboard] Time-travel failed: {error_msg}")
                return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        except Exception as e:
            error_msg = f"Error getting versions for branch {from_branch}: {str(e)}"
            dashboard_log(f"[Dashboard] {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        
        # Normalize timestamp with proper ISO 8601 handling
        from datetime import datetime
        try:
            # Try to parse the timestamp to ensure it's valid
            ts = timestamp.strip().replace(' ', 'T')
            if not ts.endswith('Z') and '+' not in ts and '-' not in ts[10:]:
                ts = ts + 'Z'
            # Parse and reformat to ensure consistency
            dt = datetime.fromisoformat(ts.replace('Z', '+00:00'))
            ts = dt.strftime('%Y-%m-%dT%H:%M:%S.%f')[:-3] + 'Z'  # Use milliseconds precision
            dashboard_log(f"[Dashboard] Normalized timestamp to: {ts}")
        except ValueError as e:
            error_msg = f"Invalid timestamp format: {str(e)}. Please use ISO format (YYYY-MM-DDTHH:MM:SSZ)"
            dashboard_log(f"[Dashboard] Time-travel failed: {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
            
        # Try to find the version in both default and project DB
        from argonctl.core.metadata import get_branch_version_by_time
        from argonctl.core.db_utils import get_project_db_path
        
        # Try default DB first
        vinfo = get_branch_version_by_time(from_branch, project, ts)
        if not vinfo:
            # Try project-specific DB
            project_db_path = get_project_db_path(project)
            vinfo = get_branch_version_by_time(from_branch, project, ts, db_path=project_db_path)
            dashboard_log(f"[Dashboard] Searching project DB for version at {ts}")
        
        if not vinfo:
            # Get available versions to help user understand what timestamps are valid
            all_versions = []
            try:
                all_versions = bm.get_all_branch_versions(from_branch, project)
                if all_versions:
                    # Format timestamps for display
                    version_times = [v['timestamp'] for v in sorted(all_versions, key=lambda x: x['timestamp'], reverse=True)[:5]]
                    error_msg = f"No version found for {from_branch} at {ts} in project: {project}. Available recent versions are at: {', '.join(version_times)}"
                else:
                    error_msg = f"No versions found for branch {from_branch} in project {project}. Make sure the branch exists and has at least one snapshot."
            except Exception:
                error_msg = f"No version found for {from_branch} at {ts} in project: {project}, and failed to fetch available versions."
            
            dashboard_log(f"[Dashboard] Time-travel failed: {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        
        # Validate new branch name format
        import re
        if not re.match(r'^[a-zA-Z0-9_-]+$', new_branch):
            error_msg = f"Invalid branch name '{new_branch}'. Branch names can only contain letters, numbers, hyphens, and underscores."
            dashboard_log(f"[Dashboard] Time-travel failed: {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        
        # Check if the new branch name already exists
        if bm.get_branch_info(project, new_branch):
            error_msg = f"Branch '{new_branch}' already exists in project '{project}'. Please choose a different branch name."
            dashboard_log(f"[Dashboard] Time-travel failed: {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        
        # Try to create the new branch from the found version
        dashboard_log(f"[Dashboard] Found version at {vinfo['timestamp']}, creating new branch: {new_branch}")
        try:
            success, result = bm.create_branch(new_branch, project, vinfo['s3_path'], vinfo['version_id'])
            
            if success:
                # Add detailed success message
                message = f"Successfully created branch '{new_branch}' from '{from_branch}' at {vinfo['timestamp']} (version: {vinfo['version_id']})"
                dashboard_log(f"[Dashboard] Time-travel successful: {message}")
                return RedirectResponse(url=f"/?project={project}&message={message}", status_code=status.HTTP_303_SEE_OTHER)
            else:
                error_msg = f"Failed to create branch '{new_branch}': {result}"
                if "already exists" in str(result).lower():
                    error_msg += ". Please choose a different branch name."
                elif "invalid branch name" in str(result).lower():
                    error_msg += ". Branch names can only contain letters, numbers, hyphens, and underscores."
                dashboard_log(f"[Dashboard] Time-travel failed during branch creation: {error_msg}")
                return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        except Exception as e:
            error_msg = f"Error creating branch '{new_branch}' from '{from_branch}' at {vinfo['timestamp']}: {str(e)}"
            dashboard_log(f"[Dashboard] Time-travel failed with exception: {error_msg}")
            return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
            
    except Exception as e:
        error_msg = f"Unexpected error during time-travel: {str(e)}"
        dashboard_log(f"[Dashboard] {error_msg}")
        return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/suspend-branch", response_class=HTMLResponse)
def suspend_branch_route(request: Request, branch_name: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Suspending branch: {branch_name} in project: {project}")
    
    # Initialize BranchManager and get project DB path
    bm = branch_manager.BranchManager()
    project_db_path = get_project_db_path(project)
    
    # Ensure project DB exists
    db_dir = os.path.dirname(project_db_path)
    if not os.path.exists(db_dir):
        os.makedirs(db_dir, exist_ok=True)
        init_db(project_db_path)
        dashboard_log(f"[Dashboard] Created and initialized project database for suspend operation")
    elif not os.path.exists(project_db_path):
        init_db(project_db_path)
        dashboard_log(f"[Dashboard] Initialized missing project database for suspend operation")
        
    try:
        # Get branch info first to log useful context
        versions_before = get_branch_versions(branch_name, project, db_path=project_db_path)
        dashboard_log(f"[Dashboard] Branch {branch_name} had {len(versions_before)} versions before suspend")
        
        # Perform the suspend operation
        success, result = bm.suspend_branch(branch_name, project)
        if not success:
            dashboard_log(f"[Dashboard] Failed to suspend branch: {result}")
            return RedirectResponse(url=f"/?project={project}&error={result}", status_code=status.HTTP_303_SEE_OTHER)
        
        # Check if a new version was created
        versions_after = get_branch_versions(branch_name, project, db_path=project_db_path)
        if len(versions_after) > len(versions_before):
            dashboard_log(f"[Dashboard] Successfully created version during suspend. New version count: {len(versions_after)}")
            latest = versions_after[0] if versions_after else None
            if latest:
                dashboard_log(f"[Dashboard] Latest version: {latest.get('version_id', 'unknown')} from {latest.get('timestamp', 'unknown')}")
        else:
            dashboard_log(f"[Dashboard] Warning: No new version was created during suspend operation")
            
    except Exception as e:
        error_msg = f"Error suspending branch: {branch_name} in project: {project}: {str(e)}"
        dashboard_log(f"[Dashboard] {error_msg}")
        return RedirectResponse(url=f"/?project={project}&error={error_msg}", status_code=status.HTTP_303_SEE_OTHER)
        
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/resume-branch", response_class=HTMLResponse)
def resume_branch_route(request: Request, branch_name: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Resuming branch: {branch_name} in project: {project}")
    bm = branch_manager.BranchManager()
    try:
        success, result = bm.resume_branch(branch_name, project)
        if not success:
            dashboard_log(f"[Dashboard] Failed to resume branch: {result}")
            return RedirectResponse(url=f"/?project={project}&error={result}", status_code=status.HTTP_303_SEE_OTHER)
    except Exception as e:
        dashboard_log(f"[Dashboard] Error resuming branch: {branch_name} in project: {project}: {e}")
        return RedirectResponse(url=f"/?project={project}&error={str(e)}", status_code=status.HTTP_303_SEE_OTHER)
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

def run_dashboard():
    """Entry point for running the dashboard via CLI"""
    uvicorn.run("dashboard.app:app", host="0.0.0.0", port=8000, reload=True)

if __name__ == "__main__":
    run_dashboard()
