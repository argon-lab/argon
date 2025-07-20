"""
Argon Client - Bridge to Go CLI services
"""

import json
import subprocess
import os
import logging
from typing import Dict, List, Any, Optional
from pathlib import Path

logger = logging.getLogger(__name__)

class ArgonClient:
    """
    Client that communicates with Argon's Go services via CLI
    """
    
    def __init__(self, mongodb_uri: Optional[str] = None):
        self.mongodb_uri = mongodb_uri or os.getenv("MONGODB_URI", "mongodb://localhost:27017")
        self.cli_path = self._find_cli()
        
        # Ensure WAL is enabled
        os.environ["ENABLE_WAL"] = "true"
        if mongodb_uri:
            os.environ["MONGODB_URI"] = mongodb_uri
    
    def _find_cli(self) -> str:
        """Find the Argon CLI binary"""
        # Try different possible locations
        possible_paths = [
            os.path.join(os.path.dirname(__file__), "..", "cli", "argon-cli"),
            "argon-cli",
            "argon",
            "/usr/local/bin/argon",
            os.path.expanduser("~/.local/bin/argon")
        ]
        
        for path in possible_paths:
            if os.path.exists(path) and os.access(path, os.X_OK):
                return path
        
        raise RuntimeError("Argon CLI not found. Please ensure argon-cli is installed and in PATH")
    
    def _run_command(self, command: List[str], check_output: bool = True) -> Dict[str, Any]:
        """Run CLI command and return parsed output"""
        full_command = [self.cli_path] + command
        
        try:
            if check_output:
                result = subprocess.run(
                    full_command,
                    capture_output=True,
                    text=True,
                    check=True,
                    env={**os.environ, "ENABLE_WAL": "true"}
                )
                
                # Try to parse JSON output, fall back to text
                output = result.stdout.strip()
                if output.startswith('{') or output.startswith('['):
                    try:
                        return json.loads(output)
                    except json.JSONDecodeError:
                        pass
                
                return {"output": output, "stderr": result.stderr}
            else:
                subprocess.run(
                    full_command,
                    check=True,
                    env={**os.environ, "ENABLE_WAL": "true"}
                )
                return {"success": True}
                
        except subprocess.CalledProcessError as e:
            error_msg = f"CLI command failed: {' '.join(full_command)}\nOutput: {e.stdout}\nError: {e.stderr}"
            logger.error(error_msg)
            raise RuntimeError(error_msg)
    
    def get_status(self) -> Dict[str, Any]:
        """Get system status"""
        return self._run_command(["status"])
    
    def list_projects(self) -> List[Dict[str, Any]]:
        """List all projects"""
        result = self._run_command(["projects", "list"])
        
        # Parse the output text into structured data
        output = result.get("output", "")
        projects = []
        
        if "WAL-Enabled Projects:" in output:
            lines = output.split('\n')[1:]  # Skip header
            for line in lines:
                line = line.strip()
                if line.startswith('-'):
                    # Parse format: "- project-name (ID: id)"
                    parts = line[2:].split(' (ID: ')
                    if len(parts) == 2:
                        name = parts[0]
                        id_part = parts[1].rstrip(')')
                        projects.append({
                            "name": name,
                            "id": id_part
                        })
        
        return projects
    
    def create_project(self, name: str) -> Dict[str, Any]:
        """Create a new project"""
        result = self._run_command(["projects", "create", name])
        
        # Parse the creation output
        output = result.get("output", "")
        if "Created WAL-enabled project" in output:
            # Parse format: "Created WAL-enabled project 'name' (ID: id)"
            import re
            match = re.search(r"Created WAL-enabled project '([^']+)' \(ID: ([^)]+)\)", output)
            if match:
                return {
                    "name": match.group(1),
                    "id": match.group(2),
                    "default_branch": "main"
                }
        
        raise RuntimeError(f"Failed to parse project creation output: {output}")
    
    def get_time_travel_info(self, project_id: str, branch_name: str = "main") -> Dict[str, Any]:
        """Get time travel information for a branch"""
        result = self._run_command(["time-travel", "info", "-p", project_id, "-b", branch_name])
        
        # Parse the time travel info output
        output = result.get("output", "")
        info = {}
        
        for line in output.split('\n'):
            line = line.strip()
            if ':' in line:
                key, value = line.split(':', 1)
                key = key.strip().lower().replace(' ', '_')
                value = value.strip()
                
                # Convert numeric values
                if value.isdigit():
                    value = int(value)
                elif ' - ' in value and key == 'lsn_range':
                    # Parse LSN range "0 - 4"
                    start, end = value.split(' - ')
                    info['lsn_start'] = int(start)
                    info['lsn_end'] = int(end)
                    continue
                
                info[key] = value
        
        return info
    
    def get_restore_preview(self, project_id: str, branch_name: str, target_lsn: int) -> Dict[str, Any]:
        """Get preview of what a restore operation would do"""
        result = self._run_command([
            "restore", "preview", 
            "-p", project_id, 
            "-b", branch_name, 
            "--lsn", str(target_lsn)
        ])
        
        # Parse restore preview output
        output = result.get("output", "")
        preview = {"safe": True, "changes": []}
        
        # This would need to be enhanced based on actual CLI output format
        if "ERROR" in output or "DANGER" in output:
            preview["safe"] = False
        
        preview["output"] = output
        return preview