"""
Argon Project - Represents a MongoDB project with branching
"""

from typing import Dict, List, Any, Optional
from datetime import datetime
import logging

from .client import ArgonClient
from .branch import Branch

logger = logging.getLogger(__name__)

class Project:
    """
    Represents an Argon project with MongoDB branching capabilities
    """
    
    def __init__(self, name: str, client: Optional[ArgonClient] = None):
        self.name = name
        self.client = client or ArgonClient()
        self._id = None
        self._branches = {}
        
        # Try to find existing project or create new one
        self._initialize()
    
    def _initialize(self):
        """Initialize project by finding existing or creating new"""
        projects = self.client.list_projects()
        
        # Look for existing project with this name
        for project in projects:
            if project.get("name") == self.name:
                self._id = project.get("id")
                logger.info(f"Found existing project: {self.name} (ID: {self._id})")
                return
        
        # Create new project if not found
        logger.info(f"Creating new project: {self.name}")
        result = self.client.create_project(self.name)
        self._id = result.get("id")
    
    @property
    def id(self) -> str:
        """Get project ID"""
        return self._id
    
    @property
    def project_id(self) -> str:
        """Alias for id"""
        return self._id
    
    def get_branch(self, name: str) -> Branch:
        """Get or create a branch"""
        if name not in self._branches:
            self._branches[name] = Branch(name, self)
        return self._branches[name]
    
    def create_branch(self, name: str, from_branch: str = "main") -> Branch:
        """Create a new branch"""
        # For now, just return a branch object - the CLI doesn't have explicit branch creation
        # The branch will be created implicitly when operations are performed on it
        branch = Branch(name, self)
        self._branches[name] = branch
        logger.info(f"Created branch: {name} in project {self.name}")
        return branch
    
    def list_branches(self) -> List[str]:
        """List all branches in this project"""
        # Get time travel info for main branch to see if project exists
        try:
            info = self.client.get_time_travel_info(self._id, "main")
            # For now, return main branch - CLI doesn't have branch listing yet
            return ["main"]
        except Exception as e:
            logger.warning(f"Could not list branches: {e}")
            return []
    
    def get_status(self) -> Dict[str, Any]:
        """Get project status"""
        return {
            "name": self.name,
            "id": self._id,
            "branches": self.list_branches(),
            "created_at": datetime.now().isoformat()
        }
    
    def delete(self):
        """Delete this project"""
        # CLI doesn't have project deletion yet
        logger.warning("Project deletion not yet implemented in CLI")
        raise NotImplementedError("Project deletion not yet available")
    
    def __repr__(self):
        return f"Project(name='{self.name}', id='{self._id}')"