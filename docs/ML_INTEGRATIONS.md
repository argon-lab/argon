# ML Framework Integrations

Argon provides seamless integrations with popular ML frameworks to enable experiment tracking and data versioning alongside MongoDB branch management.

## Overview

The ML integrations connect Argon's branching system with:
- **MLflow**: Experiment tracking and model registry
- **DVC**: Data version control and pipeline management  
- **Weights & Biases**: Experiment tracking and visualization

## Quick Start

```python
from argon.core.project import Project
from argon.integrations import create_mlflow_integration, create_dvc_integration, create_wandb_integration

# Initialize Argon project
project = Project("my-ml-project")
branch = project.create_branch("experiment-1")

# Create integrations
mlflow_integration = create_mlflow_integration(project)
dvc_integration = create_dvc_integration(project)
wandb_integration = create_wandb_integration(project)

# Start experiment
mlflow_run_id = mlflow_integration.start_run(branch)
wandb_run_id = wandb_integration.start_run(branch)

# Log experiments
mlflow_integration.log_parameters({"learning_rate": 0.01})
wandb_integration.log_metrics({"accuracy": 0.95})

# Add data to version control
dvc_integration.add_data("data/training_set.csv", branch)
```

## MLflow Integration

### Features

- **Automatic Experiment Creation**: Creates MLflow experiments matching Argon projects
- **Branch-Run Association**: Links MLflow runs to Argon branches
- **Metadata Tracking**: Stores branch info, commit hashes, and timestamps
- **Model Artifacts**: Logs models and artifacts with branch context
- **Run Comparison**: Compare experiments across different branches

### Usage

```python
from argon.integrations import create_mlflow_integration

# Create integration
mlflow_integration = create_mlflow_integration(project, tracking_uri="sqlite:///mlflow.db")

# Start run
run_id = mlflow_integration.start_run(branch, run_name="my-experiment")

# Log parameters and metrics
mlflow_integration.log_parameters({
    "learning_rate": 0.01,
    "batch_size": 32,
    "epochs": 100
})

mlflow_integration.log_model_performance({
    "accuracy": 0.95,
    "loss": 0.05,
    "f1_score": 0.94
})

# Log artifacts
mlflow_integration.log_model_artifacts("models/", "model_artifacts")

# End run
mlflow_integration.end_run("FINISHED")
```

### Branch Creation from Runs

```python
# Create branch from successful MLflow run
new_branch = mlflow_integration.create_branch_from_run(
    run_id="abc123",
    branch_name="production-candidate"
)
```

## DVC Integration

### Features

- **Data Version Control**: Track data files alongside code branches
- **Pipeline Management**: Create reproducible ML pipelines
- **Data Synchronization**: Sync data state with branch changes
- **Metrics Tracking**: Version control for model metrics
- **Pipeline Reproduction**: Recreate experiments from pipeline definitions

### Usage

```python
from argon.integrations import create_dvc_integration

# Create integration
dvc_integration = create_dvc_integration(project, dvc_repo_path="./")

# Add data to version control
dvc_file = dvc_integration.add_data("data/dataset.csv", branch)

# Create pipeline
pipeline_config = {
    "stages": {
        "preprocess": {
            "cmd": "python preprocess.py",
            "deps": ["data/raw_data.csv"],
            "outs": ["data/processed_data.csv"]
        },
        "train": {
            "cmd": "python train.py",
            "deps": ["data/processed_data.csv"],
            "outs": ["models/model.pkl"],
            "metrics": ["metrics.json"]
        }
    }
}

dvc_integration.create_pipeline(pipeline_config, branch)

# Run pipeline
result = dvc_integration.run_pipeline()
```

### Data Synchronization

```python
# Sync data with branch
dvc_integration.sync_data_with_branch(branch, "data/training_set.csv")

# Restore data state from branch
dvc_integration.restore_data_state(branch)

# Get metrics for branch
metrics = dvc_integration.get_metrics(branch)
```

## Weights & Biases Integration

### Features

- **Experiment Tracking**: Track experiments with rich visualizations
- **Artifact Logging**: Store models, images, and other artifacts
- **Run Comparison**: Compare experiments across branches
- **Report Generation**: Create reports for experiment results
- **Team Collaboration**: Share experiments with team members

### Usage

```python
from argon.integrations import create_wandb_integration

# Create integration
wandb_integration = create_wandb_integration(
    project, 
    wandb_project="my-ml-project",
    entity="my-team"
)

# Start run
run_id = wandb_integration.start_run(
    branch,
    run_name="experiment-1",
    config={"learning_rate": 0.01, "batch_size": 32},
    tags=["baseline", "cnn"],
    notes="Initial baseline experiment"
)

# Log metrics
wandb_integration.log_metrics({
    "epoch": 1,
    "train_loss": 0.5,
    "val_loss": 0.6,
    "accuracy": 0.85
})

# Log artifacts
wandb_integration.log_model("models/model.pkl", "cnn_model")
wandb_integration.log_image("training_plot", "plots/training_curve.png")

# Log tables
wandb_integration.log_table(
    "predictions",
    [["sample_1", 0.9, 1], ["sample_2", 0.1, 0]],
    ["sample_id", "prediction", "actual"]
)
```

### Advanced Features

```python
# Create branch from W&B run
new_branch = wandb_integration.create_branch_from_run(
    run_id="abc123",
    branch_name="best-model"
)

# Compare runs
comparison = wandb_integration.compare_runs([run_id_1, run_id_2])

# Sync branch with latest metrics
wandb_integration.sync_branch_metrics(branch)
```

