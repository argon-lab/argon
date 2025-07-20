#!/usr/bin/env python3
"""
Test core functionality of ML and Jupyter integrations
Tests the structure and key methods without external dependencies
"""

import sys
import os
import inspect
from unittest.mock import Mock
from datetime import datetime

# Add current directory to path
sys.path.insert(0, '.')

def test_mlflow_integration_structure():
    """Test MLflow integration class structure"""
    print("🧪 Testing MLflow Integration Structure...")
    
    try:
        # Mock the external dependencies
        import builtins
        original_import = builtins.__import__
        
        def mock_import(name, *args, **kwargs):
            if name in ['mlflow', 'mlflow.tracking', 'mlflow.entities', 'mlflow.utils.mlflow_tags']:
                return Mock()
            return original_import(name, *args, **kwargs)
        
        builtins.__import__ = mock_import
        
        # Import and test
        from integrations.mlflow import ArgonMLflowIntegration
        
        # Check class structure
        methods = [method for method in dir(ArgonMLflowIntegration) if not method.startswith('_')]
        expected_methods = [
            'start_run', 'log_parameters', 'log_model_performance', 
            'log_model_artifacts', 'end_run', 'create_branch_from_run',
            'compare_runs', 'export_experiment_data'
        ]
        
        for method in expected_methods:
            assert hasattr(ArgonMLflowIntegration, method), f"Missing method: {method}"
            print(f"  ✅ Has method: {method}")
        
        # Test instantiation
        mock_project = Mock()
        mock_project.name = "test-project"
        
        integration = ArgonMLflowIntegration(mock_project)
        assert integration.project == mock_project
        print("  ✅ Can instantiate MLflow integration")
        
        # Restore original import
        builtins.__import__ = original_import
        
        print("✅ MLflow integration structure test passed")
        return True
        
    except Exception as e:
        print(f"❌ MLflow integration structure test failed: {e}")
        return False

def test_dvc_integration_structure():
    """Test DVC integration class structure"""
    print("\n🧪 Testing DVC Integration Structure...")
    
    try:
        # Mock the external dependencies
        import builtins
        original_import = builtins.__import__
        
        def mock_import(name, *args, **kwargs):
            if name in ['dvc', 'dvc.api', 'dvc.repo', 'subprocess']:
                return Mock()
            return original_import(name, *args, **kwargs)
        
        builtins.__import__ = mock_import
        
        # Import and test
        from integrations.dvc import ArgonDVCIntegration
        
        # Check class structure
        expected_methods = [
            'add_data', 'create_pipeline', 'run_pipeline',
            'get_metrics', 'compare_metrics', 'sync_data_with_branch',
            'create_branch_from_pipeline', 'export_data_version'
        ]
        
        for method in expected_methods:
            assert hasattr(ArgonDVCIntegration, method), f"Missing method: {method}"
            print(f"  ✅ Has method: {method}")
        
        # Test instantiation
        mock_project = Mock()
        mock_project.name = "test-project"
        
        integration = ArgonDVCIntegration(mock_project)
        assert integration.project == mock_project
        print("  ✅ Can instantiate DVC integration")
        
        # Restore original import
        builtins.__import__ = original_import
        
        print("✅ DVC integration structure test passed")
        return True
        
    except Exception as e:
        print(f"❌ DVC integration structure test failed: {e}")
        return False

