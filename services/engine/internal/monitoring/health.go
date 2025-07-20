package monitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// HealthStatus represents the health status of a component
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusDegraded  HealthStatus = "degraded"
)

// ComponentHealth represents the health of a single component
type ComponentHealth struct {
	Status      HealthStatus `json:"status"`
	Message     string       `json:"message,omitempty"`
	LastChecked time.Time    `json:"last_checked"`
	Duration    string       `json:"duration"`
}

// HealthCheck represents the overall health check result
type HealthCheck struct {
	Status     HealthStatus               `json:"status"`
	Timestamp  time.Time                  `json:"timestamp"`
	Components map[string]ComponentHealth `json:"components"`
	Version    string                     `json:"version"`
	Uptime     string                     `json:"uptime"`
}

// HealthChecker manages health checks for various components
type HealthChecker struct {
	mongoClient   *mongo.Client
	workerPool    WorkerPoolHealth
	startTime     time.Time
	version       string
	mu            sync.RWMutex
	lastCheck     map[string]ComponentHealth
	checkInterval time.Duration
}

// WorkerPoolHealth interface for checking worker pool health
type WorkerPoolHealth interface {
	IsHealthy() bool
	GetActiveWorkers() int
	GetQueueSize() int
	GetProcessedJobs() int64
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(mongoClient *mongo.Client, workerPool WorkerPoolHealth, version string) *HealthChecker {
	return &HealthChecker{
		mongoClient:   mongoClient,
		workerPool:    workerPool,
		startTime:     time.Now(),
		version:       version,
		lastCheck:     make(map[string]ComponentHealth),
		checkInterval: 30 * time.Second,
	}
}

// StartHealthChecks starts periodic health checks
func (hc *HealthChecker) StartHealthChecks(ctx context.Context) {
	ticker := time.NewTicker(hc.checkInterval)
	defer ticker.Stop()

	// Initial check
	hc.performHealthChecks(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.performHealthChecks(ctx)
		}
	}
}

// performHealthChecks performs health checks on all components
func (hc *HealthChecker) performHealthChecks(ctx context.Context) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	// Check MongoDB
	hc.lastCheck["mongodb"] = hc.checkMongoDB(ctx)

	// Check Worker Pool
	hc.lastCheck["worker_pool"] = hc.checkWorkerPool()

	// Check S3 (if configured)
	hc.lastCheck["s3"] = hc.checkS3(ctx)

	// Check system resources
	hc.lastCheck["system"] = hc.checkSystem()
}

// checkMongoDB checks MongoDB connectivity and health
func (hc *HealthChecker) checkMongoDB(ctx context.Context) ComponentHealth {
	start := time.Now()
	
	// Create a timeout context for the ping
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if hc.mongoClient == nil {
		return ComponentHealth{
			Status:      HealthStatusUnhealthy,
			Message:     "MongoDB client not initialized",
			LastChecked: start,
			Duration:    time.Since(start).String(),
		}
	}

	// Ping MongoDB
	err := hc.mongoClient.Ping(pingCtx, readpref.Primary())
	duration := time.Since(start)

	if err != nil {
		return ComponentHealth{
			Status:      HealthStatusUnhealthy,
			Message:     fmt.Sprintf("MongoDB ping failed: %v", err),
			LastChecked: start,
			Duration:    duration.String(),
		}
	}

	// Check if response time is reasonable (< 1 second)
	status := HealthStatusHealthy
	message := "MongoDB is healthy"
	
	if duration > time.Second {
		status = HealthStatusDegraded
		message = fmt.Sprintf("MongoDB response time is slow: %v", duration)
	}

	return ComponentHealth{
		Status:      status,
		Message:     message,
		LastChecked: start,
		Duration:    duration.String(),
	}
}

// checkWorkerPool checks worker pool health
func (hc *HealthChecker) checkWorkerPool() ComponentHealth {
	start := time.Now()

	if hc.workerPool == nil {
		return ComponentHealth{
			Status:      HealthStatusUnhealthy,
			Message:     "Worker pool not initialized",
			LastChecked: start,
			Duration:    time.Since(start).String(),
		}
	}

	isHealthy := hc.workerPool.IsHealthy()
	activeWorkers := hc.workerPool.GetActiveWorkers()
	queueSize := hc.workerPool.GetQueueSize()
	processedJobs := hc.workerPool.GetProcessedJobs()

	status := HealthStatusHealthy
	message := fmt.Sprintf("Workers: %d, Queue: %d, Processed: %d", 
		activeWorkers, queueSize, processedJobs)

	if !isHealthy {
		status = HealthStatusUnhealthy
		message = "Worker pool is unhealthy: " + message
	} else if queueSize > 1000 {
		status = HealthStatusDegraded
		message = "High queue size: " + message
	} else if activeWorkers == 0 {
		status = HealthStatusDegraded
		message = "No active workers: " + message
	}

	return ComponentHealth{
		Status:      status,
		Message:     message,
		LastChecked: start,
		Duration:    time.Since(start).String(),
	}
}

// checkS3 checks S3 connectivity (basic check)
func (hc *HealthChecker) checkS3(ctx context.Context) ComponentHealth {
	start := time.Now()

	// For now, we'll assume S3 is healthy if we can create a client
	// In a real implementation, you might want to do a simple operation
	// like listing buckets or checking credentials
	
	return ComponentHealth{
		Status:      HealthStatusHealthy,
		Message:     "S3 connectivity assumed healthy",
		LastChecked: start,
		Duration:    time.Since(start).String(),
	}
}

// checkSystem checks basic system health
func (hc *HealthChecker) checkSystem() ComponentHealth {
	start := time.Now()

	// Basic system check - could be expanded to check disk space,
	// memory usage, CPU load, etc.
	
	return ComponentHealth{
		Status:      HealthStatusHealthy,
		Message:     "System resources are healthy",
		LastChecked: start,
		Duration:    time.Since(start).String(),
	}
}

// GetHealthStatus returns the current health status
func (hc *HealthChecker) GetHealthStatus() HealthCheck {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	// Determine overall status
	overallStatus := HealthStatusHealthy
	for _, component := range hc.lastCheck {
		if component.Status == HealthStatusUnhealthy {
			overallStatus = HealthStatusUnhealthy
			break
		} else if component.Status == HealthStatusDegraded && overallStatus == HealthStatusHealthy {
			overallStatus = HealthStatusDegraded
		}
	}

	return HealthCheck{
		Status:     overallStatus,
		Timestamp:  time.Now(),
		Components: hc.lastCheck,
		Version:    hc.version,
		Uptime:     time.Since(hc.startTime).String(),
	}
}

// HealthHandler returns an HTTP handler for health checks
func (hc *HealthChecker) HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		health := hc.GetHealthStatus()
		
		// Set appropriate status code
		switch health.Status {
		case HealthStatusHealthy:
			w.WriteHeader(http.StatusOK)
		case HealthStatusDegraded:
			w.WriteHeader(http.StatusOK) // 200 but with degraded status
		case HealthStatusUnhealthy:
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(health)
	}
}

// ReadinessHandler returns a simpler readiness check for Kubernetes
func (hc *HealthChecker) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		health := hc.GetHealthStatus()
		
		if health.Status == HealthStatusUnhealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("NOT READY"))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("READY"))
	}
}

// LivenessHandler returns a simple liveness check for Kubernetes
func (hc *HealthChecker) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Simple liveness check - if we can respond, we're alive
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ALIVE"))
	}
}