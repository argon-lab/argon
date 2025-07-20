"""
Argon Core Python API

Provides Python bindings for Argon's MongoDB branching system.
"""

from .project import Project
from .branch import Branch
from .client import ArgonClient

__all__ = ['Project', 'Branch', 'ArgonClient']
__version__ = '1.0.0'