"""
Unit tests for ML framework integrations
"""

import pytest
import tempfile
import os
import json
from unittest.mock import Mock, patch, MagicMock
from datetime import datetime

from argon.core.project import Project
from argon.core.branch import Branch
from argon.integrations.mlflow import ArgonMLflowIntegration
from argon.integrations.dvc import ArgonDVCIntegration
from argon.integrations.wandb import ArgonWandbIntegration

class TestMLflowIntegration:
    """Test MLflow integration functionality"""
    
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
        branch.parent_branch = "main"
        branch.created_at = datetime.now()
        branch.metadata = {}
        branch.get_stats.return_value = {
            "collections": 5,
            "documents": 1000,
            "size_bytes": 50000
        }
        return branch
    
    @patch('argon.integrations.mlflow.mlflow')
    def test_mlflow_integration_init(self, mock_mlflow, mock_project):
        """Test MLflow integration initialization"""
        mock_mlflow.set_tracking_uri.return_value = None
        mock_mlflow.create_experiment.return_value = "exp-123"
        mock_experiment = Mock()
        mock_experiment.experiment_id = "exp-123"
        mock_mlflow.get_experiment_by_name.return_value = mock_experiment
        
        integration = ArgonMLflowIntegration(mock_project)
        
        assert integration.project == mock_project
        assert integration.experiment_name == "argon-test-project"
        mock_mlflow.set_tracking_uri.assert_called_once()
    
    @patch('argon.integrations.mlflow.mlflow')
    def test_start_run(self, mock_mlflow, mock_project, mock_branch):
        """Test starting an MLflow run"""
        mock_mlflow.set_tracking_uri.return_value = None
        mock_mlflow.create_experiment.return_value = "exp-123"
        mock_experiment = Mock()
        mock_experiment.experiment_id = "exp-123"
        mock_mlflow.get_experiment_by_name.return_value = mock_experiment
        
        mock_run = Mock()
        mock_run.info.run_id = "run-123"
        mock_mlflow.start_run.return_value = mock_run
        
        integration = ArgonMLflowIntegration(mock_project)
        run_id = integration.start_run(mock_branch)
        
        assert run_id == "run-123"
        mock_mlflow.start_run.assert_called_once()
        mock_mlflow.set_tags.assert_called_once()
        mock_mlflow.log_metrics.assert_called_once()
    
    @patch('argon.integrations.mlflow.mlflow')
    def test_log_parameters(self, mock_mlflow, mock_project):
        """Test logging parameters"""
        mock_mlflow.set_tracking_uri.return_value = None
        mock_mlflow.create_experiment.return_value = "exp-123"
        mock_experiment = Mock()
        mock_experiment.experiment_id = "exp-123"
        mock_mlflow.get_experiment_by_name.return_value = mock_experiment
        
        integration = ArgonMLflowIntegration(mock_project)
        params = {"learning_rate": 0.01, "batch_size": 32}
        
        integration.log_parameters(params)
        
        mock_mlflow.log_params.assert_called_once_with(params)
    
    @patch('argon.integrations.mlflow.mlflow')
    def test_log_model_performance(self, mock_mlflow, mock_project):
        """Test logging model performance metrics"""
        mock_mlflow.set_tracking_uri.return_value = None
        mock_mlflow.create_experiment.return_value = "exp-123"
        mock_experiment = Mock()
        mock_experiment.experiment_id = "exp-123"
        mock_mlflow.get_experiment_by_name.return_value = mock_experiment
        
        integration = ArgonMLflowIntegration(mock_project)
        metrics = {"accuracy": 0.95, "loss": 0.05}
        
        integration.log_model_performance(metrics)
        
        mock_mlflow.log_metrics.assert_called_once_with(metrics, step=None)

