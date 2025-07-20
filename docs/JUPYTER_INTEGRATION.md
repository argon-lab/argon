# Jupyter Notebook Integration

Argon provides seamless integration with Jupyter notebooks, enabling data scientists to track experiments, manage datasets, and create reproducible workflows directly from their notebooks.

## Installation

```bash
# Install Argon with Jupyter support
pip install argon-jupyter

# Or install with all ML integrations
pip install argon-jupyter[ml]
```

## Quick Start

### 1. Load the Extension

```python
# Load the Argon extension in your notebook
%load_ext argon.integrations.jupyter_magic
```

### 2. Initialize Argon

```python
# Initialize Argon for your project
%argon_init my-ml-project
```

### 3. Set Working Branch

```python
# Create/switch to a branch for your experiment
%argon_branch experiment-1
```

### 4. Track Your Work

```python
# Log experiment parameters
%argon_params learning_rate=0.01 batch_size=32 epochs=100

# Track cell execution
%%argon_track
# Your ML code here
model = train_model(X_train, y_train)
predictions = model.predict(X_test)

# Log metrics
%argon_metrics accuracy=0.95 loss=0.05
```

## Magic Commands

### Line Magics

#### `%argon_init`
Initialize Argon for the current notebook.

```python
%argon_init my-project-name
```

#### `%argon_branch`
Set or create a working branch.

```python
%argon_branch experiment-1
```

#### `%argon_params`
Log experiment parameters.

```python
%argon_params learning_rate=0.01 batch_size=32 epochs=100
```

#### `%argon_metrics`
Log experiment metrics.

```python
%argon_metrics accuracy=0.95 precision=0.94 recall=0.95
```

#### `%argon_checkpoint`
Create a checkpoint of current state.

```python
%argon_checkpoint milestone-1 --description "Completed data preprocessing"
```

#### `%argon_status`
Show current Argon status.

```python
%argon_status
```

#### `%argon_compare`
Compare experiments across branches.

```python
%argon_compare experiment-1 experiment-2 baseline
```

### Cell Magics

#### `%%argon_track`
Track execution of an entire cell.

```python
%%argon_track
# This entire cell will be tracked
import pandas as pd
df = pd.read_csv('data.csv')
model = train_model(df)
```

## Python API

For more advanced usage, you can use the Python API directly:

```python
from argon.integrations.jupyter import init_argon_notebook, get_argon_integration

# Initialize
integration = init_argon_notebook("my-project")

# Set branch
branch = integration.set_branch("experiment-1")

# Save datasets
integration.save_dataset(df, "training_data", "Processed training dataset")

# Save models
integration.save_model(model, "classifier_v1", "Random Forest classifier")

# Load saved artifacts
loaded_df = integration.load_dataset("training_data")
loaded_model = integration.load_model("classifier_v1")
```

## Data Management

### Saving Datasets

```python
# Using magic commands
integration = get_argon_integration()
integration.save_dataset(df, "my_dataset", "Description of dataset")

# Automatic format detection
# - pandas DataFrames → CSV
# - numpy arrays → pickle
# - other objects → pickle
```

### Loading Datasets

```python
# Load previously saved dataset
df = integration.load_dataset("my_dataset")
```

### Model Management

```python
# Save trained models
integration.save_model(
    model, 
    "model_v1", 
    "Random Forest classifier",
    metadata={"algorithm": "RandomForest", "features": 20}
)

# Load models
model = integration.load_model("model_v1")
```

## Experiment Tracking

### Parameters and Metrics

```python
# Log parameters
integration.log_experiment_params({
    "learning_rate": 0.01,
    "batch_size": 32,
    "epochs": 100
})

# Log metrics
integration.log_experiment_metrics({
    "accuracy": 0.95,
    "loss": 0.05,
    "f1_score": 0.94
})
```

### Checkpoints

```python
# Create checkpoints for important milestones
integration.create_checkpoint("data_cleaned", "Data preprocessing completed")
integration.create_checkpoint("model_trained", "Initial model training done")
integration.create_checkpoint("hyperparams_tuned", "Hyperparameter tuning completed")

# List checkpoints
checkpoints = integration.list_checkpoints()
for checkpoint in checkpoints:
    print(f"{checkpoint['name']}: {checkpoint['description']}")
```

### Cell Execution Tracking

```python
# Track individual cell executions
integration.track_cell_execution(
    cell_id="cell_1",
    code="model.fit(X_train, y_train)",
    output="Model training completed",
    execution_time=45.2
)
```

