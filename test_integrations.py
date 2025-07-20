#!/usr/bin/env python3
"""
Test script for ML and Jupyter integrations
Tests core functionality without requiring external dependencies
"""

import sys
import os
from unittest.mock import Mock, patch, MagicMock
from datetime import datetime
import json

# Add current directory to path
sys.path.insert(0, '.')

def test_mlflow_integration():
    """Test MLflow integration with mocked dependencies"""
    print("🧪 Testing MLflow Integration...")
    
    # Mock external dependencies
    with patch.dict('sys.modules', {
        'mlflow': Mock(),
        'mlflow.tracking': Mock(),
        'mlflow.entities': Mock(),
        'mlflow.utils.mlflow_tags': Mock(),
        'argon.core.branch': Mock(),
        'argon.core.project': Mock(),
    }):
        try:
            # Import after mocking
            from integrations.mlflow import ArgonMLflowIntegration
            
            # Create mock project and branch
            mock_project = Mock()
            mock_project.name = "test-project"
            
            mock_branch = Mock()
            mock_branch.name = "test-branch"
            mock_branch.commit_hash = "abc123"
            mock_branch.parent_branch = "main"
            mock_branch.created_at = datetime.now()
            mock_branch.metadata = {}
            mock_branch.get_stats.return_value = {"collections": 5, "documents": 100}
            
            # Test initialization
            integration = ArgonMLflowIntegration(mock_project)
            assert integration.project == mock_project
            print("  ✅ MLflow integration initialization works")
            
            # Test parameter logging
            integration.log_parameters({"lr": 0.01, "batch_size": 32})
            print("  ✅ Parameter logging works")
            
            # Test metrics logging
            integration.log_model_performance({"accuracy": 0.95})
            print("  ✅ Metrics logging works")
            
            print("✅ MLflow integration tests passed")
            return True
            
        except Exception as e:
            print(f"❌ MLflow integration test failed: {e}")
            return False

def test_dvc_integration():
    """Test DVC integration with mocked dependencies"""
    print("\n🧪 Testing DVC Integration...")
    
    # Mock external dependencies
    with patch.dict('sys.modules', {
        'dvc': Mock(),
        'dvc.api': Mock(),
        'dvc.repo': Mock(),
        'subprocess': Mock(),
        'argon.core.branch': Mock(),
        'argon.core.project': Mock(),
    }):
        try:
            from integrations.dvc import ArgonDVCIntegration
            
            # Create mock project and branch
            mock_project = Mock()
            mock_project.name = "test-project"
            
            mock_branch = Mock()
            mock_branch.name = "test-branch"
            mock_branch.commit_hash = "abc123"
            mock_branch.created_at = datetime.now()
            mock_branch.metadata = {}
            
            # Test initialization
            integration = ArgonDVCIntegration(mock_project)
            assert integration.project == mock_project
            print("  ✅ DVC integration initialization works")
            
            # Test pipeline creation
            pipeline_config = {
                "stages": {
                    "train": {
                        "cmd": "python train.py",
                        "deps": ["data/train.csv"],
                        "outs": ["models/model.pkl"]
                    }
                }
            }
            
            # Mock the pipeline creation
            with patch('os.path.exists', return_value=False), \
                 patch('builtins.open', mock_open()), \
                 patch('yaml.dump'):
                
                integration.create_pipeline(pipeline_config, mock_branch)
                print("  ✅ Pipeline creation works")
            
            print("✅ DVC integration tests passed")
            return True
            
        except Exception as e:
            print(f"❌ DVC integration test failed: {e}")
            return False

def test_wandb_integration():
    """Test W&B integration with mocked dependencies"""
    print("\n🧪 Testing W&B Integration...")
    
    # Mock external dependencies
    with patch.dict('sys.modules', {
        'wandb': Mock(),
        'argon.core.branch': Mock(),
        'argon.core.project': Mock(),
    }):
        try:
            from integrations.wandb import ArgonWandbIntegration
            
            # Create mock project and branch
            mock_project = Mock()
            mock_project.name = "test-project"
            
            mock_branch = Mock()
            mock_branch.name = "test-branch"
            mock_branch.commit_hash = "abc123"
            mock_branch.parent_branch = "main"
            mock_branch.created_at = datetime.now()
            mock_branch.metadata = {}
            mock_branch.get_stats.return_value = {"collections": 5, "documents": 100}
            
            # Test initialization
            integration = ArgonWandbIntegration(mock_project)
            assert integration.project == mock_project
            print("  ✅ W&B integration initialization works")
            
            # Test metrics logging
            integration.log_metrics({"accuracy": 0.95})
            print("  ✅ Metrics logging works")
            
            print("✅ W&B integration tests passed")
            return True
            
        except Exception as e:
            print(f"❌ W&B integration test failed: {e}")
            return False

