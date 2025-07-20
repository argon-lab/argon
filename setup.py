"""
Setup script for Argon Python SDK
"""

from setuptools import setup, find_packages

setup(
    name="argon-python",
    version="1.0.0",
    description="Python SDK for Argon - MongoDB branching for ML and data science",
    long_description=open("README.md").read(),
    long_description_content_type="text/markdown",
    author="Argon Team",
    author_email="hello@argon.dev",
    url="https://github.com/argon-lab/argon",
    packages=find_packages(),
    install_requires=[
        "IPython>=7.0.0",
        "jupyter>=1.0.0",
        "pandas>=1.0.0",
        "numpy>=1.18.0",
        "matplotlib>=3.0.0",
        "scikit-learn>=0.24.0",
        "joblib>=1.0.0",
    ],
    extras_require={
        "ml": [
            "mlflow>=1.20.0",
            "dvc>=2.0.0",
            "wandb>=0.12.0",
        ],
        "dev": [
            "pytest>=6.0.0",
            "pytest-cov>=2.10.0",
            "black>=21.0.0",
            "flake8>=3.8.0",
        ],
    },
    classifiers=[
        "Development Status :: 4 - Beta",
        "Intended Audience :: Developers",
        "Intended Audience :: Science/Research",
        "License :: OSI Approved :: MIT License",
        "Operating System :: OS Independent",
        "Programming Language :: Python :: 3",
        "Programming Language :: Python :: 3.7",
        "Programming Language :: Python :: 3.8",
        "Programming Language :: Python :: 3.9",
        "Programming Language :: Python :: 3.10",
        "Topic :: Scientific/Engineering",
        "Topic :: Software Development :: Libraries :: Python Modules",
        "Topic :: Database",
        "Topic :: Scientific/Engineering :: Artificial Intelligence",
    ],
    python_requires=">=3.7",
    keywords="mongodb, database, branching, jupyter, ml, data-science, version-control",
    project_urls={
        "Bug Reports": "https://github.com/argon-lab/argon/issues",
        "Source": "https://github.com/argon-lab/argon",
        "Documentation": "https://docs.argon.dev",
    },
)