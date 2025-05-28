"""
Metadata management using SQLite.
"""
import sqlite3
import os
from datetime import datetime

DB_PATH = os.path.join(os.path.dirname(__file__), '..', 'metadata.db')

BRANCHES_TABLE = """
CREATE TABLE IF NOT EXISTS branches (
    branch_name TEXT,
    project_name TEXT,
    port INTEGER,
    container_id TEXT,
    s3_path TEXT,
    status TEXT DEFAULT 'running',
    last_active TEXT,
    PRIMARY KEY (branch_name, project_name)
);
"""

VERSIONS_TABLE = """
CREATE TABLE IF NOT EXISTS branch_versions (
    branch_name TEXT,
    project_name TEXT,
    s3_path TEXT,
    version_id TEXT,
    timestamp TEXT,
    PRIMARY KEY (branch_name, project_name, version_id)
);
"""

def init_db():
    """Initialize the metadata database and table if not exists."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute(BRANCHES_TABLE)
    c.execute(VERSIONS_TABLE)
    conn.commit()
    conn.close()

def add_branch(branch_name, project_name, port, container_id, s3_path, status='running', last_active=None):
    """Add a branch record to the database."""
    if not last_active:
        last_active = datetime.utcnow().isoformat()
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute(
        "INSERT INTO branches (branch_name, project_name, port, container_id, s3_path, status, last_active) VALUES (?, ?, ?, ?, ?, ?, ?)",
        (branch_name, project_name, port, container_id, s3_path, status, last_active)
    )
    conn.commit()
    conn.close()

def update_branch_status(branch_name, project_name, status):
    """Update the status of a branch."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute("UPDATE branches SET status = ?, last_active = ? WHERE branch_name = ? AND project_name = ?", (status, datetime.utcnow().isoformat(), branch_name, project_name))
    conn.commit()
    conn.close()

def update_branch_last_active(branch_name, project_name):
    """Update the last_active timestamp for a branch."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute("UPDATE branches SET last_active = ? WHERE branch_name = ? AND project_name = ?", (datetime.utcnow().isoformat(), branch_name, project_name))
    conn.commit()
    conn.close()

def get_all_branches(project_name=None):
    """Return all branch records, optionally filtered by project."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    if project_name:
        c.execute("SELECT branch_name, port, container_id, s3_path, status, last_active FROM branches WHERE project_name = ?", (project_name,))
    else:
        c.execute("SELECT branch_name, port, container_id, s3_path, status, last_active FROM branches")
    rows = c.fetchall()
    conn.close()
    return [
        {
            'branch_name': row[0],
            'port': row[1],
            'container_id': row[2],
            's3_path': row[3],
            'status': row[4],
            'last_active': row[5]
        } for row in rows
    ]

def get_branch(branch_name, project_name):
    """Return metadata for a single branch."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute("SELECT branch_name, port, container_id, s3_path, status, last_active FROM branches WHERE branch_name = ? AND project_name = ?", (branch_name, project_name))
    row = c.fetchone()
    conn.close()
    if row:
        return {
            'branch_name': row[0],
            'port': row[1],
            'container_id': row[2],
            's3_path': row[3],
            'status': row[4],
            'last_active': row[5]
        }
    return None

def remove_branch(branch_name, project_name):
    """Remove a branch record from the database."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute("DELETE FROM branches WHERE branch_name = ? AND project_name = ?", (branch_name, project_name))
    # Also remove associated versions
    c.execute("DELETE FROM branch_versions WHERE branch_name = ? AND project_name = ?", (branch_name, project_name))
    conn.commit()
    conn.close()

def add_branch_version(branch_name, project_name, s3_path, version_id, timestamp):
    """Add a branch version record."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute(
        "INSERT INTO branch_versions (branch_name, project_name, s3_path, version_id, timestamp) VALUES (?, ?, ?, ?, ?)",
        (branch_name, project_name, s3_path, version_id, timestamp)
    )
    conn.commit()
    conn.close()

def get_branch_versions(branch_name, project_name):
    """Get all versions for a specific branch, ordered by timestamp descending."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute(
        "SELECT s3_path, version_id, timestamp FROM branch_versions WHERE branch_name = ? AND project_name = ? ORDER BY timestamp DESC",
        (branch_name, project_name)
    )
    rows = c.fetchall()
    conn.close()
    return [
        {
            's3_path': row[0],
            'version_id': row[1],
            'timestamp': row[2]
        } for row in rows
    ]

def get_branch_version_by_time(branch_name, project_name, timestamp_str):
    """Get the latest branch version at or before a specific timestamp."""
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute(
        "SELECT version_id, timestamp, s3_path FROM branch_versions WHERE branch_name = ? AND project_name = ? AND timestamp <= ? ORDER BY timestamp DESC LIMIT 1",
        (branch_name, project_name, timestamp_str)
    )
    row = c.fetchone()
    conn.close()
    if row:
        return {'version_id': row[0], 'timestamp': row[1], 's3_path': row[2]}
    return None
