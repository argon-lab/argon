# Argon Deployment Guide

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Deployment Options](#deployment-options)
3. [Local Development](#local-development)
4. [Docker Deployment](#docker-deployment)
5. [Kubernetes Deployment](#kubernetes-deployment)
6. [Cloud Deployments](#cloud-deployments)
7. [Configuration](#configuration)
8. [Monitoring](#monitoring)
9. [Troubleshooting](#troubleshooting)

## Prerequisites

### System Requirements

- **CPU**: 2+ cores (4+ recommended for production)
- **Memory**: 4GB minimum (8GB+ recommended)
- **Storage**: 20GB+ available space (depends on data size)
- **OS**: Linux, macOS, or Windows with WSL2

### Software Requirements

- MongoDB 4.4+ (with change streams support)
- Go 1.21+ (for building from source)
- Python 3.9+ (for API service)
- Docker 20.10+ (for containerized deployment)

### Network Requirements

- MongoDB connection (default port 27017)
- API service port (default 8080)
- Worker communication ports (default 9090-9099)

## Deployment Options

### Quick Start

```bash
# Using Docker Compose (recommended for development)
git clone https://github.com/your-org/argon.git
cd argon
docker-compose up -d

# Using pre-built binaries
wget https://github.com/your-org/argon/releases/latest/download/argon-linux-amd64.tar.gz
tar -xzf argon-linux-amd64.tar.gz
./argonctl init --mongodb-uri "mongodb://localhost:27017/mydb"
```

## Local Development

### 1. Clone and Build

```bash
# Clone repository
git clone https://github.com/your-org/argon.git
cd argon

# Build Go components
make build-go

# Install Python dependencies
cd services/api
pip install -r requirements.txt
cd ../..

# Run tests
make test
```

### 2. Start Services

```bash
# Start MongoDB (if not running)
docker run -d -p 27017:27017 --name mongodb mongo:6.0

# Start storage service
./bin/argon-storage --config config/storage.yaml

# Start worker pool
./bin/argon-worker --config config/worker.yaml

# Start API service
cd services/api
uvicorn main:app --reload --port 8080
```

### 3. Initialize Argon

```bash
./bin/argonctl init \
  --mongodb-uri "mongodb://localhost:27017/mydb" \
  --storage-type local \
  --storage-path ./data
```

## Docker Deployment

### Using Docker Compose

```yaml
# docker-compose.yml
version: '3.8'

services:
  mongodb:
    image: mongo:6.0
    ports:
      - "27017:27017"
    volumes:
      - mongodb_data:/data/db
    command: --replSet rs0

  argon-storage:
    build:
      context: .
      dockerfile: docker/Dockerfile.storage
    environment:
      - MONGODB_URI=mongodb://mongodb:27017/argon
      - STORAGE_TYPE=s3
      - AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}
      - AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}
      - S3_BUCKET=argon-storage
    depends_on:
      - mongodb

  argon-worker:
    build:
      context: .
      dockerfile: docker/Dockerfile.worker
    environment:
      - MONGODB_URI=mongodb://mongodb:27017/argon
      - WORKER_POOL_SIZE=4
    depends_on:
      - mongodb
      - argon-storage
    deploy:
      replicas: 2

  argon-api:
    build:
      context: .
      dockerfile: docker/Dockerfile.api
    ports:
      - "8080:8080"
    environment:
      - MONGODB_URI=mongodb://mongodb:27017/argon
      - API_KEY=${ARGON_API_KEY}
    depends_on:
      - mongodb
      - argon-storage
      - argon-worker

volumes:
  mongodb_data:
```

### Building Images

```bash
# Build all images
make docker-build

# Or build individually
docker build -f docker/Dockerfile.storage -t argon-storage:latest .
docker build -f docker/Dockerfile.worker -t argon-worker:latest .
docker build -f docker/Dockerfile.api -t argon-api:latest .
```

## Kubernetes Deployment

### 1. Create Namespace and Secrets

```yaml
# namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: argon

---
# secrets.yaml
apiVersion: v1
kind: Secret
metadata:
  name: argon-secrets
  namespace: argon
type: Opaque
data:
  mongodb-uri: bW9uZ29kYjovL21vbmdvZGI6MjcwMTcvYXJnb24= # base64 encoded
  api-key: eW91ci1hcGkta2V5 # base64 encoded
  aws-access-key-id: eW91ci1hd3Mta2V5 # base64 encoded
  aws-secret-access-key: eW91ci1hd3Mtc2VjcmV0 # base64 encoded
```

### 2. Deploy Storage Service

```yaml
# storage-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: argon-storage
  namespace: argon
spec:
  replicas: 2
  selector:
    matchLabels:
      app: argon-storage
  template:
    metadata:
      labels:
        app: argon-storage
    spec:
      containers:
      - name: storage
        image: argon-storage:latest
        env:
        - name: MONGODB_URI
          valueFrom:
            secretKeyRef:
              name: argon-secrets
              key: mongodb-uri
        - name: STORAGE_TYPE
          value: "s3"
        - name: S3_BUCKET
          value: "argon-storage"
        - name: AWS_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: argon-secrets
              key: aws-access-key-id
        - name: AWS_SECRET_ACCESS_KEY
          valueFrom:
            secretKeyRef:
              name: argon-secrets
              key: aws-secret-access-key
        resources:
          requests:
            memory: "512Mi"
            cpu: "500m"
          limits:
            memory: "1Gi"
            cpu: "1000m"
```

### 3. Deploy Worker Pool

```yaml
# worker-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: argon-worker
  namespace: argon
spec:
  replicas: 4
  selector:
    matchLabels:
      app: argon-worker
  template:
    metadata:
      labels:
        app: argon-worker
    spec:
      containers:
      - name: worker
        image: argon-worker:latest
        env:
        - name: MONGODB_URI
          valueFrom:
            secretKeyRef:
              name: argon-secrets
              key: mongodb-uri
        - name: WORKER_POOL_SIZE
          value: "4"
        resources:
          requests:
            memory: "1Gi"
            cpu: "1000m"
          limits:
            memory: "2Gi"
            cpu: "2000m"
```

### 4. Deploy API Service

```yaml
# api-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: argon-api
  namespace: argon
spec:
  replicas: 3
  selector:
    matchLabels:
      app: argon-api
  template:
    metadata:
      labels:
        app: argon-api
    spec:
      containers:
      - name: api
        image: argon-api:latest
        ports:
        - containerPort: 8080
        env:
        - name: MONGODB_URI
          valueFrom:
            secretKeyRef:
              name: argon-secrets
              key: mongodb-uri
        - name: API_KEY
          valueFrom:
            secretKeyRef:
              name: argon-secrets
              key: api-key
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"

---
# api-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: argon-api
  namespace: argon
spec:
  selector:
    app: argon-api
  ports:
  - port: 80
    targetPort: 8080
  type: LoadBalancer
```

### 5. Deploy with Helm (Alternative)

```bash
# Add Argon Helm repository
helm repo add argon https://charts.argon.io
helm repo update

# Install with custom values
helm install argon argon/argon \
  --namespace argon \
  --create-namespace \
  --set mongodb.uri="mongodb://mongodb:27017/argon" \
  --set storage.type="s3" \
  --set storage.s3.bucket="argon-storage" \
  --set api.replicas=3 \
  --set worker.replicas=4
```

## Cloud Deployments

### AWS Deployment

```bash
# Using AWS CDK
cd deploy/aws-cdk
npm install
cdk deploy ArgonStack \
  --parameters MongoDBUri="mongodb+srv://..." \
  --parameters S3BucketName="argon-storage"

# Using Terraform
cd deploy/terraform/aws
terraform init
terraform plan -var="mongodb_uri=mongodb+srv://..."
terraform apply
```

### GCP Deployment

```bash
# Using Google Cloud Run
gcloud run deploy argon-api \
  --image gcr.io/your-project/argon-api:latest \
  --platform managed \
  --region us-central1 \
  --set-env-vars MONGODB_URI="mongodb+srv://..."
```

### Azure Deployment

```bash
# Using Azure Container Instances
az container create \
  --resource-group argon-rg \
  --name argon-api \
  --image argon-api:latest \
  --dns-name-label argon-api \
  --ports 8080 \
  --environment-variables \
    MONGODB_URI="mongodb+srv://..." \
    STORAGE_TYPE="azure" \
    AZURE_STORAGE_ACCOUNT="argonstorage"
```

## Configuration

### Environment Variables

```bash
# Core Configuration
MONGODB_URI=mongodb://localhost:27017/argon
ARGON_CONFIG_PATH=/etc/argon/config.yaml
LOG_LEVEL=info

# Storage Configuration
STORAGE_TYPE=s3  # local, s3, gcs, azure
STORAGE_PATH=/var/lib/argon/data  # for local storage
S3_BUCKET=argon-storage
S3_REGION=us-east-1
AWS_ACCESS_KEY_ID=your-key
AWS_SECRET_ACCESS_KEY=your-secret

# Worker Configuration
WORKER_POOL_SIZE=4
WORKER_QUEUE_SIZE=1000
WORKER_BATCH_SIZE=100

# API Configuration
API_PORT=8080
API_KEY=your-api-key
RATE_LIMIT_PER_HOUR=1000
CORS_ALLOWED_ORIGINS=*

# Performance Tuning
COMPRESSION_ENABLED=true
COMPRESSION_LEVEL=3
CACHE_SIZE_MB=1024
MAX_CONNECTIONS=100
```

### Configuration File

```yaml
# config.yaml
mongodb:
  uri: "mongodb://localhost:27017/argon"
  options:
    maxPoolSize: 100
    minPoolSize: 10
    maxIdleTimeMS: 30000

storage:
  type: "s3"
  s3:
    bucket: "argon-storage"
    region: "us-east-1"
    prefix: "branches/"
  compression:
    enabled: true
    algorithm: "zstd"
    level: 3

worker:
  poolSize: 4
  queueSize: 1000
  batchSize: 100
  checkpointInterval: "30s"

api:
  port: 8080
  auth:
    enabled: true
    apiKeyHeader: "X-API-Key"
  rateLimit:
    enabled: true
    perHour: 1000
  cors:
    enabled: true
    allowedOrigins: ["*"]

monitoring:
  metrics:
    enabled: true
    endpoint: "/metrics"
  tracing:
    enabled: true
    jaegerEndpoint: "http://jaeger:14268/api/traces"
```

## Monitoring

### Prometheus Metrics

```yaml
# prometheus-config.yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'argon'
    static_configs:
      - targets: ['argon-api:8080', 'argon-worker:9090']
```

### Grafana Dashboard

Import the Argon dashboard:
```bash
curl -X POST http://grafana:3000/api/dashboards/import \
  -H "Content-Type: application/json" \
  -d @monitoring/grafana-dashboard.json
```

### Health Checks

```bash
# API health
curl http://argon-api:8080/health

# Worker health
curl http://argon-worker:9090/health

# Storage health
curl http://argon-storage:9091/health
```

## Troubleshooting

### Common Issues

#### 1. MongoDB Connection Failed

```bash
# Check MongoDB is accessible
mongosh "mongodb://localhost:27017" --eval "db.adminCommand('ping')"

# Check replica set status (required for change streams)
mongosh "mongodb://localhost:27017" --eval "rs.status()"

# Initialize replica set if needed
mongosh "mongodb://localhost:27017" --eval "rs.initiate()"
```

#### 2. Storage Access Issues

```bash
# Test S3 access
aws s3 ls s3://argon-storage/

# Check permissions
aws iam get-role-policy --role-name argon-storage-role --policy-name S3Access

# Test local storage
touch /var/lib/argon/data/test && rm /var/lib/argon/data/test
```

#### 3. High Memory Usage

```bash
# Check memory usage
docker stats argon-worker

# Adjust worker pool size
export WORKER_POOL_SIZE=2

# Enable memory profiling
export ARGON_PPROF_ENABLED=true
curl http://localhost:6060/debug/pprof/heap > heap.prof
go tool pprof heap.prof
```

#### 4. Slow Branch Creation

```bash
# Check MongoDB indexes
mongosh "mongodb://localhost:27017/argon" --eval "db.branches.getIndexes()"

# Create missing indexes
mongosh "mongodb://localhost:27017/argon" --eval "
  db.branches.createIndex({name: 1}, {unique: true});
  db.changes.createIndex({branch_id: 1, timestamp: -1});
"

# Check storage latency
time aws s3 cp test.file s3://argon-storage/test/
```

### Debug Mode

Enable debug logging:
```bash
export LOG_LEVEL=debug
export ARGON_DEBUG=true

# Or in config.yaml
logging:
  level: debug
  format: json
  output: stdout
```

### Performance Tuning

```yaml
# performance.yaml
mongodb:
  connection:
    maxPoolSize: 200
    minPoolSize: 50
    maxIdleTimeMS: 30000
    compressors: ["zstd", "snappy"]

storage:
  parallelism: 8
  bufferSize: 4096
  compression:
    level: 6  # Higher for better compression

worker:
  batchSize: 500
  prefetchSize: 1000
  goroutines: 16

cache:
  enabled: true
  size: "2GB"
  ttl: "1h"
```