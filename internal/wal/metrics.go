package wal

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics holds WAL operation metrics
type Metrics struct {
	// Operation counters
	AppendOps     int64 `json:"append_ops"`
	QueryOps      int64 `json:"query_ops"`
	MaterialOps   int64 `json:"material_ops"`
	BranchOps     int64 `json:"branch_ops"`
	RestoreOps    int64 `json:"restore_ops"`
	
	// Error counters
	AppendErrors  int64 `json:"append_errors"`
	QueryErrors   int64 `json:"query_errors"`
	MaterialErrors int64 `json:"material_errors"`
	ConnectionErrors int64 `json:"connection_errors"`
	
	// Performance metrics
	AvgAppendLatency   time.Duration `json:"avg_append_latency"`
	AvgQueryLatency    time.Duration `json:"avg_query_latency"`
	AvgMaterialLatency time.Duration `json:"avg_material_latency"`
	
	// Current state
	CurrentLSN       int64     `json:"current_lsn"`
	ActiveBranches   int       `json:"active_branches"`
	ActiveProjects   int       `json:"active_projects"`
	LastOperationTime time.Time `json:"last_operation_time"`
	
	// Internal tracking
	latencyTracker *LatencyTracker
	mu             sync.RWMutex
}

// LatencyTracker tracks operation latencies with moving averages
type LatencyTracker struct {
	appendSamples   []time.Duration
	querySamples    []time.Duration
	materialSamples []time.Duration
	maxSamples      int
	mu              sync.RWMutex
}

// NewMetrics creates a new metrics collector
func NewMetrics() *Metrics {
	return &Metrics{
		latencyTracker: &LatencyTracker{
			appendSamples:   make([]time.Duration, 0, 100),
			querySamples:    make([]time.Duration, 0, 100),
			materialSamples: make([]time.Duration, 0, 100),
			maxSamples:      100,
		},
	}
}

// RecordAppend records a WAL append operation
func (m *Metrics) RecordAppend(latency time.Duration, success bool) {
	atomic.AddInt64(&m.AppendOps, 1)
	if !success {
		atomic.AddInt64(&m.AppendErrors, 1)
	}
	
	m.latencyTracker.recordAppend(latency)
	m.updateAverageLatencies()
	m.updateLastOperationTime()
}

// RecordQuery records a query operation
func (m *Metrics) RecordQuery(latency time.Duration, success bool) {
	atomic.AddInt64(&m.QueryOps, 1)
	if !success {
		atomic.AddInt64(&m.QueryErrors, 1)
	}
	
	m.latencyTracker.recordQuery(latency)
	m.updateAverageLatencies()
	m.updateLastOperationTime()
}

// RecordMaterialization records a materialization operation
func (m *Metrics) RecordMaterialization(latency time.Duration, success bool) {
	atomic.AddInt64(&m.MaterialOps, 1)
	if !success {
		atomic.AddInt64(&m.MaterialErrors, 1)
	}
	
	m.latencyTracker.recordMaterial(latency)
	m.updateAverageLatencies()
	m.updateLastOperationTime()
}

// RecordBranchOp records a branch operation
func (m *Metrics) RecordBranchOp() {
	atomic.AddInt64(&m.BranchOps, 1)
	m.updateLastOperationTime()
}

// RecordRestoreOp records a restore operation
func (m *Metrics) RecordRestoreOp() {
	atomic.AddInt64(&m.RestoreOps, 1)
	m.updateLastOperationTime()
}

// RecordConnectionError records a connection error
func (m *Metrics) RecordConnectionError() {
	atomic.AddInt64(&m.ConnectionErrors, 1)
}

// UpdateCurrentLSN updates the current LSN
func (m *Metrics) UpdateCurrentLSN(lsn int64) {
	atomic.StoreInt64(&m.CurrentLSN, lsn)
}

// UpdateActiveBranches updates the active branch count
func (m *Metrics) UpdateActiveBranches(count int) {
	m.mu.Lock()
	m.ActiveBranches = count
	m.mu.Unlock()
}

// UpdateActiveProjects updates the active project count
func (m *Metrics) UpdateActiveProjects(count int) {
	m.mu.Lock()
	m.ActiveProjects = count
	m.mu.Unlock()
}

// MetricsSnapshot represents a read-only snapshot of metrics without mutexes
type MetricsSnapshot struct {
	// Operation counters
	AppendOps   int64 `json:"append_ops"`
	QueryOps    int64 `json:"query_ops"`
	MaterialOps int64 `json:"material_ops"`
	BranchOps   int64 `json:"branch_ops"`
	RestoreOps  int64 `json:"restore_ops"`
	
	// Error counters
	AppendErrors     int64 `json:"append_errors"`
	QueryErrors      int64 `json:"query_errors"`
	MaterialErrors   int64 `json:"material_errors"`
	ConnectionErrors int64 `json:"connection_errors"`
	
	// Performance metrics
	AvgAppendLatency   time.Duration `json:"avg_append_latency"`
	AvgQueryLatency    time.Duration `json:"avg_query_latency"`
	AvgMaterialLatency time.Duration `json:"avg_material_latency"`
	
	// Current state
	CurrentLSN        int64     `json:"current_lsn"`
	ActiveBranches    int       `json:"active_branches"`
	ActiveProjects    int       `json:"active_projects"`
	LastOperationTime time.Time `json:"last_operation_time"`
}

