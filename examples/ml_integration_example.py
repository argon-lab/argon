"""
Example demonstrating ML framework integrations with Argon

This example shows how to use MLflow, DVC, and W&B integrations
to track experiments alongside Argon branches.
"""

import os
import numpy as np
from sklearn.ensemble import RandomForestClassifier
from sklearn.model_selection import train_test_split
from sklearn.metrics import accuracy_score, precision_score, recall_score
from sklearn.datasets import make_classification
import joblib
import json

# Import Argon and integrations
from argon.core.project import Project
from argon.integrations import create_mlflow_integration, create_dvc_integration, create_wandb_integration

def create_sample_data():
    """Create sample classification data"""
    X, y = make_classification(
        n_samples=1000,
        n_features=20,
        n_informative=10,
        n_redundant=5,
        n_clusters_per_class=1,
        random_state=42
    )
    
    X_train, X_test, y_train, y_test = train_test_split(
        X, y, test_size=0.2, random_state=42
    )
    
    return X_train, X_test, y_train, y_test

def train_model(X_train, y_train, n_estimators=100, max_depth=10):
    """Train a random forest model"""
    model = RandomForestClassifier(
        n_estimators=n_estimators,
        max_depth=max_depth,
        random_state=42
    )
    model.fit(X_train, y_train)
    return model

def evaluate_model(model, X_test, y_test):
    """Evaluate model performance"""
    y_pred = model.predict(X_test)
    
    metrics = {
        'accuracy': accuracy_score(y_test, y_pred),
        'precision': precision_score(y_test, y_pred, average='weighted'),
        'recall': recall_score(y_test, y_pred, average='weighted')
    }
    
    return metrics

