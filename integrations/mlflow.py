"""
MLflow integration for Argon - Track ML experiments with version control
"""

try:
    import mlflow
    import mlflow.tracking
    from mlflow.entities import RunStatus
    from mlflow.utils.mlflow_tags import MLFLOW_RUN_NAME, MLFLOW_SOURCE_NAME
except ImportError:
    # Mock for testing without mlflow installed
    mlflow = None
    RunStatus = None
    MLFLOW_RUN_NAME = "mlflow.runName"
    MLFLOW_SOURCE_NAME = "mlflow.source.name"
import os
import json
from typing import Dict, Any, Optional, List
import logging

try:
    from ..core.branch import Branch
    from ..core.project import Project
except ImportError:
    # Fallback for testing
    from typing import Any
    Branch = Any
    Project = Any

logger = logging.getLogger(__name__)

class ArgonMLflowIntegration:
    """
    MLflow integration that automatically tracks experiments alongside Argon branches
    """
    
    def __init__(self, project: Project, tracking_uri: Optional[str] = None):
        self.project = project
        self.tracking_uri = tracking_uri or os.getenv("MLFLOW_TRACKING_URI", "sqlite:///mlflow.db")
        
        if mlflow is None:
            logger.warning("MLflow not installed. Please install with: pip install mlflow")
            self.experiment = None
            return
            
        mlflow.set_tracking_uri(self.tracking_uri)
        
        # Set experiment name to match project
        self.experiment_name = f"argon-{project.name}"
        try:
            self.experiment = mlflow.create_experiment(self.experiment_name)
        except Exception:
            # Experiment already exists
            self.experiment = mlflow.get_experiment_by_name(self.experiment_name)
            
    def start_run(self, branch: Branch, run_name: Optional[str] = None) -> str:
        """
        Start an MLflow run tied to an Argon branch
        
        Args:
            branch: The Argon branch for this experiment
            run_name: Optional custom run name
            
        Returns:
            The MLflow run ID
        """
        mlflow.set_experiment(self.experiment_name)
        
        # Create run name from branch if not provided
        if not run_name:
            run_name = f"branch-{branch.name}-{branch.commit_hash[:8]}"
            
        # Start the run with Argon metadata
        run = mlflow.start_run(run_name=run_name)
        
        # Log Argon-specific metadata
        mlflow.set_tags({
            "argon.project": self.project.name,
            "argon.branch": branch.name,
            "argon.commit": branch.commit_hash,
            "argon.parent_branch": branch.parent_branch or "main",
            "argon.created_at": branch.created_at.isoformat(),
            MLFLOW_SOURCE_NAME: "argon"
        })
        
        # Log branch statistics as metrics
        stats = branch.get_stats()
        mlflow.log_metrics({
            "argon.collection_count": stats.get("collections", 0),
            "argon.document_count": stats.get("documents", 0),
            "argon.size_bytes": stats.get("size_bytes", 0)
        })
        
        logger.info(f"Started MLflow run {run.info.run_id} for branch {branch.name}")
        return run.info.run_id
        
    def log_model_performance(self, metrics: Dict[str, float], step: Optional[int] = None):
        """
        Log model performance metrics
        
        Args:
            metrics: Dictionary of metric names to values
            step: Optional step number for time series tracking
        """
        mlflow.log_metrics(metrics, step=step)
        
    def log_model_artifacts(self, artifacts_path: str, artifact_path: Optional[str] = None):
        """
        Log model artifacts (models, plots, etc.)
        
        Args:
            artifacts_path: Local path to artifacts
            artifact_path: Optional path within the run's artifact directory
        """
        mlflow.log_artifacts(artifacts_path, artifact_path)
        
    def log_parameters(self, params: Dict[str, Any]):
        """
        Log experiment parameters
        
        Args:
            params: Dictionary of parameter names to values
        """
        mlflow.log_params(params)
        
    def create_branch_from_run(self, run_id: str, branch_name: str) -> Branch:
        """
        Create a new Argon branch from an MLflow run
        
        Args:
            run_id: MLflow run ID
            branch_name: Name for the new branch
            
        Returns:
            The created Argon branch
        """
        run = mlflow.get_run(run_id)
        
        # Get the parent branch from run tags
        parent_branch = run.data.tags.get("argon.parent_branch", "main")
        
        # Create new branch
        branch = self.project.create_branch(branch_name, parent_branch)
        
        # Store MLflow run reference
        branch.metadata["mlflow_run_id"] = run_id
        branch.metadata["mlflow_experiment"] = self.experiment_name
        
        # Copy run artifacts to branch if needed
        artifact_path = run.info.artifact_uri
        if artifact_path and os.path.exists(artifact_path):
            # Store reference to artifacts
            branch.metadata["mlflow_artifacts"] = artifact_path
            
        branch.save()
        return branch
        
    def get_run_branches(self, run_id: str) -> List[Branch]:
        """
        Get all branches associated with an MLflow run
        
        Args:
            run_id: MLflow run ID
            
        Returns:
            List of associated branches
        """
        branches = []
        for branch in self.project.list_branches():
            if branch.metadata.get("mlflow_run_id") == run_id:
                branches.append(branch)
        return branches
        
    def compare_runs(self, run_ids: List[str]) -> Dict[str, Any]:
        """
        Compare multiple MLflow runs
        
        Args:
            run_ids: List of MLflow run IDs to compare
            
        Returns:
            Comparison data
        """
        runs = [mlflow.get_run(run_id) for run_id in run_ids]
        
        comparison = {
            "runs": [],
            "metrics": {},
            "parameters": {}
        }
        
        for run in runs:
            run_data = {
                "run_id": run.info.run_id,
                "run_name": run.data.tags.get(MLFLOW_RUN_NAME, ""),
                "status": run.info.status,
                "argon_branch": run.data.tags.get("argon.branch", ""),
                "argon_commit": run.data.tags.get("argon.commit", ""),
                "metrics": run.data.metrics,
                "parameters": run.data.params
            }
            comparison["runs"].append(run_data)
            
            # Aggregate metrics for comparison
            for metric_name, value in run.data.metrics.items():
                if metric_name not in comparison["metrics"]:
                    comparison["metrics"][metric_name] = []
                comparison["metrics"][metric_name].append(value)
                
        return comparison
        
    def end_run(self, status: str = "FINISHED"):
        """
        End the current MLflow run
        
        Args:
            status: Run status (FINISHED, FAILED, KILLED)
        """
        if status == "FINISHED":
            mlflow.end_run(RunStatus.to_string(RunStatus.FINISHED))
        elif status == "FAILED":
            mlflow.end_run(RunStatus.to_string(RunStatus.FAILED))
        elif status == "KILLED":
            mlflow.end_run(RunStatus.to_string(RunStatus.KILLED))
        else:
            mlflow.end_run()
            
    def export_experiment_data(self, output_path: str):
        """
        Export experiment data to JSON for backup/analysis
        
        Args:
            output_path: Path to save the exported data
        """
        client = mlflow.tracking.MlflowClient()
        runs = client.search_runs(
            experiment_ids=[self.experiment.experiment_id],
            max_results=1000
        )
        
        export_data = {
            "experiment": {
                "name": self.experiment.name,
                "experiment_id": self.experiment.experiment_id,
                "lifecycle_stage": self.experiment.lifecycle_stage
            },
            "runs": []
        }
        
        for run in runs:
            run_data = {
                "run_id": run.info.run_id,
                "run_name": run.data.tags.get(MLFLOW_RUN_NAME, ""),
                "status": run.info.status,
                "start_time": run.info.start_time,
                "end_time": run.info.end_time,
                "tags": run.data.tags,
                "metrics": run.data.metrics,
                "parameters": run.data.params
            }
            export_data["runs"].append(run_data)
            
        with open(output_path, 'w') as f:
            json.dump(export_data, f, indent=2, default=str)
            
        logger.info(f"Exported experiment data to {output_path}")


def create_mlflow_integration(project: Project, tracking_uri: Optional[str] = None) -> ArgonMLflowIntegration:
    """
    Create an MLflow integration for an Argon project
    
    Args:
        project: The Argon project
        tracking_uri: Optional MLflow tracking URI
        
    Returns:
        MLflow integration instance
    """
    return ArgonMLflowIntegration(project, tracking_uri)