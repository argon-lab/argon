package wal

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Monitor provides health monitoring and alerting for WAL operations
type Monitor struct {
	metrics        *Metrics
	healthChecks   []HealthCheck
	alerts         []Alert
	config         MonitorConfig
	
	// State tracking
	isHealthy      bool
	lastCheck      time.Time
	consecutiveFails int
	
	// Control
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.RWMutex
}

// MonitorConfig configures monitoring behavior
type MonitorConfig struct {
	HealthCheckInterval    time.Duration
	MetricsReportInterval  time.Duration
	AlertThresholds        AlertThresholds
	EnableLogging          bool
	EnableMetricsExport    bool
}

// AlertThresholds defines when to trigger alerts
type AlertThresholds struct {
	MaxErrorRate           float64       // Maximum acceptable error rate (0.0-1.0)
	MaxLatency             time.Duration // Maximum acceptable latency
	MaxConsecutiveFailures int           // Maximum consecutive health check failures
	MinSuccessRate         float64       // Minimum success rate (0.0-1.0)
}

// HealthCheck defines a health check function
type HealthCheck struct {
	Name        string
	Description string
	Check       func() error
	Interval    time.Duration
	Timeout     time.Duration
	Critical    bool // If true, failure means system unhealthy
}

// Alert represents a system alert
type Alert struct {
	Level       AlertLevel
	Title       string
	Message     string
	Timestamp   time.Time
	Data        map[string]interface{}
	Resolved    bool
	ResolvedAt  time.Time
}

// AlertLevel defines alert severity
type AlertLevel string

const (
	AlertLevelInfo     AlertLevel = "info"
	AlertLevelWarning  AlertLevel = "warning"
	AlertLevelError    AlertLevel = "error"
	AlertLevelCritical AlertLevel = "critical"
)

// NewMonitor creates a new WAL monitor
func NewMonitor(metrics *Metrics, config MonitorConfig) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	
	if config.HealthCheckInterval == 0 {
		config.HealthCheckInterval = 30 * time.Second
	}
	if config.MetricsReportInterval == 0 {
		config.MetricsReportInterval = 60 * time.Second
	}
	
	// Set default alert thresholds
	if config.AlertThresholds.MaxErrorRate == 0 {
		config.AlertThresholds.MaxErrorRate = 0.05 // 5% error rate
	}
	if config.AlertThresholds.MaxLatency == 0 {
		config.AlertThresholds.MaxLatency = 1 * time.Second
	}
	if config.AlertThresholds.MaxConsecutiveFailures == 0 {
		config.AlertThresholds.MaxConsecutiveFailures = 3
	}
	if config.AlertThresholds.MinSuccessRate == 0 {
		config.AlertThresholds.MinSuccessRate = 0.95 // 95% success rate
	}
	
	monitor := &Monitor{
		metrics:      metrics,
		healthChecks: make([]HealthCheck, 0),
		alerts:       make([]Alert, 0),
		config:       config,
		isHealthy:    true,
		ctx:          ctx,
		cancel:       cancel,
	}
	
	// Add default health checks
	monitor.addDefaultHealthChecks()
	
	return monitor
}

// Start begins monitoring
func (m *Monitor) Start() {
	m.wg.Add(2)
	
	go m.healthCheckLoop()
	go m.metricsReportLoop()
	
	if m.config.EnableLogging {
		log.Println("WAL Monitor: Started health monitoring")
	}
}

// Stop stops monitoring
func (m *Monitor) Stop() {
	m.cancel()
	m.wg.Wait()
	
	if m.config.EnableLogging {
		log.Println("WAL Monitor: Stopped health monitoring")
	}
}

// IsHealthy returns current health status
func (m *Monitor) IsHealthy() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isHealthy
}

