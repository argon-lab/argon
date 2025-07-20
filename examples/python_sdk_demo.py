#!/usr/bin/env python3
"""
Argon Python SDK Demo

Demonstrates how to use Argon's Python SDK for MongoDB branching
and ML experiment tracking.
"""

import sys
import os

# Add parent directory to path for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..'))

def basic_usage_demo():
    """Demonstrate basic Argon usage"""
    print("🚀 Argon Python SDK Demo\n")
    
    # Import Argon components
    from core.project import Project
    from integrations.jupyter import init_argon_notebook
    
    print("1. Creating an Argon project...")
    project = Project("ml-experiment-demo")
    print(f"   ✅ Project: {project.name} (ID: {project.id})")
    
    print("\n2. Creating branches for experiments...")
    main_branch = project.get_branch("main")
    exp_branch = project.create_branch("experiment-v1")
    print(f"   ✅ Main branch: {main_branch.name}")
    print(f"   ✅ Experiment branch: {exp_branch.name}")
    
    print("\n3. Setting up Jupyter integration...")
    jupyter = init_argon_notebook("ml-experiment-demo")
    jupyter.set_branch("experiment-v1")
    print("   ✅ Jupyter integration configured")
    
    print("\n4. Logging experiment data...")
    jupyter.log_experiment_params({
        "model": "RandomForest",
        "n_estimators": 100,
        "max_depth": 10,
        "learning_rate": 0.01
    })
    
    jupyter.log_experiment_metrics({
        "accuracy": 0.94,
        "precision": 0.92,
        "recall": 0.91,
        "f1_score": 0.915
    })
    print("   ✅ Experiment parameters and metrics logged")
    
    print("\n5. Creating experiment checkpoint...")
    jupyter.create_checkpoint("model_v1", "First working model with 94% accuracy")
    print("   ✅ Checkpoint created")
    
    print("\n6. Viewing project status...")
    status = project.get_status()
    print(f"   📊 Project: {status['name']}")
    print(f"   📊 Branches: {status['branches']}")
    print(f"   📊 Created: {status['created_at']}")
    
    print("\n🎉 Demo completed successfully!")
    print("\nWhat you just did:")
    print("- Created a MongoDB project with branching enabled")
    print("- Set up experiment branches for ML workflows")  
    print("- Logged experiment parameters and metrics")
    print("- Created checkpoints for reproducible research")
    print("\nNext steps:")
    print("- Use argon CLI to view time travel history")
    print("- Integrate with MLflow, DVC, or Weights & Biases")
    print("- Scale to production ML pipelines")

def advanced_ml_demo():
    """Demonstrate advanced ML workflow features"""
    print("\n🧠 Advanced ML Workflow Demo\n")
    
    from integrations.jupyter import init_argon_notebook
    
    # Initialize for a more complex project
    jupyter = init_argon_notebook("advanced-ml-pipeline")
    
    print("1. Setting up multiple experiment branches...")
    branches = ["baseline", "feature-engineering", "hyperparameter-tuning", "ensemble"]
    
    for branch_name in branches:
        jupyter.set_branch(branch_name)
        print(f"   ✅ Branch: {branch_name}")
        
        # Log different experimental setups
        if branch_name == "baseline":
            jupyter.log_experiment_params({"model": "LogisticRegression", "simple": True})
            jupyter.log_experiment_metrics({"accuracy": 0.78})
        elif branch_name == "feature-engineering":
            jupyter.log_experiment_params({"features": "engineered", "scaling": "standard"})
            jupyter.log_experiment_metrics({"accuracy": 0.85})
        elif branch_name == "hyperparameter-tuning":
            jupyter.log_experiment_params({"model": "XGBoost", "tuned": True})
            jupyter.log_experiment_metrics({"accuracy": 0.92})
        elif branch_name == "ensemble":
            jupyter.log_experiment_params({"model": "VotingClassifier", "n_models": 3})
            jupyter.log_experiment_metrics({"accuracy": 0.95})
        
        jupyter.create_checkpoint(f"{branch_name}_final", f"Final model for {branch_name}")
    
    print("\n2. Comparing experiments across branches...")
    jupyter.set_branch("ensemble")  # Set to final branch
    
    comparison = jupyter.compare_experiments(branches)
    print("   📈 Experiment Comparison:")
    for branch_data in comparison["branches"]:
        metrics = branch_data.get("metrics", {})
        accuracy = metrics.get("accuracy", "N/A")
        print(f"     {branch_data['name']}: {accuracy}")
    
    print("\n🎯 Advanced demo completed!")
    print("This workflow demonstrates:")
    print("- Multi-branch experiment organization")
    print("- Systematic A/B testing of ML approaches")
    print("- Automated experiment comparison")
    print("- Reproducible research with Git-like branching")

if __name__ == "__main__":
    try:
        basic_usage_demo()
        advanced_ml_demo()
    except Exception as e:
        print(f"❌ Demo failed: {e}")
        sys.exit(1)