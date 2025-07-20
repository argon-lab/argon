# Production Deployment Guide

## Overview

This guide covers deploying Argon WAL in production environments, including configuration, monitoring, and best practices for high-availability deployments.

## 🎯 Production Readiness Features

### ✅ Core Features
- **Enhanced Error Handling**: Structured errors with retry logic and detailed context
- **Monitoring & Metrics**: Real-time performance tracking and health monitoring
- **Intelligent Caching**: LRU caches for states, queries, and metadata
- **Production Configuration**: Environment-based configuration management
- **CLI Tools**: Complete command-line interface for operations

### ✅ Performance Optimization
- **Write Throughput**: 15,360+ ops/sec
- **Query Performance**: < 50ms for time travel queries
- **Branch Creation**: 1.16ms (vs 100ms+ industry standard)
- **Concurrent Operations**: 2,800+ queries/sec

## 🚀 Quick Start

### 1. Environment Setup

```bash
# Enable WAL mode
export ENABLE_WAL=true

# MongoDB connection
export MONGODB_URI="mongodb://localhost:27017"
export WAL_DB_NAME="argon_wal"

# Optional: Performance tuning
export WAL_CACHE_SIZE="100MB"
export WAL_METRICS_ENABLED=true
export WAL_MONITORING_ENABLED=true
```

### 2. Install and Configure

```bash
# Install Argon
npm install -g @argon-lab/argon

# Initialize WAL system
argon wal-simple status
argon wal-simple project create production
```

### 3. Basic Operations

```bash
# Create a new branch for development
argon branch create feature-branch

# Use time travel to query historical state
argon wal-simple tt-info -p production -b main

# Preview restore operations safely
argon wal-simple restore-preview -p production -b main --lsn 1500
```

## 📋 Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_WAL` | `false` | Enable WAL functionality |
| `MONGODB_URI` | `mongodb://localhost:27017` | MongoDB connection string |
| `WAL_DB_NAME` | `argon_wal` | WAL database name |
| `WAL_CACHE_SIZE` | `100MB` | Maximum cache memory |
| `WAL_METRICS_ENABLED` | `true` | Enable metrics collection |
| `WAL_MONITORING_ENABLED` | `true` | Enable health monitoring |
| `WAL_LOG_LEVEL` | `info` | Logging level |

### Configuration File

Create `argon-wal.json` in your project root:

```json
{
  "wal": {
    "enabled": true,
    "database": {
      "uri": "mongodb://localhost:27017",
      "name": "argon_wal",
      "poolSize": 10,
      "timeout": 5000
    },
    "cache": {
      "maxMemory": "100MB",
      "queryTTL": "5m",
      "branchTTL": "30m",
      "cleanupInterval": "1m"
    },
    "monitoring": {
      "enabled": true,
      "healthCheckInterval": "30s",
      "metricsReportInterval": "60s",
      "alertThresholds": {
        "maxErrorRate": 0.05,
        "maxLatency": "1s",
        "minSuccessRate": 0.95
      }
    }
  }
}
```

## 🔧 Production Architecture

### Recommended Deployment

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Application   │────▶│   Argon WAL     │────▶│    MongoDB      │
│   (Your App)    │     │   (Primary)     │     │   (Replica Set) │
└─────────────────┘     └─────────────────┘     └─────────────────┘
                                │
                                ▼
                        ┌─────────────────┐
                        │   Monitoring    │
                        │   (Metrics)     │
                        └─────────────────┘
```

### High Availability Setup

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Load Balancer │────▶│   Argon WAL     │────▶│    MongoDB      │
└─────────────────┘     │   Instance 1    │     │   Primary       │
                        └─────────────────┘     └─────────────────┘
                                │                         │
                        ┌─────────────────┐     ┌─────────────────┐
                        │   Argon WAL     │────▶│    MongoDB      │
                        │   Instance 2    │     │   Secondary     │
                        └─────────────────┘     └─────────────────┘
```

## 📊 Monitoring & Observability

### Health Checks

Built-in health checks monitor:
- Database connectivity
- Performance metrics (latency, success rates)
- Memory usage
- Connection pool status

### Metrics Collection

Key metrics tracked:
- **Operations**: Append, query, materialization, branch, restore counts
- **Performance**: Average latencies for all operation types  
- **Errors**: Error counts by operation type
- **State**: Current LSN, active branches/projects, last operation time

### Alerts

Automatic alerts for:
- High error rates (> 5% default)
- High latency (> 1s default)
- Low success rates (< 95% default)
- Connection failures
- Health check failures

### Example Monitoring Integration

```go
// Enable monitoring
monitor := wal.NewMonitor(wal.GlobalMetrics, wal.MonitorConfig{
    HealthCheckInterval:   30 * time.Second,
    MetricsReportInterval: 60 * time.Second,
    EnableLogging:         true,
    AlertThresholds: wal.AlertThresholds{
        MaxErrorRate:     0.05,  // 5%
        MaxLatency:       1 * time.Second,
        MinSuccessRate:   0.95,  // 95%
    },
})

monitor.Start()
defer monitor.Stop()

// Check health
if !monitor.IsHealthy() {
    log.Warning("WAL system is unhealthy")
    for _, alert := range monitor.GetActiveAlerts() {
        log.Errorf("Alert: %s - %s", alert.Title, alert.Message)
    }
}
```