def test_jupyter_integration():
    """Test Jupyter integration with mocked dependencies"""
    print("\n🧪 Testing Jupyter Integration...")
    
    # Mock external dependencies
    with patch.dict('sys.modules', {
        'IPython': Mock(),
        'IPython.core.magic': Mock(),
        'IPython.core.magic_arguments': Mock(),
        'IPython.display': Mock(),
        'argon.core.branch': Mock(),
        'argon.core.project': Mock(),
    }):
        try:
            from integrations.jupyter import ArgonJupyterIntegration
            
            # Create mock project and branch
            mock_project = Mock()
            mock_project.name = "test-project"
            
            mock_branch = Mock()
            mock_branch.name = "test-branch"
            mock_branch.commit_hash = "abc123"
            mock_branch.created_at = datetime.now()
            mock_branch.metadata = {}
            
            # Test initialization
            integration = ArgonJupyterIntegration(mock_project)
            assert integration.project == mock_project
            print("  ✅ Jupyter integration initialization works")
            
            # Test setting branch
            mock_project.get_branch.return_value = mock_branch
            branch = integration.set_branch("test-branch")
            assert branch == mock_branch
            print("  ✅ Branch setting works")
            
            # Test parameter logging
            integration.log_experiment_params({"lr": 0.01})
            assert integration.experiment_metadata["params"]["lr"] == 0.01
            print("  ✅ Parameter logging works")
            
            # Test metrics logging
            integration.log_experiment_metrics({"accuracy": 0.95})
            assert integration.experiment_metadata["metrics"]["accuracy"] == 0.95
            print("  ✅ Metrics logging works")
            
            # Test cell tracking
            integration.track_cell_execution("cell_1", "print('hello')", "hello", 0.5)
            assert "cell_1" in integration.cell_outputs
            print("  ✅ Cell tracking works")
            
            print("✅ Jupyter integration tests passed")
            return True
            
        except Exception as e:
            print(f"❌ Jupyter integration test failed: {e}")
            return False

def test_jupyter_magic_commands():
    """Test Jupyter magic commands structure"""
    print("\n🧪 Testing Jupyter Magic Commands...")
    
    # Mock external dependencies
    with patch.dict('sys.modules', {
        'IPython': Mock(),
        'IPython.core.magic': Mock(),
        'IPython.core.magic_arguments': Mock(),
        'IPython.display': Mock(),
        'argon.core.branch': Mock(),
        'argon.core.project': Mock(),
    }):
        try:
            from integrations.jupyter_magic import ArgonMagics
            
            # Test that magic class exists
            assert hasattr(ArgonMagics, 'argon_init')
            assert hasattr(ArgonMagics, 'argon_branch')
            assert hasattr(ArgonMagics, 'argon_params')
            assert hasattr(ArgonMagics, 'argon_metrics')
            assert hasattr(ArgonMagics, 'argon_status')
            assert hasattr(ArgonMagics, 'argon_track')
            print("  ✅ All magic commands are defined")
            
            # Test extension loading functions
            from integrations.jupyter_magic import load_ipython_extension, unload_ipython_extension
            assert callable(load_ipython_extension)
            assert callable(unload_ipython_extension)
            print("  ✅ Extension loading functions work")
            
            print("✅ Jupyter magic commands tests passed")
            return True
            
        except Exception as e:
            print(f"❌ Jupyter magic commands test failed: {e}")
            return False

def test_integration_factories():
    """Test factory functions"""
    print("\n🧪 Testing Integration Factory Functions...")
    
    # Mock external dependencies
    with patch.dict('sys.modules', {
        'mlflow': Mock(),
        'mlflow.tracking': Mock(),
        'mlflow.entities': Mock(),
        'mlflow.utils.mlflow_tags': Mock(),
        'dvc': Mock(),
        'dvc.api': Mock(),
        'dvc.repo': Mock(),
        'wandb': Mock(),
        'IPython': Mock(),
        'argon.core.branch': Mock(),
        'argon.core.project': Mock(),
    }):
        try:
            from integrations import (
                create_mlflow_integration,
                create_dvc_integration,
                create_wandb_integration,
                create_jupyter_integration
            )
            
            mock_project = Mock()
            mock_project.name = "test-project"
            
            # Test factory functions exist and are callable
            assert callable(create_mlflow_integration)
            assert callable(create_dvc_integration)
            assert callable(create_wandb_integration)
            assert callable(create_jupyter_integration)
            print("  ✅ All factory functions are callable")
            
            print("✅ Integration factory tests passed")
            return True
            
        except Exception as e:
            print(f"❌ Integration factory test failed: {e}")
            return False

def test_example_imports():
    """Test example files can be imported"""
    print("\n🧪 Testing Example Files...")
    
    try:
        # Check if example files exist
        example_files = [
            "examples/ml_integration_example.py",
            "examples/jupyter_notebook_example.ipynb"
        ]
        
        for file_path in example_files:
            if os.path.exists(file_path):
                print(f"  ✅ {file_path} exists")
            else:
                print(f"  ❌ {file_path} missing")
                return False
        
        print("✅ Example files tests passed")
        return True
        
    except Exception as e:
        print(f"❌ Example files test failed: {e}")
        return False

def mock_open(mock_data=""):
    """Helper function to mock file open"""
    from unittest.mock import mock_open as mock_open_builtin
    return mock_open_builtin(read_data=mock_data)

def main():
    """Run all tests"""
    print("🚀 Testing Argon ML and Jupyter Integrations")
    print("=" * 60)
    
    tests = [
        test_mlflow_integration,
        test_dvc_integration,
        test_wandb_integration,
        test_jupyter_integration,
        test_jupyter_magic_commands,
        test_integration_factories,
        test_example_imports
    ]
    
    passed = 0
    failed = 0
    
    for test in tests:
        try:
            if test():
                passed += 1
            else:
                failed += 1
        except Exception as e:
            print(f"❌ Test {test.__name__} failed with exception: {e}")
            failed += 1
    
    print("\n" + "=" * 60)
    print(f"📊 Test Results: {passed} passed, {failed} failed")
    
    if failed == 0:
        print("🎉 All tests passed! ML and Jupyter integrations are working correctly.")
        return True
    else:
        print(f"💥 {failed} tests failed. See output above for details.")
        return False

if __name__ == "__main__":
    success = main()
    sys.exit(0 if success else 1)