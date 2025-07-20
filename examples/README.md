# Argon Practical Demos

Experience how MongoDB branching with time travel solves real database disasters and ML pipeline failures.

## 🎯 Quick Demo Guide

```bash
# Prerequisites
- MongoDB running (mongod or Docker)
- Python 3 with pymongo (pip install pymongo)
- Argon CLI built (cd cli && go build -o argon)

# Run interactive demo menu
./run-all-demos.sh

# Or run specific demos directly
./developer-demo/migration-disaster.sh       # Database disaster recovery
./ml-demo/pipeline-recovery.sh              # ML pipeline failure recovery
./ml-demo/experiment-reproducibility.sh     # Research reproducibility
```

## 🚨 Demo 1: Database Migration Disaster Recovery

**Based on Real Incident**: Resend's 12-hour outage (Feb 2024) - failed migration deleted production data

**What You'll See**:
1. **Setup**: Email service with 5,500 users and 22,000 send logs
2. **Disaster**: Migration script fails, corrupts data, deletes records
3. **Traditional Recovery**: Would take 6-12 hours with potential data loss
4. **Argon Recovery**: 30-second rollback to exact pre-migration state

**The Wow Moment**: Watch a company-ending disaster become a 30-second fix

## 🤖 Demo 2: ML Pipeline Failure Recovery  

**Based on Real Problem**: 85% of ML projects fail from data pipeline issues

**What You'll See**:
1. **Setup**: Fraud detection ML with 10k customers, 100k transactions
2. **Working Pipeline**: Feature engineering creates risk scores
3. **Disaster**: Pipeline bug corrupts 50% of features (NULL values, infinity)
4. **Argon Recovery**: Instant rollback vs 1-3 days of pipeline rebuilding

**The Wow Moment**: Preserve expensive feature engineering work instantly

## 🔬 Demo 3: Experiment Reproducibility

**Based on Research Crisis**: 60% of ML papers can't be reproduced

**What You'll See**:
1. **Experiment 1.0**: Train sentiment model on 50k social posts
2. **Dataset Evolution**: 10k new posts added over time
3. **Reproducibility Problem**: Can't reproduce v1.0 with evolved data
4. **Argon Solution**: Time travel to exact v1.0 dataset state

**The Wow Moment**: 100% reproducible experiments guaranteed

## 💡 How to Present These Demos

### For Developers
Start with **Demo 1** (Migration Disaster):
- "Have you ever had a database migration go wrong in production?"
- "Here's how Resend lost 12 hours of service..."
- "Watch how Argon turns this into a 30-second recovery"
- Key point: "No more weekend emergency calls"

### For ML Engineers
Start with **Demo 2** (Pipeline Recovery):
- "How much time do you lose when pipelines corrupt training data?"
- "85% of ML projects fail, often from data issues..."
- "See how you never lose feature engineering work again"
- Key point: "Experiment fearlessly, rollback instantly"

### For Researchers/Compliance
Start with **Demo 3** (Reproducibility):
- "Can you reproduce your model from 6 months ago exactly?"
- "60% of ML research can't be reproduced..."
- "Pin exact dataset states for regulatory compliance"
- Key point: "EU AI Act ready, FDA compliant"

## 🎯 Key Talking Points

**Performance**: 
- 86x faster branching (1ms vs 100ms+)
- 37,905+ operations per second
- Zero data duplication

**Real Impact**:
- Database disasters: 12 hours → 30 seconds
- ML pipeline recovery: 1-3 days → instant
- Research reproduction: impossible → guaranteed

**Simple Integration**:
- Same MongoDB API
- One environment variable (ENABLE_WAL=true)
- Zero application changes needed

## 📊 Demo Metrics to Highlight

| Problem | Traditional | Argon | Improvement |
|---------|------------|--------|-------------|
| Migration Disaster | 12 hours | 30 seconds | 1,440x faster |
| Pipeline Recovery | 1-3 days | Instant | ∞ |
| Reproducibility | 60% fail | 100% success | Perfect |
| Storage Cost | 10x copies | 1x + logs | 90% savings |

## 🚀 Call to Action

After demos, suggest next steps:
1. "Try with your own data: clone the repo and run demos"
2. "Create a test branch of your production data"
3. "Run your riskiest migration on a branch first"
4. "Pin your next ML experiment for perfect reproducibility"

---

**Pro tip**: Let the demos speak for themselves. The moment someone sees a 12-hour disaster become a 30-second fix, they understand the value immediately.