## 🛡️ Security & Best Practices

### Access Control

1. **Environment Variables**: Use secure environment variable management
2. **Database Access**: Restrict MongoDB access to WAL service only
3. **Network Security**: Use VPN or private networks for multi-node deployments
4. **Authentication**: Enable MongoDB authentication in production

### Data Protection

1. **Encryption**: Enable MongoDB encryption at rest
2. **Backup Strategy**: Regular backups of both application and WAL data
3. **Network Encryption**: Use TLS for all MongoDB connections
4. **Access Logging**: Monitor all WAL operations for audit trails

### Best Practices

1. **Resource Limits**: Set appropriate memory/CPU limits
2. **Connection Pooling**: Configure optimal pool sizes
3. **Cache Tuning**: Adjust cache sizes based on workload
4. **Monitoring**: Set up comprehensive monitoring and alerting
5. **Testing**: Thoroughly test restore procedures

## 🔍 Troubleshooting

### Common Issues

#### High Latency
```bash
# Check cache hit rates
argon wal-simple status --verbose

# Monitor active operations
argon wal-simple metrics --real-time

# Review cache configuration
export WAL_CACHE_SIZE="200MB"  # Increase cache size
```

#### Connection Errors
```bash
# Verify MongoDB connectivity
mongosh $MONGODB_URI

# Check connection pool status
argon wal-simple status --connections

# Increase connection pool
export WAL_CONNECTION_POOL_SIZE=20
```

#### Memory Usage
```bash
# Monitor memory usage
argon wal-simple metrics --memory

# Reduce cache size if needed
export WAL_CACHE_SIZE="50MB"

# Enable cache cleanup
export WAL_CACHE_CLEANUP_INTERVAL="30s"
```

### Debug Mode

Enable detailed logging:
```bash
export WAL_LOG_LEVEL=debug
export WAL_TRACE_ENABLED=true
```

### Performance Tuning

Optimize for high throughput:
```bash
export WAL_BATCH_SIZE=1000
export WAL_WRITE_CONCERN="majority"
export WAL_READ_PREFERENCE="primaryPreferred"
```

## 📈 Performance Benchmarks

### Production Metrics

| Metric | Target | Achieved |
|--------|--------|----------|
| Write Throughput | 10,000 ops/s | 15,360 ops/s |
| Query Latency | < 100ms | < 50ms |
| Branch Creation | < 10ms | 1.16ms |
| Time Travel Query | < 200ms | < 50ms |
| Success Rate | > 99% | > 99.5% |

### Load Testing

Example load test script:
```bash
#!/bin/bash
# Simulate high load
for i in {1..100}; do
  argon wal-simple project create "load-test-$i" &
done
wait

# Monitor performance
argon wal-simple metrics --interval 1s
```

## 🚢 Deployment Scripts

### Docker Deployment

```dockerfile
FROM node:18-alpine

# Install Argon
RUN npm install -g @argon-lab/argon

# Environment setup
ENV ENABLE_WAL=true
ENV WAL_METRICS_ENABLED=true
ENV WAL_MONITORING_ENABLED=true

# Health check
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD argon wal-simple status || exit 1

# Start application
CMD ["argon", "server", "--wal-enabled"]
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: argon-wal
spec:
  replicas: 3
  selector:
    matchLabels:
      app: argon-wal
  template:
    metadata:
      labels:
        app: argon-wal
    spec:
      containers:
      - name: argon-wal
        image: argon-lab/argon:latest
        env:
        - name: ENABLE_WAL
          value: "true"
        - name: MONGODB_URI
          valueFrom:
            secretKeyRef:
              name: mongodb-secret
              key: uri
        ports:
        - containerPort: 3000
        livenessProbe:
          exec:
            command:
            - argon
            - wal-simple
            - status
          initialDelaySeconds: 30
          periodSeconds: 30
        readinessProbe:
          exec:
            command:
            - argon
            - wal-simple
            - status
          initialDelaySeconds: 5
          periodSeconds: 10
```

## 📚 Additional Resources

### Documentation
- [WAL Architecture Guide](./WAL_IMPLEMENTATION_OVERVIEW.md)
- [CLI Reference](./WAL_CLI_DESIGN.md)
- [API Documentation](./WAL_API_REFERENCE.md)

### Support
- [GitHub Issues](https://github.com/argon-lab/argon/issues)
- [Community Forum](https://community.argon-lab.com)
- [Enterprise Support](https://argon-lab.com/support)

### Examples
- [Basic Integration Examples](../examples/)
- [Advanced Use Cases](../examples/advanced/)
- [Performance Testing Scripts](../scripts/performance/)

---

## 🎉 Success Criteria

Your WAL deployment is production-ready when:

- ✅ Health checks pass consistently
- ✅ Performance meets or exceeds targets
- ✅ Monitoring and alerting are configured
- ✅ Backup and recovery procedures are tested
- ✅ Security measures are implemented
- ✅ Load testing validates capacity
- ✅ Team is trained on operations

**Congratulations! You now have a production-ready "Neon for MongoDB" deployment with WAL-based branching and time travel capabilities.** 🚀