## Branch Management

### Creating and Switching Branches

```python
# Create new branch for different experiment
%argon_branch hyperparameter-tuning

# Switch back to previous branch
%argon_branch experiment-1
```

### Comparing Experiments

```python
# Compare multiple experiments
comparison = integration.compare_experiments([
    "experiment-1", 
    "hyperparameter-tuning", 
    "feature-engineering"
])

# Display comparison
%argon_compare experiment-1 hyperparameter-tuning feature-engineering
```

## Export and Sharing

### Export Results

```python
# Export all notebook results
integration.export_notebook_results("experiment_results.json")

# The exported file contains:
# - Branch information
# - All notebook sessions
# - Parameters and metrics
# - Dataset and model metadata
# - Checkpoints
```

### Sharing with Team

```python
# Team members can load your branch
%argon_init shared-project
%argon_branch experiment-1

# Access your saved datasets and models
df = integration.load_dataset("training_data")
model = integration.load_model("classifier_v1")
```

## Integration with ML Frameworks

### MLflow Integration

```python
from argon.integrations.mlflow import create_mlflow_integration

# Create MLflow integration
mlflow_integration = create_mlflow_integration(integration.project)

# Start MLflow run tied to current branch
mlflow_run_id = mlflow_integration.start_run(integration.current_branch)

# Log to both Argon and MLflow
%argon_metrics accuracy=0.95
mlflow_integration.log_model_performance({"accuracy": 0.95})
```

### Weights & Biases Integration

```python
from argon.integrations.wandb import create_wandb_integration

# Create W&B integration
wandb_integration = create_wandb_integration(integration.project)

# Start W&B run tied to current branch
wandb_run_id = wandb_integration.start_run(integration.current_branch)

# Log to both platforms
%argon_metrics accuracy=0.95
wandb_integration.log_metrics({"accuracy": 0.95})
```

## Best Practices

### 1. Branch Naming

Use descriptive branch names that indicate the experiment purpose:

```python
%argon_branch baseline-random-forest
%argon_branch feature-engineering-pca
%argon_branch hyperparameter-tuning-xgboost
%argon_branch ensemble-voting-classifier
```

### 2. Regular Checkpoints

Create checkpoints at key milestones:

```python
%argon_checkpoint data-loaded --description "Raw data loaded and validated"
%argon_checkpoint data-cleaned --description "Data preprocessing completed"
%argon_checkpoint model-trained --description "Initial model training done"
%argon_checkpoint final-model --description "Final model ready for deployment"
```

### 3. Consistent Parameter Logging

Log parameters at the start of each experiment:

```python
%argon_params model_type=RandomForest n_estimators=100 max_depth=10 random_state=42
```

### 4. Comprehensive Metrics

Track multiple metrics for complete evaluation:

```python
%argon_metrics accuracy=0.95 precision=0.94 recall=0.95 f1_score=0.94 auc_roc=0.97
```

### 5. Document Your Work

Use descriptive names and descriptions:

```python
integration.save_dataset(
    processed_df, 
    "preprocessed_customer_data", 
    "Customer data after feature engineering and outlier removal"
)

integration.save_model(
    final_model, 
    "production_classifier", 
    "Random Forest classifier tuned for production deployment",
    metadata={"performance": "95% accuracy", "data_version": "v2.1"}
)
```

## Troubleshooting

### Common Issues

1. **Extension Not Loading**
   ```python
   # Ensure Argon is installed
   !pip install argon-jupyter
   
   # Reload extension
   %reload_ext argon.integrations.jupyter_magic
   ```

2. **No Active Branch**
   ```python
   # Always set a branch after initialization
   %argon_init my-project
   %argon_branch my-experiment
   ```

3. **Data Not Saving**
   ```python
   # Check if integration is initialized
   %argon_status
   
   # Ensure you have write permissions
   import os
   print(f"Current directory: {os.getcwd()}")
   ```

### Debug Mode

Enable debug logging for troubleshooting:

```python
import logging
logging.basicConfig(level=logging.DEBUG)

# Now magic commands will show debug information
%argon_status
```

## Examples

### Complete ML Workflow

