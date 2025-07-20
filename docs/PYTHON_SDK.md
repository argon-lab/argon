# Python SDK Reference

Complete guide to using Argon's Python SDK for ML and data science workflows.

## 🚀 **Quick Start**

### Installation
```bash
# Clone the repository and install
git clone https://github.com/argon-lab/argon.git
cd argon
pip install -e .
```

### Basic Usage
```python
from core.project import Project
from integrations.jupyter import init_argon_notebook

# Create a project
project = Project("ml-experiment")
print(f"Created project: {project.name} (ID: {project.id})")

# Create branches for experiments
main_branch = project.get_branch("main")
exp_branch = project.create_branch("experiment-v1")
```

## 📊 **Core API Reference**

### **Project Class**
```python
from core.project import Project

# Create or connect to existing project
project = Project("my-ml-project")

# Properties
project.name        # Project name
project.id          # Unique project ID
project.project_id  # Alias for id

# Methods
branch = project.get_branch("branch-name")           # Get existing branch
branch = project.create_branch("new-branch")         # Create new branch
branches = project.list_branches()                   # List all branches
status = project.get_status()                        # Get project status
```

### **Branch Class**
```python
# Get branch from project
branch = project.get_branch("main")

# Properties
branch.name            # Branch name
branch.project_id      # Parent project ID
branch.branch_id       # Unique branch ID
branch.commit_hash     # Current commit (LSN as string)
branch.created_at      # Creation timestamp

# Time travel methods
info = branch.get_info()                             # Get branch info
lsn_start, lsn_end = branch.get_lsn_range()         # Get LSN range

# Checkpoint management
branch.create_checkpoint("checkpoint-name", "description")
checkpoints = branch.list_checkpoints()

# Metadata management
branch.metadata["custom_key"] = "custom_value"
branch.save()  # Save metadata
```

### **Client Class**
```python
from core.client import ArgonClient

# Initialize client
client = ArgonClient()  # Uses default MongoDB connection
client = ArgonClient("mongodb://custom-host:27017")  # Custom connection

# Direct operations
status = client.get_status()
projects = client.list_projects()
project_data = client.create_project("new-project")
info = client.get_time_travel_info(project_id, branch_name)
```

## 🧪 **ML/Data Science Integration**

### **Jupyter Notebook Integration**
```python
from integrations.jupyter import init_argon_notebook

# Initialize Argon for notebooks
jupyter = init_argon_notebook("ml-project")

# Set working branch
branch = jupyter.set_branch("experiment-1")

# Log experiment parameters
jupyter.log_experiment_params({
    "model": "RandomForest",
    "n_estimators": 100,
    "max_depth": 10,
    "learning_rate": 0.01
})

# Log experiment metrics
jupyter.log_experiment_metrics({
    "accuracy": 0.94,
    "precision": 0.92,
    "recall": 0.91,
    "f1_score": 0.915
})

# Save datasets
jupyter.save_dataset(df, "training_data", "Cleaned training dataset")
loaded_df = jupyter.load_dataset("training_data")

# Save models
jupyter.save_model(model, "random_forest_v1", "Initial RF model")
loaded_model = jupyter.load_model("random_forest_v1")

# Create checkpoints
jupyter.create_checkpoint("model_v1", "First working model with 94% accuracy")

# Compare experiments across branches
comparison = jupyter.compare_experiments(["baseline", "experiment-1", "experiment-2"])
```

### **MLflow Integration**
```python
from integrations.mlflow import create_mlflow_integration

# Create MLflow integration
mlflow_integration = create_mlflow_integration(project)

# Start experiment run
run_id = mlflow_integration.start_run("experiment-branch")

# Log parameters and metrics (automatically tied to Argon branch)
mlflow_integration.log_params({"lr": 0.01, "epochs": 100})
mlflow_integration.log_metrics({"accuracy": 0.95, "loss": 0.05})

# End run
mlflow_integration.end_run()
```

### **Weights & Biases Integration**
```python
from integrations.wandb import create_wandb_integration

# Create W&B integration
wandb_integration = create_wandb_integration(project)

# Initialize run (tied to Argon branch)
wandb_integration.init_run("experiment-branch", config={"lr": 0.01})

# Log metrics (automatically associated with branch)
wandb_integration.log_metrics({"accuracy": 0.95})
```

## 📈 **Advanced Workflows**

### **Multi-Branch Experiment Tracking**
```python
from integrations.jupyter import init_argon_notebook

jupyter = init_argon_notebook("advanced-ml-pipeline")

# Set up multiple experiment branches
experiments = [
    ("baseline", {"model": "LogisticRegression"}),
    ("feature-eng", {"model": "XGBoost", "features": "engineered"}),
    ("hypertuned", {"model": "XGBoost", "tuned": True}),
    ("ensemble", {"model": "VotingClassifier", "n_models": 3})
]

results = {}
for branch_name, params in experiments:
    # Switch to experiment branch
    jupyter.set_branch(branch_name)
    
    # Log parameters
    jupyter.log_experiment_params(params)
    
    # Run experiment (your ML code here)
    accuracy = run_experiment(params)  # Your function
    
    # Log results
    jupyter.log_experiment_metrics({"accuracy": accuracy})
    jupyter.create_checkpoint(f"{branch_name}_final")
    
    results[branch_name] = accuracy

# Compare all experiments
comparison = jupyter.compare_experiments(list(results.keys()))
print("Experiment Results:")
for branch_data in comparison["branches"]:
    name = branch_data["name"]
    acc = branch_data.get("metrics", {}).get("accuracy", "N/A")
    print(f"  {name}: {acc}")
```