def main():
    """Main experiment workflow"""
    print("🚀 Starting ML Integration Example")
    
    # Initialize Argon project
    project = Project("ml-experiment-demo")
    
    # Create a new branch for this experiment
    branch = project.create_branch("random-forest-experiment")
    print(f"📁 Created Argon branch: {branch.name}")
    
    # Initialize ML framework integrations
    mlflow_integration = create_mlflow_integration(project)
    dvc_integration = create_dvc_integration(project)
    wandb_integration = create_wandb_integration(project)
    
    # Prepare experiment configuration
    config = {
        'model_type': 'RandomForest',
        'n_estimators': 100,
        'max_depth': 10,
        'test_size': 0.2,
        'random_state': 42
    }
    
    # Start experiment runs
    print("🔬 Starting experiment runs...")
    
    # Start MLflow run
    mlflow_run_id = mlflow_integration.start_run(branch, "random-forest-v1")
    mlflow_integration.log_parameters(config)
    
    # Start W&B run
    wandb_run_id = wandb_integration.start_run(
        branch, 
        "random-forest-v1",
        config=config,
        tags=['random-forest', 'classification', 'sklearn'],
        notes="Experimenting with random forest hyperparameters"
    )
    
    try:
        # Create sample data
        print("📊 Creating sample data...")
        X_train, X_test, y_train, y_test = create_sample_data()
        
        # Save data to files (for DVC tracking)
        os.makedirs("data", exist_ok=True)
        np.save("data/X_train.npy", X_train)
        np.save("data/X_test.npy", X_test)
        np.save("data/y_train.npy", y_train)
        np.save("data/y_test.npy", y_test)
        
        # Add data to DVC
        dvc_integration.add_data("data", branch)
        
        # Train model
        print("🤖 Training model...")
        model = train_model(X_train, y_train, 
                          n_estimators=config['n_estimators'],
                          max_depth=config['max_depth'])
        
        # Save model
        os.makedirs("models", exist_ok=True)
        model_path = "models/random_forest_model.pkl"
        joblib.dump(model, model_path)
        
        # Evaluate model
        print("📈 Evaluating model...")
        metrics = evaluate_model(model, X_test, y_test)
        
        # Log metrics to all platforms
        mlflow_integration.log_model_performance(metrics)
        wandb_integration.log_metrics(metrics)
        
        # Log model artifacts
        mlflow_integration.log_model_artifacts("models")
        wandb_integration.log_model(model_path, "random_forest_model", metadata=config)
        
        # Create DVC pipeline for reproducibility
        pipeline_config = {
            "stages": {
                "train": {
                    "cmd": "python ml_integration_example.py",
                    "deps": ["data/X_train.npy", "data/y_train.npy"],
                    "outs": ["models/random_forest_model.pkl"],
                    "metrics": ["metrics.json"]
                },
                "evaluate": {
                    "cmd": "python evaluate_model.py",
                    "deps": ["models/random_forest_model.pkl", "data/X_test.npy", "data/y_test.npy"],
                    "metrics": ["evaluation_metrics.json"]
                }
            }
        }
        
        dvc_integration.create_pipeline(pipeline_config, branch)
        
        # Save metrics to file for DVC
        with open("metrics.json", "w") as f:
            json.dump(metrics, f, indent=2)
        
        # Log additional W&B artifacts
        wandb_integration.log_table(
            "feature_importance",
            [[i, importance] for i, importance in enumerate(model.feature_importances_)],
            ["feature_index", "importance"]
        )
        
        print("✅ Experiment completed successfully!")
        print(f"📊 Results:")
        for metric, value in metrics.items():
            print(f"  {metric}: {value:.4f}")
        
        # Demonstrate comparison capabilities
        print("\n🔍 Demonstrating comparison capabilities...")
        
        # Create another branch for comparison
        comparison_branch = project.create_branch("random-forest-comparison")
        
        # Start another experiment with different parameters
        config_2 = config.copy()
        config_2['n_estimators'] = 200
        config_2['max_depth'] = 15
        
        mlflow_run_id_2 = mlflow_integration.start_run(comparison_branch, "random-forest-v2")
        mlflow_integration.log_parameters(config_2)
        
        wandb_run_id_2 = wandb_integration.start_run(
            comparison_branch,
            "random-forest-v2", 
            config=config_2,
            tags=['random-forest', 'classification', 'sklearn', 'comparison']
        )
        
        # Train second model
        model_2 = train_model(X_train, y_train, 
                            n_estimators=config_2['n_estimators'],
                            max_depth=config_2['max_depth'])
        metrics_2 = evaluate_model(model_2, X_test, y_test)
        
        # Log metrics for second model
        mlflow_integration.log_model_performance(metrics_2)
        wandb_integration.log_metrics(metrics_2)
        
        # Compare runs
        mlflow_comparison = mlflow_integration.compare_runs([mlflow_run_id, mlflow_run_id_2])
        wandb_comparison = wandb_integration.compare_runs([wandb_run_id, wandb_run_id_2])
        
        print("\n📊 MLflow Comparison:")
        for run in mlflow_comparison['runs']:
            print(f"  Run {run['run_id'][:8]}: {run['argon_branch']} - Accuracy: {run['metrics'].get('accuracy', 'N/A')}")
        
        print("\n📊 W&B Comparison:")
        for run in wandb_comparison['runs']:
            print(f"  Run {run['run_id'][:8]}: {run['argon_branch']} - Accuracy: {run['metrics'].get('accuracy', 'N/A')}")
        
        # End runs
        mlflow_integration.end_run("FINISHED")
        wandb_integration.finish_run(0)
        
        # Export experiment data
        print("\n💾 Exporting experiment data...")
        mlflow_integration.export_experiment_data("mlflow_export.json")
        wandb_integration.export_experiment_data(branch, "wandb_export.json")
        dvc_integration.export_data_version(branch, "dvc_export.json")
        
        print("✅ All integrations completed successfully!")
        print("\n🎯 Key Benefits Demonstrated:")
        print("  1. Unified experiment tracking across ML platforms")
        print("  2. Automatic branch-experiment association")
        print("  3. Reproducible data versioning with DVC")
        print("  4. Comprehensive metrics logging and comparison")
        print("  5. Easy export and backup of experiment data")
        
    except Exception as e:
        print(f"❌ Experiment failed: {e}")
        
        # End runs with failure status
        mlflow_integration.end_run("FAILED")
        wandb_integration.finish_run(1)
        
        raise

if __name__ == "__main__":
    main()