```python
# 1. Setup
%load_ext argon.integrations.jupyter_magic
%argon_init customer-churn-prediction
%argon_branch initial-exploration

# 2. Data Loading
%%argon_track
import pandas as pd
df = pd.read_csv('customer_data.csv')
integration = get_argon_integration()
integration.save_dataset(df, "raw_customer_data", "Raw customer data from CSV")

# 3. Preprocessing
%argon_checkpoint data-loaded --description "Raw data loaded successfully"

%%argon_track
# Data preprocessing code
processed_df = preprocess_data(df)
integration.save_dataset(processed_df, "processed_data", "Cleaned and preprocessed data")

# 4. Model Training
%argon_params model=RandomForest n_estimators=100 test_size=0.2 random_state=42

%%argon_track
from sklearn.ensemble import RandomForestClassifier
model = RandomForestClassifier(n_estimators=100, random_state=42)
model.fit(X_train, y_train)

# 5. Evaluation
%%argon_track
predictions = model.predict(X_test)
accuracy = accuracy_score(y_test, predictions)

%argon_metrics accuracy=0.92 precision=0.89 recall=0.94

# 6. Save Model
integration.save_model(model, "churn_classifier", "Random Forest for customer churn prediction")

# 7. Create Final Checkpoint
%argon_checkpoint model-complete --description "Model trained and evaluated successfully"
```

### Hyperparameter Tuning

```python
# Create new branch for tuning
%argon_branch hyperparameter-tuning

# Try different parameters
for n_est in [50, 100, 200]:
    for max_d in [10, 20, None]:
        %argon_params n_estimators={n_est} max_depth={max_d}
        
        %%argon_track
        model = RandomForestClassifier(n_estimators=n_est, max_depth=max_d)
        model.fit(X_train, y_train)
        acc = accuracy_score(y_test, model.predict(X_test))
        
        %argon_metrics accuracy={acc}
        
        if acc > best_accuracy:
            integration.save_model(model, f"model_nest{n_est}_maxd{max_d}", f"Model with {n_est} estimators and max_depth {max_d}")
```

### Model Comparison

```python
# Compare different approaches
%argon_compare baseline-random-forest feature-engineering-pca hyperparameter-tuning-xgboost

# Export comparison results
integration.export_notebook_results("model_comparison_results.json")
```

## Advanced Features

### Custom Cell Tracking

```python
import time
from argon.integrations.jupyter import get_argon_integration

def track_execution(func):
    """Decorator to track function execution"""
    def wrapper(*args, **kwargs):
        start_time = time.time()
        result = func(*args, **kwargs)
        execution_time = time.time() - start_time
        
        integration = get_argon_integration()
        if integration:
            integration.track_cell_execution(
                cell_id=func.__name__,
                code=f"Function: {func.__name__}",
                output=str(result),
                execution_time=execution_time
            )
        
        return result
    return wrapper

@track_execution
def train_model(X, y):
    # Your training code here
    return model
```

### Batch Operations

```python
# Save multiple datasets at once
datasets = {
    "train_data": X_train,
    "test_data": X_test,
    "validation_data": X_val
}

for name, data in datasets.items():
    integration.save_dataset(data, name, f"Dataset: {name}")
```

### Integration with Other Tools

```python
# Use with pandas profiling
import pandas_profiling

profile = pandas_profiling.ProfileReport(df)
profile.to_file("data_profile.html")

# Save profile as dataset
integration.save_dataset(profile, "data_profile", "Pandas profiling report")
```

## API Reference

### ArgonJupyterIntegration

- `set_branch(branch_name)` - Set working branch
- `track_cell_execution(cell_id, code, output, execution_time)` - Track cell execution
- `log_experiment_params(params)` - Log parameters
- `log_experiment_metrics(metrics)` - Log metrics
- `save_dataset(data, name, description)` - Save dataset
- `load_dataset(name)` - Load dataset
- `save_model(model, name, description, metadata)` - Save model
- `load_model(name)` - Load model
- `create_checkpoint(name, description)` - Create checkpoint
- `list_checkpoints()` - List checkpoints
- `export_notebook_results(output_path)` - Export results
- `compare_experiments(branch_names)` - Compare experiments

### Magic Commands

- `%argon_init project_name` - Initialize Argon
- `%argon_branch branch_name` - Set branch
- `%argon_params key=value ...` - Log parameters
- `%argon_metrics key=value ...` - Log metrics
- `%argon_checkpoint name --description "desc"` - Create checkpoint
- `%argon_status` - Show status
- `%argon_compare branch1 branch2 ...` - Compare branches
- `%%argon_track` - Track cell execution

## Contributing

We welcome contributions to improve Jupyter integration! See our [Contributing Guide](../CONTRIBUTING.md) for details.