## Combined Workflow

### Multi-Platform Experiment Tracking

```python
# Initialize all integrations
mlflow_integration = create_mlflow_integration(project)
dvc_integration = create_dvc_integration(project)
wandb_integration = create_wandb_integration(project)

# Create experiment branch
branch = project.create_branch("multi-platform-experiment")

# Start runs on all platforms
mlflow_run_id = mlflow_integration.start_run(branch)
wandb_run_id = wandb_integration.start_run(branch)

# Add data to version control
dvc_integration.add_data("data/", branch)

# Log parameters (same to all platforms)
params = {"learning_rate": 0.01, "batch_size": 32}
mlflow_integration.log_parameters(params)

# Log metrics (same to all platforms)
metrics = {"accuracy": 0.95, "loss": 0.05}
mlflow_integration.log_model_performance(metrics)
wandb_integration.log_metrics(metrics)

# Log artifacts
mlflow_integration.log_model_artifacts("models/")
wandb_integration.log_model("models/model.pkl", "final_model")

# Create DVC pipeline for reproducibility
pipeline_config = {
    "stages": {
        "train": {
            "cmd": "python train.py",
            "deps": ["data/", "train.py"],
            "outs": ["models/model.pkl"],
            "metrics": ["metrics.json"]
        }
    }
}
dvc_integration.create_pipeline(pipeline_config, branch)

# End runs
mlflow_integration.end_run("FINISHED")
wandb_integration.finish_run(0)
```

## Best Practices

### 1. Naming Conventions

- Use consistent naming across platforms
- Include branch name and commit hash in run names
- Use descriptive experiment names

```python
run_name = f"branch-{branch.name}-{branch.commit_hash[:8]}"
```

### 2. Metadata Tracking

- Store branch metadata in all platforms
- Include parent branch information
- Track creation timestamps

```python
# Store comprehensive metadata
metadata = {
    "argon_project": project.name,
    "argon_branch": branch.name,
    "argon_commit": branch.commit_hash,
    "argon_parent": branch.parent_branch,
    "created_at": branch.created_at.isoformat()
}
```

### 3. Data Versioning

- Version control all data files with DVC
- Sync data state with branch changes
- Use meaningful data file names

```python
# Version control data systematically
for data_file in ["train.csv", "val.csv", "test.csv"]:
    dvc_integration.add_data(f"data/{data_file}", branch)
```

### 4. Experiment Comparison

- Compare experiments across branches
- Use consistent metrics for comparison
- Export comparison results

```python
# Systematic comparison
comparison_data = {
    "mlflow": mlflow_integration.compare_runs([run1, run2]),
    "wandb": wandb_integration.compare_runs([run1, run2])
}
```

### 5. Error Handling

- Properly handle integration failures
- End runs with appropriate status
- Log errors for debugging

```python
try:
    # Run experiment
    pass
except Exception as e:
    mlflow_integration.end_run("FAILED")
    wandb_integration.finish_run(1)
    logger.error(f"Experiment failed: {e}")
```

## Configuration

### Environment Variables

```bash
# MLflow
export MLFLOW_TRACKING_URI=sqlite:///mlflow.db

# DVC
export DVC_REMOTE=s3://my-bucket/dvc-store

# Weights & Biases
export WANDB_API_KEY=your-api-key
export WANDB_ENTITY=your-team
```

### Integration Settings

```python
# MLflow settings
mlflow_integration = create_mlflow_integration(
    project,
    tracking_uri="postgresql://user:pass@localhost/mlflow"
)

# DVC settings
dvc_integration = create_dvc_integration(
    project,
    dvc_repo_path="/path/to/dvc/repo"
)

# W&B settings
wandb_integration = create_wandb_integration(
    project,
    wandb_project="my-project",
    entity="my-team"
)
```

## Examples

See `examples/ml_integration_example.py` for a complete example demonstrating all integrations working together.

## Troubleshooting

### Common Issues

1. **MLflow Connection Issues**
   - Check tracking URI
   - Verify database permissions
   - Ensure MLflow server is running

2. **DVC Setup Problems**
   - Run `dvc init` in repository
   - Check DVC remote configuration
   - Verify file permissions

3. **W&B Authentication**
   - Set `WANDB_API_KEY` environment variable
   - Run `wandb login` command
   - Check entity permissions

### Debug Mode

Enable debug logging for troubleshooting:

```python
import logging
logging.basicConfig(level=logging.DEBUG)
```

## API Reference

### MLflow Integration

- `ArgonMLflowIntegration.start_run(branch, run_name=None)`
- `ArgonMLflowIntegration.log_parameters(params)`
- `ArgonMLflowIntegration.log_model_performance(metrics)`
- `ArgonMLflowIntegration.log_model_artifacts(path)`
- `ArgonMLflowIntegration.end_run(status)`

### DVC Integration

- `ArgonDVCIntegration.add_data(data_path, branch)`
- `ArgonDVCIntegration.create_pipeline(config, branch)`
- `ArgonDVCIntegration.run_pipeline(stage=None)`
- `ArgonDVCIntegration.get_metrics(branch)`

### W&B Integration

- `ArgonWandbIntegration.start_run(branch, run_name=None, config=None)`
- `ArgonWandbIntegration.log_metrics(metrics)`
- `ArgonWandbIntegration.log_artifacts(artifacts)`
- `ArgonWandbIntegration.log_model(model_path, model_name)`
- `ArgonWandbIntegration.finish_run(exit_code)`