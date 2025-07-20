"""
ML Framework Integrations for Argon

This package provides integrations with popular ML frameworks to enable
seamless experiment tracking and data versioning alongside Argon branches.
"""

try:
    from .mlflow import ArgonMLflowIntegration, create_mlflow_integration
except ImportError:
    ArgonMLflowIntegration = None
    create_mlflow_integration = None

try:
    from .dvc import ArgonDVCIntegration, create_dvc_integration
except ImportError:
    ArgonDVCIntegration = None
    create_dvc_integration = None

try:
    from .wandb import ArgonWandbIntegration, create_wandb_integration
except ImportError:
    ArgonWandbIntegration = None
    create_wandb_integration = None

try:
    from .jupyter import ArgonJupyterIntegration, create_jupyter_integration
except ImportError:
    ArgonJupyterIntegration = None
    create_jupyter_integration = None

__all__ = [
    'ArgonMLflowIntegration',
    'ArgonDVCIntegration', 
    'ArgonWandbIntegration',
    'ArgonJupyterIntegration',
    'create_mlflow_integration',
    'create_dvc_integration',
    'create_wandb_integration',
    'create_jupyter_integration'
]

__version__ = '1.0.0'