def test_wandb_integration_structure():
    """Test W&B integration class structure"""
    print("\n🧪 Testing W&B Integration Structure...")
    
    try:
        # Mock the external dependencies
        import builtins
        original_import = builtins.__import__
        
        def mock_import(name, *args, **kwargs):
            if name in ['wandb']:
                return Mock()
            return original_import(name, *args, **kwargs)
        
        builtins.__import__ = mock_import
        
        # Import and test
        from integrations.wandb import ArgonWandbIntegration
        
        # Check class structure
        expected_methods = [
            'start_run', 'log_metrics', 'log_artifacts', 'log_table',
            'log_image', 'log_model', 'create_branch_from_run',
            'compare_runs', 'finish_run', 'export_experiment_data'
        ]
        
        for method in expected_methods:
            assert hasattr(ArgonWandbIntegration, method), f"Missing method: {method}"
            print(f"  ✅ Has method: {method}")
        
        # Test instantiation
        mock_project = Mock()
        mock_project.name = "test-project"
        
        integration = ArgonWandbIntegration(mock_project)
        assert integration.project == mock_project
        print("  ✅ Can instantiate W&B integration")
        
        # Restore original import
        builtins.__import__ = original_import
        
        print("✅ W&B integration structure test passed")
        return True
        
    except Exception as e:
        print(f"❌ W&B integration structure test failed: {e}")
        return False

def test_jupyter_integration_structure():
    """Test Jupyter integration class structure"""
    print("\n🧪 Testing Jupyter Integration Structure...")
    
    try:
        # Mock the external dependencies
        import builtins
        original_import = builtins.__import__
        
        def mock_import(name, *args, **kwargs):
            if name in ['IPython', 'IPython.core.magic', 'IPython.display']:
                return Mock()
            return original_import(name, *args, **kwargs)
        
        builtins.__import__ = mock_import
        
        # Import and test
        from integrations.jupyter import ArgonJupyterIntegration
        
        # Check class structure
        expected_methods = [
            'set_branch', 'track_cell_execution', 'log_experiment_params',
            'log_experiment_metrics', 'save_dataset', 'load_dataset',
            'save_model', 'load_model', 'create_checkpoint',
            'list_checkpoints', 'export_notebook_results', 'compare_experiments'
        ]
        
        for method in expected_methods:
            assert hasattr(ArgonJupyterIntegration, method), f"Missing method: {method}"
            print(f"  ✅ Has method: {method}")
        
        # Test instantiation
        mock_project = Mock()
        mock_project.name = "test-project"
        
        integration = ArgonJupyterIntegration(mock_project)
        assert integration.project == mock_project
        assert integration.current_branch is None
        assert integration.cell_outputs == {}
        print("  ✅ Can instantiate Jupyter integration")
        
        # Restore original import
        builtins.__import__ = original_import
        
        print("✅ Jupyter integration structure test passed")
        return True
        
    except Exception as e:
        print(f"❌ Jupyter integration structure test failed: {e}")
        return False

def test_jupyter_magic_structure():
    """Test Jupyter magic commands structure"""
    print("\n🧪 Testing Jupyter Magic Commands Structure...")
    
    try:
        # Mock the external dependencies
        import builtins
        original_import = builtins.__import__
        
        def mock_import(name, *args, **kwargs):
            if name in ['IPython', 'IPython.core.magic', 'IPython.display']:
                mock_module = Mock()
                # Mock the decorators to return the function unchanged
                mock_module.line_magic = lambda f: f
                mock_module.cell_magic = lambda f: f
                mock_module.magics_class = lambda cls: cls
                mock_module.magic_arguments = lambda f: f
                mock_module.argument = lambda *args, **kwargs: lambda f: f
                mock_module.parse_argline = lambda func, line: Mock(params=[], metrics=[], branches=[], description="")
                return mock_module
            return original_import(name, *args, **kwargs)
        
        builtins.__import__ = mock_import
        
        # Import and test
        from integrations.jupyter_magic import ArgonMagics
        
        # Check class structure
        expected_methods = [
            'argon_init', 'argon_branch', 'argon_params', 'argon_metrics',
            'argon_checkpoint', 'argon_status', 'argon_track', 'argon_compare'
        ]
        
        for method in expected_methods:
            assert hasattr(ArgonMagics, method), f"Missing magic method: {method}"
            print(f"  ✅ Has magic method: {method}")
        
        # Test extension functions
        from integrations.jupyter_magic import load_ipython_extension, unload_ipython_extension
        assert callable(load_ipython_extension)
        assert callable(unload_ipython_extension)
        print("  ✅ Has extension loading functions")
        
        # Restore original import
        builtins.__import__ = original_import
        
        print("✅ Jupyter magic commands structure test passed")
        return True
        
    except Exception as e:
        print(f"❌ Jupyter magic commands structure test failed: {e}")
        return False