// GetSnapshot returns a read-only snapshot of current metrics
func (m *Metrics) GetSnapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	snapshot := MetricsSnapshot{
		AppendOps:          atomic.LoadInt64(&m.AppendOps),
		QueryOps:           atomic.LoadInt64(&m.QueryOps),
		MaterialOps:        atomic.LoadInt64(&m.MaterialOps),
		BranchOps:          atomic.LoadInt64(&m.BranchOps),
		RestoreOps:         atomic.LoadInt64(&m.RestoreOps),
		AppendErrors:       atomic.LoadInt64(&m.AppendErrors),
		QueryErrors:        atomic.LoadInt64(&m.QueryErrors),
		MaterialErrors:     atomic.LoadInt64(&m.MaterialErrors),
		ConnectionErrors:   atomic.LoadInt64(&m.ConnectionErrors),
		AvgAppendLatency:   m.AvgAppendLatency,
		AvgQueryLatency:    m.AvgQueryLatency,
		AvgMaterialLatency: m.AvgMaterialLatency,
		CurrentLSN:         atomic.LoadInt64(&m.CurrentLSN),
		ActiveBranches:     m.ActiveBranches,
		ActiveProjects:     m.ActiveProjects,
		LastOperationTime:  m.LastOperationTime,
	}
	
	return snapshot
}

// GetSuccessRate returns the success rate for each operation type
func (m *Metrics) GetSuccessRate() map[string]float64 {
	appendOps := atomic.LoadInt64(&m.AppendOps)
	queryOps := atomic.LoadInt64(&m.QueryOps)
	materialOps := atomic.LoadInt64(&m.MaterialOps)
	
	appendErrors := atomic.LoadInt64(&m.AppendErrors)
	queryErrors := atomic.LoadInt64(&m.QueryErrors)
	materialErrors := atomic.LoadInt64(&m.MaterialErrors)
	
	rates := make(map[string]float64)
	
	if appendOps > 0 {
		rates["append"] = float64(appendOps-appendErrors) / float64(appendOps)
	}
	if queryOps > 0 {
		rates["query"] = float64(queryOps-queryErrors) / float64(queryOps)
	}
	if materialOps > 0 {
		rates["materialization"] = float64(materialOps-materialErrors) / float64(materialOps)
	}
	
	return rates
}

// Reset resets all metrics (useful for testing)
func (m *Metrics) Reset() {
	atomic.StoreInt64(&m.AppendOps, 0)
	atomic.StoreInt64(&m.QueryOps, 0)
	atomic.StoreInt64(&m.MaterialOps, 0)
	atomic.StoreInt64(&m.BranchOps, 0)
	atomic.StoreInt64(&m.RestoreOps, 0)
	atomic.StoreInt64(&m.AppendErrors, 0)
	atomic.StoreInt64(&m.QueryErrors, 0)
	atomic.StoreInt64(&m.MaterialErrors, 0)
	atomic.StoreInt64(&m.ConnectionErrors, 0)
	atomic.StoreInt64(&m.CurrentLSN, 0)
	
	m.mu.Lock()
	m.ActiveBranches = 0
	m.ActiveProjects = 0
	m.LastOperationTime = time.Time{}
	m.AvgAppendLatency = 0
	m.AvgQueryLatency = 0
	m.AvgMaterialLatency = 0
	m.latencyTracker.reset()
	m.mu.Unlock()
}

// LatencyTracker methods
func (lt *LatencyTracker) recordAppend(latency time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	
	lt.appendSamples = append(lt.appendSamples, latency)
	if len(lt.appendSamples) > lt.maxSamples {
		lt.appendSamples = lt.appendSamples[1:]
	}
}

func (lt *LatencyTracker) recordQuery(latency time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	
	lt.querySamples = append(lt.querySamples, latency)
	if len(lt.querySamples) > lt.maxSamples {
		lt.querySamples = lt.querySamples[1:]
	}
}

func (lt *LatencyTracker) recordMaterial(latency time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	
	lt.materialSamples = append(lt.materialSamples, latency)
	if len(lt.materialSamples) > lt.maxSamples {
		lt.materialSamples = lt.materialSamples[1:]
	}
}

func (lt *LatencyTracker) getAverages() (time.Duration, time.Duration, time.Duration) {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	
	appendAvg := lt.calculateAverage(lt.appendSamples)
	queryAvg := lt.calculateAverage(lt.querySamples)
	materialAvg := lt.calculateAverage(lt.materialSamples)
	
	return appendAvg, queryAvg, materialAvg
}

func (lt *LatencyTracker) calculateAverage(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	
	total := time.Duration(0)
	for _, sample := range samples {
		total += sample
	}
	
	return total / time.Duration(len(samples))
}

func (lt *LatencyTracker) reset() {
	lt.appendSamples = lt.appendSamples[:0]
	lt.querySamples = lt.querySamples[:0]
	lt.materialSamples = lt.materialSamples[:0]
}

// Helper methods
func (m *Metrics) updateAverageLatencies() {
	appendAvg, queryAvg, materialAvg := m.latencyTracker.getAverages()
	
	m.mu.Lock()
	m.AvgAppendLatency = appendAvg
	m.AvgQueryLatency = queryAvg
	m.AvgMaterialLatency = materialAvg
	m.mu.Unlock()
}

func (m *Metrics) updateLastOperationTime() {
	m.mu.Lock()
	m.LastOperationTime = time.Now()
	m.mu.Unlock()
}

// Global metrics instance
var GlobalMetrics = NewMetrics()