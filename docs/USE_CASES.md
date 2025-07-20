# Argon Use Cases

## Overview

This document showcases real-world use cases for Argon, demonstrating how teams can leverage Git-like version control for MongoDB in their ML/AI workflows.

## Table of Contents

1. [ML Model Training](#ml-model-training)
2. [A/B Testing](#ab-testing)
3. [Data Pipeline Development](#data-pipeline-development)
4. [Feature Engineering](#feature-engineering)
5. [Collaborative Data Science](#collaborative-data-science)
6. [Production Debugging](#production-debugging)

## ML Model Training

### Scenario: Experimenting with Different Training Datasets

A data science team needs to train models on different versions of their dataset without affecting production data.

```bash
# Create a branch for experiment
argonctl branch create model-v2-experiment --from main

# Switch to the experiment branch
argonctl branch switch model-v2-experiment
```

```python
# train_model.py
import pymongo
from sklearn.model_selection import train_test_split
from your_ml_framework import ModelTrainer

# Connect to the experiment branch
client = pymongo.MongoClient(os.environ['ARGON_MONGODB_URI'])
db = client.ml_database

# Modify training data
db.training_data.update_many(
    {"category": "outlier"},
    {"$set": {"weight": 0.1}}  # Reduce outlier influence
)

# Add augmented data
augmented_samples = generate_augmented_data()
db.training_data.insert_many(augmented_samples)

# Train model on modified dataset
X, y = load_data_from_mongodb(db.training_data)
X_train, X_test, y_train, y_test = train_test_split(X, y)

model = ModelTrainer()
model.fit(X_train, y_train)
metrics = model.evaluate(X_test, y_test)

# Save results
db.experiment_results.insert_one({
    "experiment_id": "model-v2",
    "branch": "model-v2-experiment",
    "metrics": metrics,
    "timestamp": datetime.now()
})
```

```bash
# Compare results across branches
argonctl branch compare model-v2-experiment --with main \
  --collection experiment_results

# If results are good, merge back
argonctl branch merge model-v2-experiment --into main
```

### Integration with MLflow

```python
# mlflow_integration.py
import mlflow
import argon

class ArgonMLflow:
    def __init__(self, branch_name):
        self.branch = argon.Branch(branch_name)
        self.setup_mlflow()
    
    def setup_mlflow(self):
        # Track which data branch was used
        mlflow.set_tag("argon.branch", self.branch.name)
        mlflow.set_tag("argon.parent", self.branch.parent)
        mlflow.set_tag("argon.created_at", self.branch.created_at)
    
    def log_data_version(self):
        # Log data statistics
        stats = self.branch.get_stats()
        mlflow.log_metrics({
            "data.total_samples": stats["documents"],
            "data.size_mb": stats["size_bytes"] / 1024 / 1024,
            "data.collections": len(stats["collections"])
        })

# Usage
with mlflow.start_run():
    argon_mlflow = ArgonMLflow("model-v2-experiment")
    argon_mlflow.log_data_version()
    
    # Train your model
    model = train_model()
    mlflow.sklearn.log_model(model, "model")
```

## A/B Testing

### Scenario: Testing New Recommendation Algorithm

An e-commerce platform wants to test a new recommendation algorithm without affecting the control group.

```bash
# Create branches for A/B test
argonctl branch create ab-test-control --from main
argonctl branch create ab-test-treatment --from main
```

```python
# ab_test_setup.py
import random
from datetime import datetime

# Assign users to groups
users = db.users.find({})
for user in users:
    group = "control" if random.random() < 0.5 else "treatment"
    user_id = user["_id"]
    
    if group == "control":
        # Control group - existing algorithm
        db_control = get_branch_connection("ab-test-control")
        db_control.users.update_one(
            {"_id": user_id},
            {"$set": {"ab_group": "control", "algorithm": "v1"}}
        )
    else:
        # Treatment group - new algorithm
        db_treatment = get_branch_connection("ab-test-treatment")
        db_treatment.users.update_one(
            {"_id": user_id},
            {"$set": {"ab_group": "treatment", "algorithm": "v2"}}
        )
```

```python
# recommendation_service.py
def get_recommendations(user_id):
    # Determine which branch to use
    user = db.users.find_one({"_id": user_id})
    branch = f"ab-test-{user['ab_group']}"
    
    # Connect to appropriate branch
    db_branch = get_branch_connection(branch)
    
    # Run recommendation algorithm
    if user["algorithm"] == "v1":
        recommendations = existing_algorithm(db_branch, user_id)
    else:
        recommendations = new_algorithm(db_branch, user_id)
    
    # Log interaction
    db_branch.interactions.insert_one({
        "user_id": user_id,
        "recommendations": recommendations,
        "timestamp": datetime.now(),
        "algorithm": user["algorithm"]
    })
    
    return recommendations
```

```python
# analyze_results.py
def analyze_ab_test():
    # Analyze both branches
    control_metrics = calculate_metrics("ab-test-control")
    treatment_metrics = calculate_metrics("ab-test-treatment")
    
    # Statistical significance test
    p_value = perform_t_test(control_metrics, treatment_metrics)
    
    if p_value < 0.05 and treatment_metrics["conversion"] > control_metrics["conversion"]:
        print("New algorithm is significantly better!")
        # Merge treatment branch
        argon.merge_branch("ab-test-treatment", "main")
    else:
        print("No significant improvement")
        # Discard experiment branches
        argon.delete_branch("ab-test-treatment")
        argon.delete_branch("ab-test-control")
```

## Data Pipeline Development

### Scenario: Developing ETL Pipeline Without Production Risk

A data engineering team needs to develop and test a new ETL pipeline.

```bash
# Create development branch
argonctl branch create etl-pipeline-dev --from main
```

```python
# etl_pipeline.py
import argon
from airflow import DAG
from airflow.operators.python_operator import PythonOperator

def extract_data(**context):
    # Connect to development branch
    db = argon.connect("etl-pipeline-dev")
    
    # Extract data from external sources
    raw_data = fetch_from_external_api()
    
    # Store in raw collection
    db.raw_data.insert_many(raw_data)
    return len(raw_data)

def transform_data(**context):
    db = argon.connect("etl-pipeline-dev")
    
    # Read raw data
    raw_docs = db.raw_data.find({"processed": {"$ne": True}})
    
    transformed = []
    for doc in raw_docs:
        # Apply transformations
        transformed_doc = {
            "user_id": doc["userId"],
            "event_type": standardize_event_type(doc["event"]),
            "timestamp": parse_timestamp(doc["time"]),
            "metadata": extract_metadata(doc)
        }
        transformed.append(transformed_doc)
        
        # Mark as processed
        db.raw_data.update_one(
            {"_id": doc["_id"]},
            {"$set": {"processed": True}}
        )
    
    # Store transformed data
    if transformed:
        db.transformed_data.insert_many(transformed)
    
    return len(transformed)

def validate_data(**context):
    db = argon.connect("etl-pipeline-dev")
    
    # Run validation checks
    validations = {
        "schema_check": validate_schema(db.transformed_data),
        "completeness": check_completeness(db.transformed_data),
        "consistency": check_consistency(db.transformed_data),
        "duplicates": check_duplicates(db.transformed_data)
    }
    
    # Store validation results
    db.validation_results.insert_one({
        "pipeline_run": context["run_id"],
        "timestamp": datetime.now(),
        "results": validations
    })
    
    # If all validations pass, mark as ready to merge
    if all(v["passed"] for v in validations.values()):
        argon.tag_branch("etl-pipeline-dev", "validated")
    
    return validations

# Airflow DAG
dag = DAG(
    'etl_pipeline_dev',
    default_args=default_args,
    schedule_interval='@daily'
)

extract_task = PythonOperator(
    task_id='extract_data',
    python_callable=extract_data,
    dag=dag
)

transform_task = PythonOperator(
    task_id='transform_data',
    python_callable=transform_data,
    dag=dag
)

validate_task = PythonOperator(
    task_id='validate_data',
    python_callable=validate_data,
    dag=dag
)

extract_task >> transform_task >> validate_task
```

```bash
# After validation passes, deploy to production
argonctl branch merge etl-pipeline-dev --into main \
  --strategy theirs  # Use all changes from dev branch
```

## Feature Engineering

### Scenario: Creating New Features for ML Models

A data scientist needs to experiment with feature engineering without affecting other team members.

```python
# feature_engineering.py
import pandas as pd
import numpy as np
from argon import Branch

class FeatureEngineer:
    def __init__(self, branch_name):
        self.branch = Branch.create(branch_name, from_branch="main")
        self.db = self.branch.connect()
    
    def create_user_features(self):
        """Create aggregated user features"""
        pipeline = [
            {
                "$group": {
                    "_id": "$user_id",
                    "total_purchases": {"$sum": 1},
                    "avg_purchase_value": {"$avg": "$amount"},
                    "days_since_first_purchase": {
                        "$first": "$created_at"
                    },
                    "favorite_category": {
                        "$first": "$category"
                    }
                }
            },
            {
                "$addFields": {
                    "customer_lifetime_value": {
                        "$multiply": ["$total_purchases", "$avg_purchase_value"]
                    },
                    "purchase_frequency": {
                        "$divide": ["$total_purchases", "$days_active"]
                    }
                }
            }
        ]
        
        # Execute aggregation and store results
        user_features = list(self.db.purchases.aggregate(pipeline))
        self.db.user_features.insert_many(user_features)
        
        return len(user_features)
    
    def create_time_features(self):
        """Extract time-based features"""
        purchases = self.db.purchases.find({})
        
        time_features = []
        for purchase in purchases:
            dt = purchase["created_at"]
            features = {
                "purchase_id": purchase["_id"],
                "hour_of_day": dt.hour,
                "day_of_week": dt.weekday(),
                "is_weekend": dt.weekday() >= 5,
                "month": dt.month,
                "quarter": (dt.month - 1) // 3 + 1,
                "is_holiday": is_holiday(dt),
                "days_to_payday": days_to_payday(dt)
            }
            time_features.append(features)
        
        self.db.time_features.insert_many(time_features)
        return len(time_features)
    
    def create_interaction_features(self):
        """Create features from user interactions"""
        # Get user browsing history
        browsing = pd.DataFrame(list(self.db.browsing_history.find()))
        purchases = pd.DataFrame(list(self.db.purchases.find()))
        
        # Calculate browse-to-purchase rate
        browse_purchase = browsing.merge(
            purchases, 
            on=["user_id", "product_id"],
            how="left"
        )
        
        interaction_features = browse_purchase.groupby("user_id").agg({
            "purchase_id": lambda x: x.notna().sum() / len(x),  # Conversion rate
            "time_on_page": "mean",
            "clicks": "sum"
        }).reset_index()
        
        # Store features
        self.db.interaction_features.insert_many(
            interaction_features.to_dict("records")
        )
        
        return len(interaction_features)
    
    def validate_features(self):
        """Validate created features"""
        # Check for nulls
        for collection in ["user_features", "time_features", "interaction_features"]:
            null_count = self.db[collection].count_documents({
                "$or": [
                    {"$expr": {"$eq": ["$field", None]}},
                    {"$expr": {"$eq": ["$field", np.nan]}}
                ]
            })
            if null_count > 0:
                print(f"Warning: {null_count} null values in {collection}")
        
        # Check distributions
        self.analyze_feature_distributions()
        
        return True

# Usage
engineer = FeatureEngineer("feature-engineering-v3")
engineer.create_user_features()
engineer.create_time_features()
engineer.create_interaction_features()

if engineer.validate_features():
    # Test model performance with new features
    model_metrics = test_model_with_features("feature-engineering-v3")
    
    if model_metrics["auc"] > baseline_metrics["auc"]:
        # Merge successful features
        argon.merge_branch("feature-engineering-v3", "main")
```

## Collaborative Data Science

### Scenario: Multiple Data Scientists Working on Same Dataset

A team of data scientists needs to work on different aspects of the same project simultaneously.

```python
# collaboration_workflow.py
import argon
from argon.collaboration import BranchManager

class DataScienceProject:
    def __init__(self, project_name):
        self.project = project_name
        self.branch_manager = BranchManager()
    
    def setup_team_branches(self, team_members):
        """Create personal branches for each team member"""
        branches = {}
        for member in team_members:
            branch_name = f"{self.project}/{member['name']}"
            branch = self.branch_manager.create_branch(
                name=branch_name,
                from_branch="main",
                description=f"Personal branch for {member['name']} - {member['task']}"
            )
            branches[member['name']] = branch
            
            # Set up permissions
            branch.grant_access(member['email'], 'write')
            
        return branches
    
    def create_integration_branch(self):
        """Create branch for integrating team work"""
        return self.branch_manager.create_branch(
            name=f"{self.project}/integration",
            from_branch="main",
            description="Integration branch for team collaboration"
        )

# Set up project
project = DataScienceProject("customer-churn-prediction")

team = [
    {"name": "alice", "email": "alice@company.com", "task": "feature engineering"},
    {"name": "bob", "email": "bob@company.com", "task": "model selection"},
    {"name": "carol", "email": "carol@company.com", "task": "data cleaning"}
]

branches = project.setup_team_branches(team)
integration = project.create_integration_branch()
```

```python
# alice_feature_engineering.py
# Alice works on feature engineering
db = argon.connect("customer-churn-prediction/alice")

# Create customer behavior features
behavior_features = create_behavioral_features(db.customers, db.transactions)
db.features.behavioral.insert_many(behavior_features)

# Create recency features
recency_features = create_recency_features(db.transactions)
db.features.recency.insert_many(recency_features)

# Signal completion
argon.tag_branch("customer-churn-prediction/alice", "features-ready")
```

```python
# bob_model_selection.py
# Bob works on model selection
db = argon.connect("customer-churn-prediction/bob")

# Test different models
models = {
    "random_forest": RandomForestClassifier(),
    "xgboost": XGBClassifier(),
    "neural_net": MLPClassifier()
}

results = []
for name, model in models.items():
    metrics = cross_validate_model(model, db.training_data)
    results.append({
        "model": name,
        "metrics": metrics,
        "timestamp": datetime.now()
    })

db.model_evaluation.insert_many(results)

# Signal completion
argon.tag_branch("customer-churn-prediction/bob", "models-evaluated")
```

```python
# integration.py
# Integrate team work
def integrate_team_work():
    integration_db = argon.connect("customer-churn-prediction/integration")
    
    # Check if all team members are ready
    ready_branches = []
    for member in ["alice", "bob", "carol"]:
        branch = f"customer-churn-prediction/{member}"
        tags = argon.get_branch_tags(branch)
        if any("ready" in tag for tag in tags):
            ready_branches.append(branch)
    
    if len(ready_branches) == 3:
        # Merge all branches into integration
        for branch in ready_branches:
            argon.merge_branch(branch, "customer-churn-prediction/integration")
        
        # Run integration tests
        run_integration_tests(integration_db)
        
        # If successful, merge to main
        argon.merge_branch("customer-churn-prediction/integration", "main")
```

## Production Debugging

### Scenario: Debugging Production Issues Without Downtime

A production issue needs investigation without affecting live services.

```python
# production_debug.py
import argon
from datetime import datetime, timedelta

class ProductionDebugger:
    def __init__(self, issue_id):
        self.issue_id = issue_id
        self.debug_branch = f"debug/{issue_id}"
        
        # Create snapshot of current production state
        self.snapshot = argon.create_snapshot(
            branch="main",
            name=f"pre-debug-{issue_id}"
        )
        
        # Create debug branch
        self.branch = argon.create_branch(
            name=self.debug_branch,
            from_snapshot=self.snapshot
        )
        
        self.db = argon.connect(self.debug_branch)
    
    def replay_issue_timeframe(self, start_time, end_time):
        """Replay events during issue timeframe"""
        # Get all events during the issue
        events = self.db.event_log.find({
            "timestamp": {
                "$gte": start_time,
                "$lte": end_time
            }
        }).sort("timestamp", 1)
        
        # Replay events to understand issue
        for event in events:
            self.process_event_debug(event)
    
    def process_event_debug(self, event):
        """Process event with additional debugging"""
        # Add debugging information
        debug_info = {
            "event_id": event["_id"],
            "debug_timestamp": datetime.now(),
            "memory_state": self.capture_memory_state(),
            "related_data": self.get_related_data(event)
        }
        
        # Store debug information
        self.db.debug_trace.insert_one(debug_info)
        
        # Process event normally to reproduce issue
        try:
            result = process_event(event)
            debug_info["result"] = result
            debug_info["success"] = True
        except Exception as e:
            debug_info["error"] = str(e)
            debug_info["stack_trace"] = traceback.format_exc()
            debug_info["success"] = False
            
            # Found the issue!
            self.analyze_failure(event, e)
    
    def analyze_failure(self, event, error):
        """Analyze why the failure occurred"""
        analysis = {
            "issue_id": self.issue_id,
            "failed_event": event,
            "error": str(error),
            "analysis": {}
        }
        
        # Check data consistency
        analysis["analysis"]["data_consistency"] = self.check_data_consistency(event)
        
        # Check for race conditions
        analysis["analysis"]["race_conditions"] = self.check_race_conditions(event)
        
        # Check for missing dependencies
        analysis["analysis"]["dependencies"] = self.check_dependencies(event)
        
        # Store analysis
        self.db.issue_analysis.insert_one(analysis)
        
        # Generate fix recommendations
        self.generate_fix_recommendations(analysis)
    
    def test_fix(self, fix_function):
        """Test a fix in the debug branch"""
        # Apply fix to debug branch
        fix_function(self.db)
        
        # Re-run problematic events
        problem_events = self.db.debug_trace.find({"success": False})
        
        fix_results = []
        for event in problem_events:
            original_event = self.db.event_log.find_one({"_id": event["event_id"]})
            
            try:
                result = process_event(original_event)
                fix_results.append({
                    "event_id": event["event_id"],
                    "fixed": True,
                    "result": result
                })
            except Exception as e:
                fix_results.append({
                    "event_id": event["event_id"],
                    "fixed": False,
                    "error": str(e)
                })
        
        # If fix works, prepare for production
        if all(r["fixed"] for r in fix_results):
            print(f"Fix successful! Ready to apply to production.")
            self.prepare_production_fix(fix_function)
        else:
            print(f"Fix failed for {sum(not r['fixed'] for r in fix_results)} events")
        
        return fix_results
    
    def cleanup(self):
        """Clean up debug branch after investigation"""
        # Save important findings
        findings = self.db.issue_analysis.find({})
        argon.export_collection(
            branch=self.debug_branch,
            collection="issue_analysis",
            output_file=f"debug_{self.issue_id}_findings.json"
        )
        
        # Delete debug branch
        argon.delete_branch(self.debug_branch)

# Usage during production incident
debugger = ProductionDebugger("INCIDENT-2024-001")

# Replay the timeframe when issue occurred
issue_start = datetime.now() - timedelta(hours=2)
issue_end = datetime.now() - timedelta(hours=1)
debugger.replay_issue_timeframe(issue_start, issue_end)

# Test potential fix
def apply_fix(db):
    # Fix: Add missing index
    db.orders.create_index([("user_id", 1), ("status", 1)])
    
    # Fix: Add data validation
    db.orders.update_many(
        {"status": {"$exists": False}},
        {"$set": {"status": "pending"}}
    )

debugger.test_fix(apply_fix)

# Clean up
debugger.cleanup()
```

## Best Practices

### 1. Branch Naming Conventions

```
feature/<feature-name>      # New features
experiment/<experiment-id>  # ML experiments  
debug/<issue-id>           # Debugging
user/<username>/<task>     # Personal branches
release/<version>          # Release preparation
```

### 2. Merge Strategies

```python
# For experiments - keep all changes from experiment
argon.merge_branch("experiment/new-model", "main", strategy="theirs")

# For bug fixes - carefully merge
argon.merge_branch("debug/issue-123", "main", strategy="manual")

# For features - use three-way merge
argon.merge_branch("feature/recommendations", "main", strategy="ours")
```

### 3. Cleanup Policy

```python
# Auto-cleanup old branches
def cleanup_old_branches():
    branches = argon.list_branches()
    for branch in branches:
        if branch.last_activity < datetime.now() - timedelta(days=30):
            if branch.name.startswith("experiment/"):
                argon.archive_branch(branch.name)
            elif branch.name.startswith("debug/"):
                argon.delete_branch(branch.name)
```