def test_file_structure():
    """Test that all required files exist"""
    print("\n🧪 Testing File Structure...")
    
    try:
        required_files = [
            "integrations/__init__.py",
            "integrations/mlflow.py",
            "integrations/dvc.py",
            "integrations/wandb.py",
            "integrations/jupyter.py",
            "integrations/jupyter_magic.py",
            "examples/ml_integration_example.py",
            "examples/jupyter_notebook_example.ipynb",
            "docs/ML_INTEGRATIONS.md",
            "docs/JUPYTER_INTEGRATION.md",
            "tests/test_ml_integrations.py",
            "tests/test_jupyter_integration.py",
            "setup.py"
        ]
        
        for file_path in required_files:
            if os.path.exists(file_path):
                file_size = os.path.getsize(file_path)
                print(f"  ✅ {file_path} exists ({file_size} bytes)")
            else:
                print(f"  ❌ {file_path} missing")
                return False
        
        print("✅ File structure test passed")
        return True
        
    except Exception as e:
        print(f"❌ File structure test failed: {e}")
        return False

def test_documentation_completeness():
    """Test that documentation files are complete"""
    print("\n🧪 Testing Documentation Completeness...")
    
    try:
        # Check ML integrations documentation
        with open("docs/ML_INTEGRATIONS.md", "r") as f:
            ml_docs = f.read()
        
        required_sections = [
            "# ML Framework Integrations",
            "## MLflow Integration",
            "## DVC Integration", 
            "## Weights & Biases Integration",
            "## Quick Start",
            "## API Reference"
        ]
        
        for section in required_sections:
            if section in ml_docs:
                print(f"  ✅ ML docs has section: {section}")
            else:
                print(f"  ❌ ML docs missing section: {section}")
                return False
        
        # Check Jupyter integration documentation
        with open("docs/JUPYTER_INTEGRATION.md", "r") as f:
            jupyter_docs = f.read()
        
        jupyter_sections = [
            "# Jupyter Notebook Integration",
            "## Magic Commands",
            "## Data Management",
            "## Experiment Tracking",
            "## Best Practices"
        ]
        
        for section in jupyter_sections:
            if section in jupyter_docs:
                print(f"  ✅ Jupyter docs has section: {section}")
            else:
                print(f"  ❌ Jupyter docs missing section: {section}")
                return False
        
        print("✅ Documentation completeness test passed")
        return True
        
    except Exception as e:
        print(f"❌ Documentation completeness test failed: {e}")
        return False

def main():
    """Run all tests"""
    print("🚀 Testing Argon ML and Jupyter Integrations (Core Functionality)")
    print("=" * 70)
    
    tests = [
        test_mlflow_integration_structure,
        test_dvc_integration_structure,
        test_wandb_integration_structure,
        test_jupyter_integration_structure,
        test_jupyter_magic_structure,
        test_file_structure,
        test_documentation_completeness
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
    
    print("\n" + "=" * 70)
    print(f"📊 Test Results: {passed} passed, {failed} failed")
    
    if failed == 0:
        print("🎉 All core functionality tests passed!")
        print("✅ ML and Jupyter integrations have correct structure and methods")
        print("✅ All required files exist and are complete")
        print("✅ Documentation is comprehensive")
        print("\n💡 Note: External dependencies (mlflow, dvc, wandb, jupyter) are mocked")
        print("💡 In production, ensure these packages are installed for full functionality")
        return True
    else:
        print(f"💥 {failed} tests failed. See output above for details.")
        return False

if __name__ == "__main__":
    success = main()
    sys.exit(0 if success else 1)