// GetHealthStatus returns detailed health information
func (m *Monitor) GetHealthStatus() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	status := map[string]interface{}{
		"healthy":            m.isHealthy,
		"last_check":         m.lastCheck,
		"consecutive_fails":  m.consecutiveFails,
		"active_alerts":      len(m.getActiveAlerts()),
		"total_alerts":       len(m.alerts),
		"health_checks":      len(m.healthChecks),
	}
	
	// Add metrics summary
	snapshot := m.metrics.GetSnapshot()
	successRates := m.metrics.GetSuccessRate()
	
	status["metrics"] = map[string]interface{}{
		"total_operations": snapshot.AppendOps + snapshot.QueryOps + snapshot.MaterialOps,
		"success_rates":    successRates,
		"current_lsn":      snapshot.CurrentLSN,
		"active_branches":  snapshot.ActiveBranches,
		"active_projects":  snapshot.ActiveProjects,
		"last_operation":   snapshot.LastOperationTime,
	}
	
	return status
}

// GetActiveAlerts returns currently active alerts
func (m *Monitor) GetActiveAlerts() []Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getActiveAlerts()
}

// AddHealthCheck adds a custom health check
func (m *Monitor) AddHealthCheck(check HealthCheck) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.healthChecks = append(m.healthChecks, check)
}

// TriggerAlert creates a new alert
func (m *Monitor) TriggerAlert(level AlertLevel, title, message string, data map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	alert := Alert{
		Level:     level,
		Title:     title,
		Message:   message,
		Timestamp: time.Now(),
		Data:      data,
		Resolved:  false,
	}
	
	m.alerts = append(m.alerts, alert)
	
	if m.config.EnableLogging {
		log.Printf("WAL Monitor Alert [%s]: %s - %s", level, title, message)
	}
}

// ResolveAlert resolves an alert by title
func (m *Monitor) ResolveAlert(title string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	for i := range m.alerts {
		if m.alerts[i].Title == title && !m.alerts[i].Resolved {
			m.alerts[i].Resolved = true
			m.alerts[i].ResolvedAt = time.Now()
			
			if m.config.EnableLogging {
				log.Printf("WAL Monitor: Resolved alert '%s'", title)
			}
			break
		}
	}
}

// Private methods
func (m *Monitor) addDefaultHealthChecks() {
	// Database connectivity check
	m.healthChecks = append(m.healthChecks, HealthCheck{
		Name:        "database_connectivity",
		Description: "Verify MongoDB connection is active",
		Check:       m.checkDatabaseConnectivity,
		Interval:    30 * time.Second,
		Timeout:     5 * time.Second,
		Critical:    true,
	})
	
	// Performance check
	m.healthChecks = append(m.healthChecks, HealthCheck{
		Name:        "performance_metrics",
		Description: "Check operation latencies and success rates",
		Check:       m.checkPerformanceMetrics,
		Interval:    60 * time.Second,
		Timeout:     1 * time.Second,
		Critical:    false,
	})
	
	// Memory usage check
	m.healthChecks = append(m.healthChecks, HealthCheck{
		Name:        "memory_usage",
		Description: "Monitor memory usage and detect leaks",
		Check:       m.checkMemoryUsage,
		Interval:    120 * time.Second,
		Timeout:     1 * time.Second,
		Critical:    false,
	})
}

func (m *Monitor) healthCheckLoop() {
	defer m.wg.Done()
	
	ticker := time.NewTicker(m.config.HealthCheckInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.runHealthChecks()
		}
	}
}

func (m *Monitor) metricsReportLoop() {
	defer m.wg.Done()
	
	ticker := time.NewTicker(m.config.MetricsReportInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if m.config.EnableMetricsExport {
				m.exportMetrics()
			}
		}
	}
}

