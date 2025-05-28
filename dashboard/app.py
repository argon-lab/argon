from fastapi import FastAPI, Request, Form, status
from fastapi.responses import HTMLResponse, RedirectResponse, StreamingResponse, PlainTextResponse, JSONResponse
from fastapi.templating import Jinja2Templates
from fastapi.staticfiles import StaticFiles
from core import branch_manager, metadata
from rich import print
import uvicorn
import os
import sqlite3
import threading
import time
from datetime import datetime
from core.metadata import get_all_branches, update_branch_status, get_branch_versions # Added get_branch_versions
from core.branch_manager import suspend_branch
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
from core import branch_manager
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
                            suspend_branch(b['branch_name'], project)
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
    branches_data = branch_manager.list_branches(selected_project) if selected_project else []
    
    branches_with_versions = []
    if selected_project:
        for branch in branches_data:
            versions = get_branch_versions(branch['branch_name'], selected_project)
            branch['versions'] = versions
            branches_with_versions.append(branch)
    
    return templates.TemplateResponse("index.html", {"request": request, "branches": branches_with_versions, "projects": projects, "selected_project": selected_project})

@app.post("/select-project", response_class=HTMLResponse)
def select_project(request: Request, project: str = Form(...)):
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/create-branch", response_class=HTMLResponse)
def create_branch(request: Request, branch_name: str = Form(...), from_branch: str = Form(None), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Creating branch: {branch_name} (from: {from_branch}) in project: {project}")
    base = f"branches/{project}/{from_branch}/dump.archive" if from_branch else None
    branch_manager.create_branch(branch_name, project, base)
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/delete-branch", response_class=HTMLResponse)
def delete_branch(request: Request, branch_name: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Deleting branch: {branch_name} in project: {project}")
    branch_manager.delete_branch(branch_name, project)
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/time-travel", response_class=HTMLResponse)
def time_travel(request: Request, new_branch: str = Form(...), from_branch: str = Form(...), timestamp: str = Form(...), project: str = Form(...)):
    from core.metadata import get_branch_version_by_time
    from core.branch_manager import create_branch
    # Normalize timestamp: replace space with T if needed, and trim/parse to match DB format
    ts = timestamp.strip().replace(' ', 'T')
    # Try to match with and without fractional seconds
    vinfo = get_branch_version_by_time(from_branch, project, ts)
    if not vinfo and '.' not in ts:
        # Try with .000000 microseconds if user omitted them
        ts2 = ts + '.000000'
        vinfo = get_branch_version_by_time(from_branch, project, ts2)
    if vinfo:
        dashboard_log(f"[Dashboard] Time-travel restore: {new_branch} from {from_branch} at {ts} in project: {project}")
        create_branch(new_branch, project, vinfo['s3_path'], vinfo['version_id'])
    else:
        dashboard_log(f"[Dashboard] Time-travel failed: No version found for {from_branch} at {ts} in project: {project}")
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/suspend-branch", response_class=HTMLResponse)
def suspend_branch_route(request: Request, branch_name: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Suspending branch: {branch_name} in project: {project}")
    from core.branch_manager import suspend_branch
    suspend_branch(branch_name, project)
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)

@app.post("/resume-branch", response_class=HTMLResponse)
def resume_branch_route(request: Request, branch_name: str = Form(...), project: str = Form(...)):
    dashboard_log(f"[Dashboard] Resuming branch: {branch_name} in project: {project}")
    from core.branch_manager import resume_branch
    ok, msg = resume_branch(branch_name, project)
    if not ok:
        # Show error in dashboard (simple way: flash message or query param)
        return RedirectResponse(url=f"/?project={project}&error={msg}", status_code=status.HTTP_303_SEE_OTHER)
    return RedirectResponse(url=f"/?project={project}", status_code=status.HTTP_303_SEE_OTHER)