### **Reproducible Research Pipeline**
```python
def create_reproducible_experiment(project_name, experiment_name):
    """Create a fully reproducible ML experiment"""
    
    # Initialize project and branch
    jupyter = init_argon_notebook(project_name)
    branch = jupyter.set_branch(experiment_name)
    
    # Log environment info
    jupyter.log_experiment_params({
        "python_version": sys.version,
        "timestamp": datetime.now().isoformat(),
        "git_commit": get_git_commit(),  # Your function
        "dependencies": get_package_versions()  # Your function
    })
    
    # Create initial checkpoint
    jupyter.create_checkpoint("experiment_start", "Experiment initialization")
    
    return jupyter

# Usage
jupyter = create_reproducible_experiment("research-project", "hypothesis-1")

# Your experiment code
data = load_and_preprocess_data()
jupyter.save_dataset(data, "preprocessed", "Cleaned and preprocessed data")
jupyter.create_checkpoint("data_ready", "Data preprocessing complete")

model = train_model(data)
jupyter.save_model(model, "trained_model", "Initial trained model")
jupyter.create_checkpoint("model_trained", "Model training complete")

results = evaluate_model(model, test_data)
jupyter.log_experiment_metrics(results)
jupyter.create_checkpoint("evaluation_complete", "Model evaluation finished")
```

## 🔧 **Configuration**

### **Environment Variables**
```bash
# MongoDB connection
export MONGODB_URI="mongodb://localhost:27017"

# Enable WAL system
export ENABLE_WAL=true

# Optional: Custom database name
export ARGON_DATABASE="argon_wal"
```

### **Python Configuration**
```python
# Custom MongoDB connection
from core.client import ArgonClient

client = ArgonClient("mongodb://custom-host:27017")
project = Project("my-project", client=client)
```

## 🛠️ **Error Handling**

### **Common Error Patterns**
```python
from core.project import Project

try:
    project = Project("my-project")
    branch = project.create_branch("new-branch")
except RuntimeError as e:
    if "CLI not found" in str(e):
        print("Argon CLI not installed. Run: brew install argon-lab/tap/argonctl")
    elif "connection refused" in str(e):
        print("MongoDB not running. Start MongoDB and try again.")
    else:
        print(f"Error: {e}")
```

### **Validation**
```python
# Check system status before operations
client = ArgonClient()
status = client.get_status()

if "Connection: OK" not in status.get("output", ""):
    raise RuntimeError("Argon system not ready. Check MongoDB connection.")

# Proceed with operations
project = Project("my-project")
```

## 📝 **Examples**

### **Complete ML Workflow Example**
```python
#!/usr/bin/env python3
"""Complete ML workflow with Argon branching"""

from core.project import Project
from integrations.jupyter import init_argon_notebook
import pandas as pd
from sklearn.ensemble import RandomForestClassifier
from sklearn.model_selection import train_test_split
from sklearn.metrics import accuracy_score

def main():
    # Initialize Argon
    jupyter = init_argon_notebook("sklearn-demo")
    jupyter.set_branch("random-forest-experiment")
    
    # Load and save data
    # df = pd.read_csv("your_data.csv")  # Your data
    # jupyter.save_dataset(df, "raw_data", "Original dataset")
    
    # Log experiment setup
    jupyter.log_experiment_params({
        "algorithm": "RandomForest",
        "n_estimators": 100,
        "max_depth": 10,
        "test_size": 0.2,
        "random_state": 42
    })
    
    jupyter.create_checkpoint("experiment_start", "Experiment initialized")
    
    # Train model (replace with your data and target)
    # X_train, X_test, y_train, y_test = train_test_split(
    #     df.drop('target', axis=1), df['target'], 
    #     test_size=0.2, random_state=42
    # )
    # 
    # model = RandomForestClassifier(n_estimators=100, max_depth=10, random_state=42)
    # model.fit(X_train, y_train)
    # 
    # # Save model and evaluate
    # jupyter.save_model(model, "random_forest", "Trained RandomForest model")
    # 
    # y_pred = model.predict(X_test)
    # accuracy = accuracy_score(y_test, y_pred)
    
    # For demo purposes
    accuracy = 0.94
    
    # Log results
    jupyter.log_experiment_metrics({
        "accuracy": accuracy,
        "model_size": "1.2MB"  # example
    })
    
    jupyter.create_checkpoint("experiment_complete", f"Model trained with {accuracy:.2%} accuracy")
    
    print(f"✅ Experiment complete! Accuracy: {accuracy:.2%}")
    print("🔍 View results with: argon wal-simple tt-info -p <project-id> -b random-forest-experiment")

if __name__ == "__main__":
    main()
```

## 🆘 **Troubleshooting**

### **Common Issues**

1. **"Argon CLI not found"**
   ```bash
   # Install Argon CLI
   brew install argon-lab/tap/argonctl
   # Or download directly from GitHub releases
   ```

2. **"Connection refused"**
   ```bash
   # Start MongoDB
   mongod --dbpath /path/to/db
   # Or use Docker
   docker run -d -p 27017:27017 mongo
   ```

3. **"Branch not found"**
   - This is expected for new branches until they have WAL entries
   - Branch objects work in Python even if not yet in WAL system

4. **Import errors**
   ```bash
   # Install in development mode
   pip install -e .
   ```

### **Debug Mode**
```python
import logging
logging.basicConfig(level=logging.DEBUG)

# Now all Argon operations will show debug info
```

## 🔗 **Related Documentation**

- [Go SDK Reference](./GO_SDK.md) - For Go applications
- [CLI Reference](./CLI_REFERENCE.md) - Command-line usage
- [ML Integrations](./ML_INTEGRATIONS.md) - Advanced ML workflows
- [Performance](./PERFORMANCE.md) - Benchmark data