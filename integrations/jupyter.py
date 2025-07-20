"""
Jupyter Notebook integration for Argon - Track experiments and data within notebooks
"""

import json
import os
import subprocess
from typing import Dict, Any, Optional, List
from datetime import datetime
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

class ArgonJupyterIntegration:
    """
    Jupyter integration that provides magic commands and cell tracking for Argon
    """
    
    def __init__(self, project: Project):
        self.project = project
        self.current_branch = None
        self.cell_outputs = {}
        self.experiment_metadata = {}
        
    def set_branch(self, branch_name: str) -> Branch:
        """
        Set the current branch for notebook operations
        
        Args:
            branch_name: Name of the branch to work with
            
        Returns:
            The Argon branch object
        """
        try:
            self.current_branch = self.project.get_branch(branch_name)
        except:
            # Create branch if it doesn't exist
            self.current_branch = self.project.create_branch(branch_name)
            
        # Initialize notebook metadata
        if "jupyter_sessions" not in self.current_branch.metadata:
            self.current_branch.metadata["jupyter_sessions"] = []
            
        session_info = {
            "started_at": datetime.now().isoformat(),
            "notebook_path": os.getcwd(),
            "cell_count": 0,
            "outputs": []
        }
        
        self.current_branch.metadata["jupyter_sessions"].append(session_info)
        self.current_branch.save()
        
        return self.current_branch
        
    def track_cell_execution(self, cell_id: str, code: str, output: Any, execution_time: float = None):
        """
        Track execution of a notebook cell
        
        Args:
            cell_id: Unique identifier for the cell
            code: Code that was executed
            output: Output from the cell execution
            execution_time: Time taken to execute (seconds)
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        cell_data = {
            "cell_id": cell_id,
            "code": code,
            "output": str(output) if output else "",
            "execution_time": execution_time,
            "timestamp": datetime.now().isoformat()
        }
        
        # Store in current session
        self.cell_outputs[cell_id] = cell_data
        
        # Update branch metadata
        sessions = self.current_branch.metadata.get("jupyter_sessions", [])
        if sessions:
            sessions[-1]["cell_count"] += 1
            sessions[-1]["outputs"].append(cell_data)
            sessions[-1]["last_updated"] = datetime.now().isoformat()
            
        self.current_branch.metadata["jupyter_sessions"] = sessions
        self.current_branch.save()
        
    def log_experiment_params(self, params: Dict[str, Any]):
        """
        Log experiment parameters from notebook
        
        Args:
            params: Dictionary of parameters
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        self.experiment_metadata["params"] = params
        self.current_branch.metadata["experiment_params"] = params
        self.current_branch.save()
        
    def log_experiment_metrics(self, metrics: Dict[str, float]):
        """
        Log experiment metrics from notebook
        
        Args:
            metrics: Dictionary of metrics
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        self.experiment_metadata["metrics"] = metrics
        self.current_branch.metadata["experiment_metrics"] = metrics
        self.current_branch.save()
        
    def save_dataset(self, data, name: str, description: str = ""):
        """
        Save a dataset to the current branch
        
        Args:
            data: Dataset to save (pandas DataFrame, numpy array, etc.)
            name: Name for the dataset
            description: Optional description
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        # Create data directory if it doesn't exist
        data_dir = os.path.join(os.getcwd(), "argon_data")
        os.makedirs(data_dir, exist_ok=True)
        
        # Save data based on type
        file_path = os.path.join(data_dir, f"{name}.pkl")
        
        try:
            import pandas as pd
            if isinstance(data, pd.DataFrame):
                file_path = os.path.join(data_dir, f"{name}.csv")
                data.to_csv(file_path, index=False)
            else:
                import pickle
                with open(file_path, 'wb') as f:
                    pickle.dump(data, f)
        except ImportError:
            # Fallback to pickle
            import pickle
            with open(file_path, 'wb') as f:
                pickle.dump(data, f)
                
        # Record dataset metadata
        dataset_info = {
            "name": name,
            "description": description,
            "file_path": file_path,
            "created_at": datetime.now().isoformat(),
            "type": type(data).__name__
        }
        
        if "datasets" not in self.current_branch.metadata:
            self.current_branch.metadata["datasets"] = []
            
        self.current_branch.metadata["datasets"].append(dataset_info)
        self.current_branch.save()
        
        return file_path
        
    def load_dataset(self, name: str):
        """
        Load a dataset from the current branch
        
        Args:
            name: Name of the dataset to load
            
        Returns:
            The loaded dataset
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return None
            
        datasets = self.current_branch.metadata.get("datasets", [])
        
        for dataset in datasets:
            if dataset["name"] == name:
                file_path = dataset["file_path"]
                
                if file_path.endswith('.csv'):
                    import pandas as pd
                    return pd.read_csv(file_path)
                else:
                    import pickle
                    with open(file_path, 'rb') as f:
                        return pickle.load(f)
                        
        logger.warning(f"Dataset '{name}' not found in current branch")
        return None
        
    def save_model(self, model, name: str, description: str = "", metadata: Dict[str, Any] = None):
        """
        Save a model to the current branch
        
        Args:
            model: Model to save
            name: Name for the model
            description: Optional description
            metadata: Optional metadata
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        # Create models directory if it doesn't exist
        models_dir = os.path.join(os.getcwd(), "argon_models")
        os.makedirs(models_dir, exist_ok=True)
        
        # Save model
        file_path = os.path.join(models_dir, f"{name}.pkl")
        
        try:
            import joblib
            joblib.dump(model, file_path)
        except ImportError:
            import pickle
            with open(file_path, 'wb') as f:
                pickle.dump(model, f)
                
        # Record model metadata
        model_info = {
            "name": name,
            "description": description,
            "file_path": file_path,
            "created_at": datetime.now().isoformat(),
            "type": type(model).__name__,
            "metadata": metadata or {}
        }
        
        if "models" not in self.current_branch.metadata:
            self.current_branch.metadata["models"] = []
            
        self.current_branch.metadata["models"].append(model_info)
        self.current_branch.save()
        
        return file_path
        
    def load_model(self, name: str):
        """
        Load a model from the current branch
        
        Args:
            name: Name of the model to load
            
        Returns:
            The loaded model
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return None
            
        models = self.current_branch.metadata.get("models", [])
        
        for model in models:
            if model["name"] == name:
                file_path = model["file_path"]
                
                try:
                    import joblib
                    return joblib.load(file_path)
                except ImportError:
                    import pickle
                    with open(file_path, 'rb') as f:
                        return pickle.load(f)
                        
        logger.warning(f"Model '{name}' not found in current branch")
        return None
        
    def create_checkpoint(self, name: str, description: str = ""):
        """
        Create a checkpoint of the current notebook state
        
        Args:
            name: Name for the checkpoint
            description: Optional description
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        checkpoint_data = {
            "name": name,
            "description": description,
            "created_at": datetime.now().isoformat(),
            "cell_outputs": dict(self.cell_outputs),
            "experiment_metadata": dict(self.experiment_metadata)
        }
        
        if "checkpoints" not in self.current_branch.metadata:
            self.current_branch.metadata["checkpoints"] = []
            
        self.current_branch.metadata["checkpoints"].append(checkpoint_data)
        self.current_branch.save()
        
        logger.info(f"Created checkpoint '{name}' on branch {self.current_branch.name}")
        
    def list_checkpoints(self) -> List[Dict[str, Any]]:
        """
        List all checkpoints in the current branch
        
        Returns:
            List of checkpoint metadata
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return []
            
        return self.current_branch.metadata.get("checkpoints", [])
        
    def export_notebook_results(self, output_path: str):
        """
        Export notebook execution results to a file
        
        Args:
            output_path: Path to save the results
        """
        if not self.current_branch:
            logger.warning("No active branch. Use set_branch() first.")
            return
            
        export_data = {
            "branch": {
                "name": self.current_branch.name,
                "commit": self.current_branch.commit_hash,
                "created_at": self.current_branch.created_at.isoformat()
            },
            "notebook_sessions": self.current_branch.metadata.get("jupyter_sessions", []),
            "experiment_params": self.current_branch.metadata.get("experiment_params", {}),
            "experiment_metrics": self.current_branch.metadata.get("experiment_metrics", {}),
            "datasets": self.current_branch.metadata.get("datasets", []),
            "models": self.current_branch.metadata.get("models", []),
            "checkpoints": self.current_branch.metadata.get("checkpoints", [])
        }
        
        with open(output_path, 'w') as f:
            json.dump(export_data, f, indent=2, default=str)
            
        logger.info(f"Exported notebook results to {output_path}")
        
    def compare_experiments(self, branch_names: List[str]) -> Dict[str, Any]:
        """
        Compare experiments across multiple branches
        
        Args:
            branch_names: List of branch names to compare
            
        Returns:
            Comparison results
        """
        comparison = {
            "branches": [],
            "metrics_comparison": {},
            "params_comparison": {}
        }
        
        for branch_name in branch_names:
            try:
                branch = self.project.get_branch(branch_name)
                branch_data = {
                    "name": branch.name,
                    "commit": branch.commit_hash,
                    "params": branch.metadata.get("experiment_params", {}),
                    "metrics": branch.metadata.get("experiment_metrics", {}),
                    "datasets": len(branch.metadata.get("datasets", [])),
                    "models": len(branch.metadata.get("models", [])),
                    "checkpoints": len(branch.metadata.get("checkpoints", []))
                }
                comparison["branches"].append(branch_data)
                
                # Aggregate metrics for comparison
                for metric_name, value in branch_data["metrics"].items():
                    if metric_name not in comparison["metrics_comparison"]:
                        comparison["metrics_comparison"][metric_name] = []
                    comparison["metrics_comparison"][metric_name].append({
                        "branch": branch_name,
                        "value": value
                    })
                    
            except Exception as e:
                logger.error(f"Error comparing branch {branch_name}: {e}")
                
        return comparison


def create_jupyter_integration(project: Project) -> ArgonJupyterIntegration:
    """
    Create a Jupyter integration for an Argon project
    
    Args:
        project: The Argon project
        
    Returns:
        Jupyter integration instance
    """
    return ArgonJupyterIntegration(project)


# Global instance for notebook magic commands
_global_integration = None

def init_argon_notebook(project_name: str) -> ArgonJupyterIntegration:
    """
    Initialize Argon for notebook use
    
    Args:
        project_name: Name of the Argon project
        
    Returns:
        Jupyter integration instance
    """
    global _global_integration
    
    project = Project(project_name)
    _global_integration = create_jupyter_integration(project)
    
    return _global_integration


def get_argon_integration() -> Optional[ArgonJupyterIntegration]:
    """
    Get the current Argon integration instance
    
    Returns:
        The current integration or None if not initialized
    """
    return _global_integration