class TestDVCIntegration:
    """Test DVC integration functionality"""
    
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
        return branch
    
    @patch('argon.integrations.dvc.dvc.repo.Repo')
    @patch('argon.integrations.dvc.subprocess.run')
    def test_dvc_integration_init(self, mock_subprocess, mock_dvc_repo, mock_project):
        """Test DVC integration initialization"""
        mock_repo = Mock()
        mock_dvc_repo.return_value = mock_repo
        
        with tempfile.TemporaryDirectory() as temp_dir:
            integration = ArgonDVCIntegration(mock_project, temp_dir)
            
            assert integration.project == mock_project
            assert integration.dvc_repo_path == temp_dir
            assert integration.dvc_repo == mock_repo
    
    @patch('argon.integrations.dvc.dvc.repo.Repo')
    @patch('argon.integrations.dvc.subprocess.run')
    def test_add_data(self, mock_subprocess, mock_dvc_repo, mock_project, mock_branch):
        """Test adding data to DVC"""
        mock_repo = Mock()
        mock_dvc_repo.return_value = mock_repo
        
        with tempfile.TemporaryDirectory() as temp_dir:
            integration = ArgonDVCIntegration(mock_project, temp_dir)
            
            # Create test data file
            test_file = os.path.join(temp_dir, "test_data.csv")
            with open(test_file, 'w') as f:
                f.write("test,data\n1,2\n")
            
            dvc_file = integration.add_data(test_file, mock_branch)
            
            assert dvc_file == f"{test_file}.dvc"
            mock_subprocess.assert_called_with(
                ["dvc", "add", test_file], 
                cwd=temp_dir, 
                check=True
            )
            assert "dvc_files" in mock_branch.metadata
            mock_branch.save.assert_called_once()
    
    @patch('argon.integrations.dvc.dvc.repo.Repo')
    @patch('argon.integrations.dvc.subprocess.run')
    def test_create_pipeline(self, mock_subprocess, mock_dvc_repo, mock_project, mock_branch):
        """Test creating a DVC pipeline"""
        mock_repo = Mock()
        mock_dvc_repo.return_value = mock_repo
        
        with tempfile.TemporaryDirectory() as temp_dir:
            integration = ArgonDVCIntegration(mock_project, temp_dir)
            
            pipeline_config = {
                "stages": {
                    "train": {
                        "cmd": "python train.py",
                        "deps": ["data/train.csv"],
                        "outs": ["models/model.pkl"]
                    }
                }
            }
            
            pipeline_path = integration.create_pipeline(pipeline_config, mock_branch)
            
            assert pipeline_path == os.path.join(temp_dir, "dvc.yaml")
            assert "dvc_pipeline" in mock_branch.metadata
            mock_branch.save.assert_called_once()
    
    @patch('argon.integrations.dvc.dvc.repo.Repo')
    @patch('argon.integrations.dvc.subprocess.run')
    def test_run_pipeline(self, mock_subprocess, mock_dvc_repo, mock_project):
        """Test running a DVC pipeline"""
        mock_repo = Mock()
        mock_dvc_repo.return_value = mock_repo
        
        # Mock successful subprocess run
        mock_result = Mock()
        mock_result.stdout = "Pipeline completed successfully"
        mock_result.stderr = ""
        mock_subprocess.return_value = mock_result
        
        with tempfile.TemporaryDirectory() as temp_dir:
            integration = ArgonDVCIntegration(mock_project, temp_dir)
            
            result = integration.run_pipeline()
            
            assert result["success"] is True
            assert "Pipeline completed successfully" in result["stdout"]
            mock_subprocess.assert_called_with(
                ["dvc", "repro"],
                cwd=temp_dir,
                capture_output=True,
                text=True,
                check=True
            )

