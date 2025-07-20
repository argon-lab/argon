"""
Weights & Biases integration for Argon - Track experiments and visualize results
"""

try:
    import wandb
except ImportError:
    # Mock for testing without wandb installed
    wandb = None
import os
import json
from typing import Dict, Any, Optional, List
import logging
from datetime import datetime

try:
    from ..core.branch import Branch
    from ..core.project import Project
except ImportError:
    # Fallback for testing
    from typing import Any
    Branch = Any
    Project = Any

logger = logging.getLogger(__name__)

class ArgonWandbIntegration:
    """
    Weights & Biases integration that tracks experiments alongside Argon branches
    """
    
    def __init__(self, project: Project, wandb_project: Optional[str] = None, entity: Optional[str] = None):
        self.project = project
        self.wandb_project = wandb_project or f"argon-{project.name}"
        self.entity = entity or os.getenv("WANDB_ENTITY")
        self.current_run = None
        
        # Initialize wandb
        if not wandb.api.api_key:
            logger.warning("WANDB_API_KEY not set. Please set it to use W&B integration.")
            
    def start_run(self, branch: Branch, run_name: Optional[str] = None, config: Optional[Dict[str, Any]] = None,
                  tags: Optional[List[str]] = None, notes: Optional[str] = None) -> str:
        """
        Start a W&B run tied to an Argon branch
        
        Args:
            branch: The Argon branch for this experiment
            run_name: Optional custom run name
            config: Configuration parameters
            tags: Optional tags for the run
            notes: Optional notes for the run
            
        Returns:
            The W&B run ID
        """
        # Create run name from branch if not provided
        if not run_name:
            run_name = f"branch-{branch.name}-{branch.commit_hash[:8]}"
            
        # Prepare tags
        run_tags = tags or []
        run_tags.extend([
            f"argon-project:{self.project.name}",
            f"argon-branch:{branch.name}",
            f"argon-commit:{branch.commit_hash[:8]}"
        ])
        
        # Prepare config with Argon metadata
        run_config = config or {}
        run_config.update({
            "argon": {
                "project": self.project.name,
                "branch": branch.name,
                "commit": branch.commit_hash,
                "parent_branch": branch.parent_branch or "main",
                "created_at": branch.created_at.isoformat()
            }
        })
        
        # Get branch statistics
        stats = branch.get_stats()
        run_config["argon"]["stats"] = stats
        
        # Start W&B run
        self.current_run = wandb.init(
            project=self.wandb_project,
            entity=self.entity,
            name=run_name,
            config=run_config,
            tags=run_tags,
            notes=notes or f"Experiment for Argon branch {branch.name}"
        )
        
        # Store W&B run metadata in branch
        branch.metadata["wandb_run"] = {
            "run_id": self.current_run.id,
            "run_name": run_name,
            "project": self.wandb_project,
            "entity": self.entity,
            "url": self.current_run.url,
            "created_at": datetime.now().isoformat()
        }
        branch.save()
        
        logger.info(f"Started W&B run {self.current_run.id} for branch {branch.name}")
        return self.current_run.id
        
    def log_metrics(self, metrics: Dict[str, Any], step: Optional[int] = None, commit: Optional[bool] = None):
        """
        Log metrics to W&B
        
        Args:
            metrics: Dictionary of metric names to values
            step: Optional step number
            commit: Whether to commit the metrics
        """
        if not self.current_run:
            logger.warning("No active W&B run. Start a run first.")
            return
            
        wandb.log(metrics, step=step, commit=commit)
        
    def log_artifacts(self, artifacts: Dict[str, str], artifact_type: str = "model"):
        """
        Log artifacts to W&B
        
        Args:
            artifacts: Dictionary of artifact names to file paths
            artifact_type: Type of artifact (model, dataset, etc.)
        """
        if not self.current_run:
            logger.warning("No active W&B run. Start a run first.")
            return
            
        for name, path in artifacts.items():
            artifact = wandb.Artifact(name, type=artifact_type)
            artifact.add_file(path)
            self.current_run.log_artifact(artifact)
            
    def log_table(self, table_name: str, data: List[List[Any]], columns: List[str]):
        """
        Log a table to W&B
        
        Args:
            table_name: Name of the table
            data: Table data as list of rows
            columns: Column names
        """
        if not self.current_run:
            logger.warning("No active W&B run. Start a run first.")
            return
            
        table = wandb.Table(columns=columns, data=data)
        wandb.log({table_name: table})
        
    def log_image(self, image_name: str, image_path: str, caption: Optional[str] = None):
        """
        Log an image to W&B
        
        Args:
            image_name: Name for the image
            image_path: Path to the image file
            caption: Optional caption
        """
        if not self.current_run:
            logger.warning("No active W&B run. Start a run first.")
            return
            
        wandb.log({image_name: wandb.Image(image_path, caption=caption)})
        
    def log_model(self, model_path: str, model_name: str, metadata: Optional[Dict[str, Any]] = None):
        """
        Log a model to W&B
        
        Args:
            model_path: Path to the model file
            model_name: Name for the model
            metadata: Optional metadata
        """
        if not self.current_run:
            logger.warning("No active W&B run. Start a run first.")
            return
            
        artifact = wandb.Artifact(model_name, type="model", metadata=metadata)
        artifact.add_file(model_path)
        self.current_run.log_artifact(artifact)
        
    def create_branch_from_run(self, run_id: str, branch_name: str) -> Branch:
        """
        Create a new Argon branch from a W&B run
        
        Args:
            run_id: W&B run ID
            branch_name: Name for the new branch
            
        Returns:
            The created Argon branch
        """
        # Get run information
        api = wandb.Api()
        run = api.run(f"{self.entity}/{self.wandb_project}/{run_id}")
        
        # Get parent branch from run config
        parent_branch = run.config.get("argon", {}).get("parent_branch", "main")
        
        # Create new branch
        branch = self.project.create_branch(branch_name, parent_branch)
        
        # Store W&B run reference
        branch.metadata["wandb_run"] = {
            "run_id": run_id,
            "run_name": run.name,
            "project": self.wandb_project,
            "entity": self.entity,
            "url": run.url,
            "state": run.state,
            "created_at": run.created_at.isoformat()
        }
        
        # Store final metrics
        branch.metadata["wandb_metrics"] = dict(run.summary)
        
        branch.save()
        return branch
        
    def compare_runs(self, run_ids: List[str]) -> Dict[str, Any]:
        """
        Compare multiple W&B runs
        
        Args:
            run_ids: List of W&B run IDs to compare
            
        Returns:
            Comparison data
        """
        api = wandb.Api()
        runs = [api.run(f"{self.entity}/{self.wandb_project}/{run_id}") for run_id in run_ids]
        
        comparison = {
            "runs": [],
            "metrics": {},
            "config": {}
        }
        
        for run in runs:
            run_data = {
                "run_id": run.id,
                "run_name": run.name,
                "state": run.state,
                "argon_branch": run.config.get("argon", {}).get("branch", ""),
                "argon_commit": run.config.get("argon", {}).get("commit", ""),
                "url": run.url,
                "metrics": dict(run.summary),
                "config": dict(run.config)
            }
            comparison["runs"].append(run_data)
            
            # Aggregate metrics for comparison
            for metric_name, value in run.summary.items():
                if metric_name not in comparison["metrics"]:
                    comparison["metrics"][metric_name] = []
                comparison["metrics"][metric_name].append(value)
                
        return comparison
        
    def get_run_branches(self, run_id: str) -> List[Branch]:
        """
        Get all branches associated with a W&B run
        
        Args:
            run_id: W&B run ID
            
        Returns:
            List of associated branches
        """
        branches = []
        for branch in self.project.list_branches():
            wandb_run = branch.metadata.get("wandb_run", {})
            if wandb_run.get("run_id") == run_id:
                branches.append(branch)
        return branches
        
    def sync_branch_metrics(self, branch: Branch):
        """
        Sync branch with its W&B run metrics
        
        Args:
            branch: Argon branch to sync
        """
        wandb_run = branch.metadata.get("wandb_run", {})
        if not wandb_run:
            logger.warning(f"No W&B run associated with branch {branch.name}")
            return
            
        run_id = wandb_run["run_id"]
        
        # Get latest metrics from W&B
        api = wandb.Api()
        run = api.run(f"{self.entity}/{self.wandb_project}/{run_id}")
        
        # Update branch metadata
        branch.metadata["wandb_metrics"] = dict(run.summary)
        branch.metadata["wandb_run"]["state"] = run.state
        branch.metadata["wandb_run"]["updated_at"] = datetime.now().isoformat()
        
        branch.save()
        
    def create_report(self, branch: Branch, report_name: str, description: str = "") -> str:
        """
        Create a W&B report for a branch
        
        Args:
            branch: Argon branch
            report_name: Name for the report
            description: Optional description
            
        Returns:
            Report URL
        """
        wandb_run = branch.metadata.get("wandb_run", {})
        if not wandb_run:
            logger.warning(f"No W&B run associated with branch {branch.name}")
            return ""
            
        # Create report content
        report_content = f"""
# Argon Branch Report: {branch.name}

## Branch Information
- **Branch**: {branch.name}
- **Commit**: {branch.commit_hash}
- **Created**: {branch.created_at}
- **Parent Branch**: {branch.parent_branch or 'main'}

## Experiment Results
- **W&B Run**: [{wandb_run['run_name']}]({wandb_run['url']})
- **Run ID**: {wandb_run['run_id']}

## Metrics
{json.dumps(branch.metadata.get('wandb_metrics', {}), indent=2)}

## Description
{description}
        """
        
        # Note: W&B API doesn't support programmatic report creation yet
        # This would need to be done through the W&B UI
        logger.info(f"Report content prepared for branch {branch.name}")
        return wandb_run["url"]
        
    def finish_run(self, exit_code: int = 0):
        """
        Finish the current W&B run
        
        Args:
            exit_code: Exit code for the run
        """
        if self.current_run:
            wandb.finish(exit_code=exit_code)
            self.current_run = None
            
    def export_experiment_data(self, branch: Branch, output_path: str):
        """
        Export experiment data for a branch
        
        Args:
            branch: Argon branch
            output_path: Path to save the exported data
        """
        wandb_run = branch.metadata.get("wandb_run", {})
        if not wandb_run:
            logger.warning(f"No W&B run associated with branch {branch.name}")
            return
            
        # Get run data from W&B
        api = wandb.Api()
        run = api.run(f"{self.entity}/{self.wandb_project}/{wandb_run['run_id']}")
        
        export_data = {
            "branch": {
                "name": branch.name,
                "commit": branch.commit_hash,
                "created_at": branch.created_at.isoformat()
            },
            "wandb_run": {
                "run_id": run.id,
                "run_name": run.name,
                "state": run.state,
                "url": run.url,
                "config": dict(run.config),
                "summary": dict(run.summary)
            },
            "metrics_history": []
        }
        
        # Get metrics history
        for row in run.scan_history():
            export_data["metrics_history"].append(row)
            
        with open(output_path, 'w') as f:
            json.dump(export_data, f, indent=2, default=str)
            
        logger.info(f"Exported experiment data for branch {branch.name} to {output_path}")


def create_wandb_integration(project: Project, wandb_project: Optional[str] = None, 
                           entity: Optional[str] = None) -> ArgonWandbIntegration:
    """
    Create a W&B integration for an Argon project
    
    Args:
        project: The Argon project
        wandb_project: Optional W&B project name
        entity: Optional W&B entity name
        
    Returns:
        W&B integration instance
    """
    return ArgonWandbIntegration(project, wandb_project, entity)