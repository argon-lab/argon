"""
DVC (Data Version Control) integration for Argon - Track data and model versions
"""

try:
    import dvc.api
    import dvc.repo
except ImportError:
    # Mock for testing without dvc installed
    dvc = None
import os
import json
import shutil
from pathlib import Path
from typing import Dict, Any, Optional, List
import logging
import subprocess

try:
    from ..core.branch import Branch
    from ..core.project import Project
except ImportError:
    # Fallback for testing
    from typing import Any
    Branch = Any
    Project = Any

logger = logging.getLogger(__name__)

class ArgonDVCIntegration:
    """
    DVC integration that synchronizes data versioning with Argon branches
    """
    
    def __init__(self, project: Project, dvc_repo_path: Optional[str] = None):
        self.project = project
        self.dvc_repo_path = dvc_repo_path or os.getcwd()
        self.dvc_repo = None
        
        # Initialize DVC repo if it doesn't exist
        self._initialize_dvc_repo()
        
    def _initialize_dvc_repo(self):
        """Initialize DVC repository if it doesn't exist"""
        try:
            self.dvc_repo = dvc.repo.Repo(self.dvc_repo_path)
        except dvc.repo.NotDvcRepoError:
            logger.info("Initializing DVC repository")
            subprocess.run(["dvc", "init"], cwd=self.dvc_repo_path, check=True)
            self.dvc_repo = dvc.repo.Repo(self.dvc_repo_path)
            
        # Create .dvcignore if it doesn't exist
        dvcignore_path = os.path.join(self.dvc_repo_path, ".dvcignore")
        if not os.path.exists(dvcignore_path):
            with open(dvcignore_path, 'w') as f:
                f.write("# DVC ignore file\n")
                f.write("*.pyc\n")
                f.write("__pycache__/\n")
                f.write(".DS_Store\n")
                
    def add_data(self, data_path: str, branch: Branch) -> str:
        """
        Add data to DVC tracking and associate with Argon branch
        
        Args:
            data_path: Path to the data file/directory
            branch: Argon branch to associate with
            
        Returns:
            The DVC file path (.dvc file)
        """
        # Add to DVC
        dvc_file = f"{data_path}.dvc"
        subprocess.run(["dvc", "add", data_path], cwd=self.dvc_repo_path, check=True)
        
        # Store DVC metadata in branch
        if "dvc_files" not in branch.metadata:
            branch.metadata["dvc_files"] = []
            
        dvc_info = {
            "data_path": data_path,
            "dvc_file": dvc_file,
            "added_at": branch.created_at.isoformat()
        }
        
        branch.metadata["dvc_files"].append(dvc_info)
        branch.save()
        
        logger.info(f"Added {data_path} to DVC tracking for branch {branch.name}")
        return dvc_file
        
    def create_pipeline(self, pipeline_config: Dict[str, Any], branch: Branch) -> str:
        """
        Create a DVC pipeline and associate with Argon branch
        
        Args:
            pipeline_config: DVC pipeline configuration
            branch: Argon branch to associate with
            
        Returns:
            Path to the dvc.yaml file
        """
        pipeline_path = os.path.join(self.dvc_repo_path, "dvc.yaml")
        
        # Load existing pipeline if it exists
        existing_pipeline = {}
        if os.path.exists(pipeline_path):
            with open(pipeline_path, 'r') as f:
                import yaml
                existing_pipeline = yaml.safe_load(f) or {}
                
        # Add stages from config
        if "stages" not in existing_pipeline:
            existing_pipeline["stages"] = {}
            
        for stage_name, stage_config in pipeline_config.get("stages", {}).items():
            # Add branch information to stage
            stage_config["desc"] = f"Stage for Argon branch {branch.name} - {stage_config.get('desc', '')}"
            existing_pipeline["stages"][stage_name] = stage_config
            
        # Write updated pipeline
        with open(pipeline_path, 'w') as f:
            import yaml
            yaml.dump(existing_pipeline, f, default_flow_style=False)
            
        # Store pipeline metadata in branch
        branch.metadata["dvc_pipeline"] = {
            "pipeline_path": pipeline_path,
            "stages": list(pipeline_config.get("stages", {}).keys()),
            "created_at": branch.created_at.isoformat()
        }
        branch.save()
        
        logger.info(f"Created DVC pipeline for branch {branch.name}")
        return pipeline_path
        
    def run_pipeline(self, stage_name: Optional[str] = None, force: bool = False) -> Dict[str, Any]:
        """
        Run DVC pipeline
        
        Args:
            stage_name: Optional specific stage to run
            force: Force run even if outputs exist
            
        Returns:
            Pipeline run results
        """
        cmd = ["dvc", "repro"]
        if stage_name:
            cmd.append(stage_name)
        if force:
            cmd.append("--force")
            
        try:
            result = subprocess.run(cmd, cwd=self.dvc_repo_path, capture_output=True, text=True, check=True)
            return {
                "success": True,
                "stdout": result.stdout,
                "stderr": result.stderr
            }
        except subprocess.CalledProcessError as e:
            return {
                "success": False,
                "stdout": e.stdout,
                "stderr": e.stderr,
                "error": str(e)
            }
            
    def get_metrics(self, branch: Branch) -> Dict[str, Any]:
        """
        Get metrics from DVC for a specific branch
        
        Args:
            branch: Argon branch
            
        Returns:
            Metrics data
        """
        try:
            # Get DVC metrics
            result = subprocess.run(
                ["dvc", "metrics", "show", "--json"],
                cwd=self.dvc_repo_path,
                capture_output=True,
                text=True,
                check=True
            )
            
            metrics = json.loads(result.stdout)
            
            # Store metrics in branch metadata
            branch.metadata["dvc_metrics"] = metrics
            branch.save()
            
            return metrics
            
        except subprocess.CalledProcessError as e:
            logger.error(f"Failed to get DVC metrics: {e}")
            return {}
            
    def compare_metrics(self, branch1: Branch, branch2: Branch) -> Dict[str, Any]:
        """
        Compare metrics between two branches
        
        Args:
            branch1: First branch
            branch2: Second branch
            
        Returns:
            Comparison results
        """
        metrics1 = branch1.metadata.get("dvc_metrics", {})
        metrics2 = branch2.metadata.get("dvc_metrics", {})
        
        comparison = {
            "branch1": {
                "name": branch1.name,
                "metrics": metrics1
            },
            "branch2": {
                "name": branch2.name,
                "metrics": metrics2
            },
            "differences": {}
        }
        
        # Find metric differences
        all_metrics = set(metrics1.keys()) | set(metrics2.keys())
        for metric in all_metrics:
            val1 = metrics1.get(metric)
            val2 = metrics2.get(metric)
            
            if val1 != val2:
                comparison["differences"][metric] = {
                    "branch1": val1,
                    "branch2": val2,
                    "change": (val2 - val1) if isinstance(val1, (int, float)) and isinstance(val2, (int, float)) else None
                }
                
        return comparison
        
    def sync_data_with_branch(self, branch: Branch, data_path: str):
        """
        Sync data state with Argon branch
        
        Args:
            branch: Argon branch
            data_path: Path to data to sync
        """
        # Check if data is DVC-tracked
        dvc_file = f"{data_path}.dvc"
        if not os.path.exists(dvc_file):
            # Add to DVC if not already tracked
            self.add_data(data_path, branch)
        else:
            # Update DVC tracking
            subprocess.run(["dvc", "add", data_path], cwd=self.dvc_repo_path, check=True)
            
    def create_branch_from_pipeline(self, pipeline_name: str, branch_name: str) -> Branch:
        """
        Create Argon branch from DVC pipeline state
        
        Args:
            pipeline_name: Name of the DVC pipeline
            branch_name: Name for the new Argon branch
            
        Returns:
            Created Argon branch
        """
        # Create new branch
        branch = self.project.create_branch(branch_name)
        
        # Get pipeline state
        try:
            result = subprocess.run(
                ["dvc", "status", pipeline_name],
                cwd=self.dvc_repo_path,
                capture_output=True,
                text=True,
                check=True
            )
            
            branch.metadata["dvc_pipeline_state"] = {
                "pipeline": pipeline_name,
                "status": result.stdout,
                "created_at": branch.created_at.isoformat()
            }
            
        except subprocess.CalledProcessError:
            logger.warning(f"Could not get pipeline status for {pipeline_name}")
            
        # Get current metrics
        metrics = self.get_metrics(branch)
        
        branch.save()
        return branch
        
    def export_data_version(self, branch: Branch, output_path: str):
        """
        Export data version information for a branch
        
        Args:
            branch: Argon branch
            output_path: Path to save export
        """
        export_data = {
            "branch": {
                "name": branch.name,
                "commit": branch.commit_hash,
                "created_at": branch.created_at.isoformat()
            },
            "dvc_files": branch.metadata.get("dvc_files", []),
            "dvc_pipeline": branch.metadata.get("dvc_pipeline", {}),
            "dvc_metrics": branch.metadata.get("dvc_metrics", {})
        }
        
        with open(output_path, 'w') as f:
            json.dump(export_data, f, indent=2, default=str)
            
        logger.info(f"Exported data version for branch {branch.name} to {output_path}")
        
    def restore_data_state(self, branch: Branch):
        """
        Restore data state from branch metadata
        
        Args:
            branch: Argon branch to restore from
        """
        dvc_files = branch.metadata.get("dvc_files", [])
        
        for dvc_info in dvc_files:
            dvc_file = dvc_info["dvc_file"]
            if os.path.exists(dvc_file):
                # Pull data from DVC
                subprocess.run(["dvc", "pull", dvc_file], cwd=self.dvc_repo_path)
                
        logger.info(f"Restored data state for branch {branch.name}")


def create_dvc_integration(project: Project, dvc_repo_path: Optional[str] = None) -> ArgonDVCIntegration:
    """
    Create a DVC integration for an Argon project
    
    Args:
        project: The Argon project
        dvc_repo_path: Optional path to DVC repository
        
    Returns:
        DVC integration instance
    """
    return ArgonDVCIntegration(project, dvc_repo_path)