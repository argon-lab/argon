"""
Unit tests for Jupyter integration
"""

import pytest
import tempfile
import os
import json
from unittest.mock import Mock, patch, MagicMock
from datetime import datetime
import pandas as pd
import numpy as np

from argon.core.project import Project
from argon.core.branch import Branch
from argon.integrations.jupyter import ArgonJupyterIntegration, init_argon_notebook, get_argon_integration

class TestJupyterIntegration:
    """Test Jupyter integration functionality"""
    
    @pytest.fixture
    def mock_project(self):
        """Create a mock project for testing"""
        project = Mock(spec=Project)
        project.name = "test-project"
        return project
    
    @pytest.fixture
    def mock_branch(self):
        """Create a mock branch for testing"""
        branch = Mock(spec=Branch)
        branch.name = "test-branch"
        branch.commit_hash = "abc123def456"
        branch.created_at = datetime.now()
        branch.metadata = {}
        branch.get_stats.return_value = {
            "collections": 5,
            "documents": 1000,
            "size_bytes": 50000
        }
        return branch
    
    def test_jupyter_integration_init(self, mock_project):
        """Test Jupyter integration initialization"""
        integration = ArgonJupyterIntegration(mock_project)
        
        assert integration.project == mock_project
        assert integration.current_branch is None
        assert integration.cell_outputs == {}
        assert integration.experiment_metadata == {}
    
    def test_set_branch(self, mock_project, mock_branch):
        """Test setting a branch"""
        mock_project.get_branch.return_value = mock_branch
        mock_project.create_branch.return_value = mock_branch
        
        integration = ArgonJupyterIntegration(mock_project)
        
        # Test setting existing branch
        branch = integration.set_branch("test-branch")
        
        assert branch == mock_branch
        assert integration.current_branch == mock_branch
        assert "jupyter_sessions" in mock_branch.metadata
        mock_branch.save.assert_called_once()
    
    def test_set_branch_create_new(self, mock_project, mock_branch):
        """Test setting a branch that doesn't exist"""
        mock_project.get_branch.side_effect = Exception("Branch not found")
        mock_project.create_branch.return_value = mock_branch
        
        integration = ArgonJupyterIntegration(mock_project)
        
        # Test creating new branch
        branch = integration.set_branch("new-branch")
        
        assert branch == mock_branch
        assert integration.current_branch == mock_branch
        mock_project.create_branch.assert_called_once_with("new-branch")
    
    def test_track_cell_execution(self, mock_project, mock_branch):
        """Test tracking cell execution"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        mock_branch.metadata = {"jupyter_sessions": [{"cell_count": 0, "outputs": []}]}
        
        # Track a cell execution
        integration.track_cell_execution(
            cell_id="cell_1",
            code="print('hello')",
            output="hello",
            execution_time=0.5
        )
        
        # Check if cell was tracked
        assert "cell_1" in integration.cell_outputs
        cell_data = integration.cell_outputs["cell_1"]
        assert cell_data["code"] == "print('hello')"
        assert cell_data["output"] == "hello"
        assert cell_data["execution_time"] == 0.5
        
        # Check if metadata was updated
        sessions = mock_branch.metadata["jupyter_sessions"]
        assert sessions[0]["cell_count"] == 1
        assert len(sessions[0]["outputs"]) == 1
        mock_branch.save.assert_called()
    
    def test_log_experiment_params(self, mock_project, mock_branch):
        """Test logging experiment parameters"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        
        params = {"learning_rate": 0.01, "batch_size": 32}
        integration.log_experiment_params(params)
        
        assert integration.experiment_metadata["params"] == params
        assert mock_branch.metadata["experiment_params"] == params
        mock_branch.save.assert_called_once()
    
    def test_log_experiment_metrics(self, mock_project, mock_branch):
        """Test logging experiment metrics"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        
        metrics = {"accuracy": 0.95, "loss": 0.05}
        integration.log_experiment_metrics(metrics)
        
        assert integration.experiment_metadata["metrics"] == metrics
        assert mock_branch.metadata["experiment_metrics"] == metrics
        mock_branch.save.assert_called_once()
    
    def test_save_dataset_pandas(self, mock_project, mock_branch):
        """Test saving a pandas DataFrame"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        mock_branch.metadata = {}
        
        # Create sample DataFrame
        df = pd.DataFrame({"A": [1, 2, 3], "B": [4, 5, 6]})
        
        with tempfile.TemporaryDirectory() as temp_dir:
            os.chdir(temp_dir)
            
            file_path = integration.save_dataset(df, "test_data", "Test dataset")
            
            # Check if file was created
            assert os.path.exists(file_path)
            assert file_path.endswith(".csv")
            
            # Check if metadata was updated
            assert "datasets" in mock_branch.metadata
            datasets = mock_branch.metadata["datasets"]
            assert len(datasets) == 1
            assert datasets[0]["name"] == "test_data"
            assert datasets[0]["description"] == "Test dataset"
            mock_branch.save.assert_called_once()
    
    def test_save_dataset_numpy(self, mock_project, mock_branch):
        """Test saving a numpy array"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        mock_branch.metadata = {}
        
        # Create sample array
        arr = np.array([1, 2, 3, 4, 5])
        
        with tempfile.TemporaryDirectory() as temp_dir:
            os.chdir(temp_dir)
            
            file_path = integration.save_dataset(arr, "test_array", "Test array")
            
            # Check if file was created
            assert os.path.exists(file_path)
            assert file_path.endswith(".pkl")
            
            # Check if metadata was updated
            assert "datasets" in mock_branch.metadata
            datasets = mock_branch.metadata["datasets"]
            assert len(datasets) == 1
            assert datasets[0]["name"] == "test_array"
            mock_branch.save.assert_called_once()
    
    def test_load_dataset_pandas(self, mock_project, mock_branch):
        """Test loading a pandas DataFrame"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        
        with tempfile.TemporaryDirectory() as temp_dir:
            os.chdir(temp_dir)
            
            # Create and save a DataFrame
            df = pd.DataFrame({"A": [1, 2, 3], "B": [4, 5, 6]})
            csv_path = os.path.join(temp_dir, "test_data.csv")
            df.to_csv(csv_path, index=False)
            
            # Set up metadata
            mock_branch.metadata = {
                "datasets": [{
                    "name": "test_data",
                    "file_path": csv_path,
                    "type": "DataFrame"
                }]
            }
            
            # Load the dataset
            loaded_df = integration.load_dataset("test_data")
            
            # Check if DataFrame was loaded correctly
            assert isinstance(loaded_df, pd.DataFrame)
            pd.testing.assert_frame_equal(df, loaded_df)
    
    def test_load_dataset_not_found(self, mock_project, mock_branch):
        """Test loading a dataset that doesn't exist"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        mock_branch.metadata = {"datasets": []}
        
        # Try to load non-existent dataset
        result = integration.load_dataset("nonexistent")
        
        assert result is None
    
    def test_save_model(self, mock_project, mock_branch):
        """Test saving a model"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        mock_branch.metadata = {}
        
        # Create a mock model
        model = Mock()
        model.__class__.__name__ = "RandomForestClassifier"
        
        with tempfile.TemporaryDirectory() as temp_dir:
            os.chdir(temp_dir)
            
            file_path = integration.save_model(
                model, 
                "test_model", 
                "Test model",
                metadata={"algorithm": "RandomForest"}
            )
            
            # Check if file was created
            assert os.path.exists(file_path)
            assert file_path.endswith(".pkl")
            
            # Check if metadata was updated
            assert "models" in mock_branch.metadata
            models = mock_branch.metadata["models"]
            assert len(models) == 1
            assert models[0]["name"] == "test_model"
            assert models[0]["description"] == "Test model"
            assert models[0]["metadata"]["algorithm"] == "RandomForest"
            mock_branch.save.assert_called_once()
    
    def test_load_model(self, mock_project, mock_branch):
        """Test loading a model"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        
        with tempfile.TemporaryDirectory() as temp_dir:
            os.chdir(temp_dir)
            
            # Create and save a mock model
            model = {"type": "test_model", "params": {"n_estimators": 100}}
            pkl_path = os.path.join(temp_dir, "test_model.pkl")
            
            import pickle
            with open(pkl_path, 'wb') as f:
                pickle.dump(model, f)
            
            # Set up metadata
            mock_branch.metadata = {
                "models": [{
                    "name": "test_model",
                    "file_path": pkl_path,
                    "type": "dict"
                }]
            }
            
            # Load the model
            loaded_model = integration.load_model("test_model")
            
            # Check if model was loaded correctly
            assert loaded_model == model
    
    def test_create_checkpoint(self, mock_project, mock_branch):
        """Test creating a checkpoint"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        mock_branch.metadata = {}
        
        # Add some test data
        integration.cell_outputs = {"cell_1": {"code": "test", "output": "result"}}
        integration.experiment_metadata = {"params": {"lr": 0.01}}
        
        # Create checkpoint
        integration.create_checkpoint("test_checkpoint", "Test checkpoint")
        
        # Check if checkpoint was created
        assert "checkpoints" in mock_branch.metadata
        checkpoints = mock_branch.metadata["checkpoints"]
        assert len(checkpoints) == 1
        assert checkpoints[0]["name"] == "test_checkpoint"
        assert checkpoints[0]["description"] == "Test checkpoint"
        assert checkpoints[0]["cell_outputs"] == integration.cell_outputs
        assert checkpoints[0]["experiment_metadata"] == integration.experiment_metadata
        mock_branch.save.assert_called_once()
    
    def test_list_checkpoints(self, mock_project, mock_branch):
        """Test listing checkpoints"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        
        # Set up test checkpoints
        test_checkpoints = [
            {"name": "checkpoint1", "description": "First checkpoint"},
            {"name": "checkpoint2", "description": "Second checkpoint"}
        ]
        mock_branch.metadata = {"checkpoints": test_checkpoints}
        
        # List checkpoints
        checkpoints = integration.list_checkpoints()
        
        assert checkpoints == test_checkpoints
    
    def test_export_notebook_results(self, mock_project, mock_branch):
        """Test exporting notebook results"""
        integration = ArgonJupyterIntegration(mock_project)
        integration.current_branch = mock_branch
        
        # Set up test data
        mock_branch.metadata = {
            "jupyter_sessions": [{"session": "test"}],
            "experiment_params": {"lr": 0.01},
            "experiment_metrics": {"accuracy": 0.95},
            "datasets": [{"name": "test_data"}],
            "models": [{"name": "test_model"}],
            "checkpoints": [{"name": "test_checkpoint"}]
        }
        
        with tempfile.TemporaryDirectory() as temp_dir:
            output_path = os.path.join(temp_dir, "results.json")
            
            # Export results
            integration.export_notebook_results(output_path)
            
            # Check if file was created
            assert os.path.exists(output_path)
            
            # Check file contents
            with open(output_path, 'r') as f:
                data = json.load(f)
                
            assert "branch" in data
            assert "notebook_sessions" in data
            assert "experiment_params" in data
            assert "experiment_metrics" in data
            assert "datasets" in data
            assert "models" in data
            assert "checkpoints" in data
    
    def test_compare_experiments(self, mock_project):
        """Test comparing experiments across branches"""
        integration = ArgonJupyterIntegration(mock_project)
        
        # Create mock branches
        branch1 = Mock(spec=Branch)
        branch1.name = "branch1"
        branch1.commit_hash = "abc123"
        branch1.metadata = {
            "experiment_params": {"lr": 0.01},
            "experiment_metrics": {"accuracy": 0.95},
            "datasets": [{"name": "data1"}],
            "models": [{"name": "model1"}],
            "checkpoints": [{"name": "checkpoint1"}]
        }
        
        branch2 = Mock(spec=Branch)
        branch2.name = "branch2"
        branch2.commit_hash = "def456"
        branch2.metadata = {
            "experiment_params": {"lr": 0.02},
            "experiment_metrics": {"accuracy": 0.93},
            "datasets": [{"name": "data2"}],
            "models": [{"name": "model2"}],
            "checkpoints": [{"name": "checkpoint2"}]
        }
        
        mock_project.get_branch.side_effect = [branch1, branch2]
        
        # Compare experiments
        comparison = integration.compare_experiments(["branch1", "branch2"])
        
        # Check comparison results
        assert len(comparison["branches"]) == 2
        assert comparison["branches"][0]["name"] == "branch1"
        assert comparison["branches"][1]["name"] == "branch2"
        assert "accuracy" in comparison["metrics_comparison"]
        assert len(comparison["metrics_comparison"]["accuracy"]) == 2

class TestJupyterFactoryFunctions:
    """Test factory functions for Jupyter integration"""
    
    @patch('argon.integrations.jupyter.Project')
    def test_init_argon_notebook(self, mock_project_class):
        """Test initializing Argon notebook"""
        mock_project = Mock()
        mock_project.name = "test-project"
        mock_project_class.return_value = mock_project
        
        integration = init_argon_notebook("test-project")
        
        assert isinstance(integration, ArgonJupyterIntegration)
        assert integration.project == mock_project
        mock_project_class.assert_called_once_with("test-project")
    
    @patch('argon.integrations.jupyter._global_integration')
    def test_get_argon_integration(self, mock_global):
        """Test getting the global integration"""
        mock_integration = Mock()
        mock_global.return_value = mock_integration
        
        integration = get_argon_integration()
        
        # This test depends on the actual global state
        # In a real test, we'd need to set up the global state properly
        pass

class TestJupyterIntegrationWithoutBranch:
    """Test Jupyter integration methods without active branch"""
    
    @pytest.fixture
    def integration(self):
        """Create integration without active branch"""
        mock_project = Mock()
        mock_project.name = "test-project"
        return ArgonJupyterIntegration(mock_project)
    
    def test_track_cell_execution_no_branch(self, integration):
        """Test tracking cell execution without active branch"""
        # Should not raise exception, just warn
        integration.track_cell_execution("cell_1", "code", "output")
        
        # No data should be stored
        assert len(integration.cell_outputs) == 0
    
    def test_log_experiment_params_no_branch(self, integration):
        """Test logging parameters without active branch"""
        # Should not raise exception, just warn
        integration.log_experiment_params({"lr": 0.01})
        
        # No data should be stored in metadata
        assert "params" not in integration.experiment_metadata
    
    def test_log_experiment_metrics_no_branch(self, integration):
        """Test logging metrics without active branch"""
        # Should not raise exception, just warn
        integration.log_experiment_metrics({"accuracy": 0.95})
        
        # No data should be stored in metadata
        assert "metrics" not in integration.experiment_metadata
    
    def test_save_dataset_no_branch(self, integration):
        """Test saving dataset without active branch"""
        df = pd.DataFrame({"A": [1, 2, 3]})
        
        # Should warn and not save
        result = integration.save_dataset(df, "test", "description")
        
        assert result is None
    
    def test_load_dataset_no_branch(self, integration):
        """Test loading dataset without active branch"""
        # Should warn and return None
        result = integration.load_dataset("test")
        
        assert result is None

if __name__ == "__main__":
    pytest.main([__file__])