func (m *Monitor) runHealthChecks() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.lastCheck = time.Now()
	allHealthy := true
	
	for _, check := range m.healthChecks {
		err := m.runSingleHealthCheck(check)
		if err != nil {
			if check.Critical {
				allHealthy = false
			}
			
			// Trigger alert for failed check
			m.triggerHealthCheckAlert(check, err)
		} else {
			// Resolve any existing alerts for this check
			m.ResolveAlert(fmt.Sprintf("health_check_%s", check.Name))
		}
	}
	
	if allHealthy {
		m.consecutiveFails = 0
		if !m.isHealthy {
			m.isHealthy = true
			m.ResolveAlert("system_unhealthy")
		}
	} else {
		m.consecutiveFails++
		if m.consecutiveFails >= m.config.AlertThresholds.MaxConsecutiveFailures {
			m.isHealthy = false
			m.TriggerAlert(AlertLevelCritical, "system_unhealthy", 
				fmt.Sprintf("System unhealthy after %d consecutive failures", m.consecutiveFails),
				map[string]interface{}{
					"consecutive_failures": m.consecutiveFails,
					"last_check": m.lastCheck,
				})
		}
	}
}

func (m *Monitor) runSingleHealthCheck(check HealthCheck) error {
	done := make(chan error, 1)
	
	go func() {
		done <- check.Check()
	}()
	
	select {
	case err := <-done:
		return err
	case <-time.After(check.Timeout):
		return fmt.Errorf("health check '%s' timed out after %v", check.Name, check.Timeout)
	}
}

func (m *Monitor) triggerHealthCheckAlert(check HealthCheck, err error) {
	level := AlertLevelWarning
	if check.Critical {
		level = AlertLevelError
	}
	
	title := fmt.Sprintf("health_check_%s", check.Name)
	message := fmt.Sprintf("Health check '%s' failed: %v", check.Name, err)
	
	data := map[string]interface{}{
		"check_name":        check.Name,
		"check_description": check.Description,
		"error":             err.Error(),
		"critical":          check.Critical,
	}
	
	alert := Alert{
		Level:     level,
		Title:     title,
		Message:   message,
		Timestamp: time.Now(),
		Data:      data,
		Resolved:  false,
	}
	
	m.alerts = append(m.alerts, alert)
}

func (m *Monitor) getActiveAlerts() []Alert {
	active := make([]Alert, 0)
	for _, alert := range m.alerts {
		if !alert.Resolved {
			active = append(active, alert)
		}
	}
	return active
}

func (m *Monitor) exportMetrics() {
	snapshot := m.metrics.GetSnapshot()
	successRates := m.metrics.GetSuccessRate()
	
	if m.config.EnableLogging {
		log.Printf("WAL Metrics: Ops=%d/%d/%d, Errors=%d/%d/%d, LSN=%d, Success=%.2f/%.2f/%.2f",
			snapshot.AppendOps, snapshot.QueryOps, snapshot.MaterialOps,
			snapshot.AppendErrors, snapshot.QueryErrors, snapshot.MaterialErrors,
			snapshot.CurrentLSN,
			successRates["append"], successRates["query"], successRates["materialization"])
	}
}

// Health check implementations
func (m *Monitor) checkDatabaseConnectivity() error {
	// This would typically ping the database
	// For now, we'll assume it's healthy
	return nil
}

func (m *Monitor) checkPerformanceMetrics() error {
	snapshot := m.metrics.GetSnapshot()
	successRates := m.metrics.GetSuccessRate()
	
	// Check latency thresholds
	if snapshot.AvgAppendLatency > m.config.AlertThresholds.MaxLatency {
		return fmt.Errorf("append latency %v exceeds threshold %v", 
			snapshot.AvgAppendLatency, m.config.AlertThresholds.MaxLatency)
	}
	
	if snapshot.AvgQueryLatency > m.config.AlertThresholds.MaxLatency {
		return fmt.Errorf("query latency %v exceeds threshold %v", 
			snapshot.AvgQueryLatency, m.config.AlertThresholds.MaxLatency)
	}
	
	// Check success rates
	for operation, rate := range successRates {
		if rate < m.config.AlertThresholds.MinSuccessRate {
			return fmt.Errorf("%s success rate %.2f below threshold %.2f", 
				operation, rate, m.config.AlertThresholds.MinSuccessRate)
		}
	}
	
	return nil
}

func (m *Monitor) checkMemoryUsage() error {
	// This would typically check memory usage
	// For now, we'll assume it's healthy
	return nil
}