class TestWandbIntegration:
    """Test W&B integration functionality"""
    
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
        branch.parent_branch = "main"
        branch.created_at = datetime.now()
        branch.metadata = {}
        branch.get_stats.return_value = {
            "collections": 5,
            "documents": 1000,
            "size_bytes": 50000
        }
        return branch
    
    @patch('argon.integrations.wandb.wandb')
    def test_wandb_integration_init(self, mock_wandb, mock_project):
        """Test W&B integration initialization"""
        mock_wandb.api.api_key = "test-key"
        
        integration = ArgonWandbIntegration(mock_project)
        
        assert integration.project == mock_project
        assert integration.wandb_project == "argon-test-project"
    
    @patch('argon.integrations.wandb.wandb')
    def test_start_run(self, mock_wandb, mock_project, mock_branch):
        """Test starting a W&B run"""
        mock_wandb.api.api_key = "test-key"
        
        mock_run = Mock()
        mock_run.id = "run-123"
        mock_run.url = "https://wandb.ai/test/test-project/runs/run-123"
        mock_wandb.init.return_value = mock_run
        
        integration = ArgonWandbIntegration(mock_project)
        run_id = integration.start_run(mock_branch)
        
        assert run_id == "run-123"
        mock_wandb.init.assert_called_once()
        assert "wandb_run" in mock_branch.metadata
        mock_branch.save.assert_called_once()
    
    @patch('argon.integrations.wandb.wandb')
    def test_log_metrics(self, mock_wandb, mock_project):
        """Test logging metrics"""
        mock_wandb.api.api_key = "test-key"
        
        mock_run = Mock()
        mock_run.id = "run-123"
        mock_wandb.init.return_value = mock_run
        
        integration = ArgonWandbIntegration(mock_project)
        integration.current_run = mock_run
        
        metrics = {"accuracy": 0.95, "loss": 0.05}
        integration.log_metrics(metrics)
        
        mock_wandb.log.assert_called_once_with(metrics, step=None, commit=None)
    
    @patch('argon.integrations.wandb.wandb')
    def test_log_artifacts(self, mock_wandb, mock_project):
        """Test logging artifacts"""
        mock_wandb.api.api_key = "test-key"
        
        mock_run = Mock()
        mock_run.id = "run-123"
        mock_wandb.init.return_value = mock_run
        
        mock_artifact = Mock()
        mock_wandb.Artifact.return_value = mock_artifact
        
        integration = ArgonWandbIntegration(mock_project)
        integration.current_run = mock_run
        
        with tempfile.TemporaryDirectory() as temp_dir:
            # Create test artifact
            test_file = os.path.join(temp_dir, "model.pkl")
            with open(test_file, 'w') as f:
                f.write("fake model data")
            
            artifacts = {"model": test_file}
            integration.log_artifacts(artifacts)
            
            mock_wandb.Artifact.assert_called_once_with("model", type="model")
            mock_artifact.add_file.assert_called_once_with(test_file)
            mock_run.log_artifact.assert_called_once_with(mock_artifact)
    
    @patch('argon.integrations.wandb.wandb')
    def test_compare_runs(self, mock_wandb, mock_project):
        """Test comparing W&B runs"""
        mock_wandb.api.api_key = "test-key"
        
        # Mock API and runs
        mock_api = Mock()
        mock_wandb.Api.return_value = mock_api
        
        mock_run1 = Mock()
        mock_run1.id = "run-1"
        mock_run1.name = "experiment-1"
        mock_run1.state = "finished"
        mock_run1.config = {"learning_rate": 0.01}
        mock_run1.summary = {"accuracy": 0.95}
        mock_run1.url = "https://wandb.ai/test/test-project/runs/run-1"
        
        mock_run2 = Mock()
        mock_run2.id = "run-2"
        mock_run2.name = "experiment-2"
        mock_run2.state = "finished"
        mock_run2.config = {"learning_rate": 0.02}
        mock_run2.summary = {"accuracy": 0.93}
        mock_run2.url = "https://wandb.ai/test/test-project/runs/run-2"
        
        mock_api.run.side_effect = [mock_run1, mock_run2]
        
        integration = ArgonWandbIntegration(mock_project)
        comparison = integration.compare_runs(["run-1", "run-2"])
        
        assert len(comparison["runs"]) == 2
        assert comparison["runs"][0]["run_id"] == "run-1"
        assert comparison["runs"][1]["run_id"] == "run-2"
        assert "accuracy" in comparison["metrics"]
        assert len(comparison["metrics"]["accuracy"]) == 2

class TestIntegrationFactory:
    """Test integration factory functions"""
    
    @patch('argon.integrations.mlflow.mlflow')
    def test_create_mlflow_integration(self, mock_mlflow):
        """Test creating MLflow integration"""
        from argon.integrations import create_mlflow_integration
        
        mock_mlflow.set_tracking_uri.return_value = None
        mock_mlflow.create_experiment.return_value = "exp-123"
        mock_experiment = Mock()
        mock_experiment.experiment_id = "exp-123"
        mock_mlflow.get_experiment_by_name.return_value = mock_experiment
        
        project = Mock(spec=Project)
        project.name = "test-project"
        
        integration = create_mlflow_integration(project)
        
        assert isinstance(integration, ArgonMLflowIntegration)
        assert integration.project == project
    
    @patch('argon.integrations.dvc.dvc.repo.Repo')
    @patch('argon.integrations.dvc.subprocess.run')
    def test_create_dvc_integration(self, mock_subprocess, mock_dvc_repo):
        """Test creating DVC integration"""
        from argon.integrations import create_dvc_integration
        
        mock_repo = Mock()
        mock_dvc_repo.return_value = mock_repo
        
        project = Mock(spec=Project)
        project.name = "test-project"
        
        integration = create_dvc_integration(project)
        
        assert isinstance(integration, ArgonDVCIntegration)
        assert integration.project == project
    
    @patch('argon.integrations.wandb.wandb')
    def test_create_wandb_integration(self, mock_wandb):
        """Test creating W&B integration"""
        from argon.integrations import create_wandb_integration
        
        mock_wandb.api.api_key = "test-key"
        
        project = Mock(spec=Project)
        project.name = "test-project"
        
        integration = create_wandb_integration(project)
        
        assert isinstance(integration, ArgonWandbIntegration)
        assert integration.project == project

if __name__ == "__main__":
    pytest.main([__file__])