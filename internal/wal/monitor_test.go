package wal

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise Start's actual ticker and Stop's WaitGroup, rather than just calling
// alert helpers. Before the fix both successful checks and the critical-failure
// threshold re-entered the monitor mutex, preventing Stop from ever returning.
func TestMonitorTickerStopsAfterHealthChecks(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := "healthy"
		if failing {
			name = "critical_failure"
		}
		t.Run(name, func(t *testing.T) {
			m := NewMonitor(NewMetrics(), MonitorConfig{
				HealthCheckInterval: 5 * time.Millisecond,
				AlertThresholds:     AlertThresholds{MaxConsecutiveFailures: 1},
			})
			var calls atomic.Int32
			check := HealthCheck{Name: "regression", Critical: true, Timeout: time.Second,
				Check: func() error {
					calls.Add(1)
					if failing {
						return errors.New("unavailable")
					}
					return nil
				},
			}
			if failing {
				m.healthChecks = []HealthCheck{check}
			} else {
				// Keep the default healthy checks: their first successful alert
				// resolution caused the observed released-binary deadlock.
				m.AddHealthCheck(check)
			}
			m.Start()
			defer m.cancel()
			deadline := time.Now().Add(2 * time.Second)
			for calls.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if calls.Load() < 2 {
				t.Fatal("health ticker did not complete multiple rounds")
			}
			stopped := make(chan struct{})
			go func() { m.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop blocked after health-check ticks")
			}
		})
	}
}

func TestMonitorHealthFailureAndRecovery(t *testing.T) {
	m := NewMonitor(NewMetrics(), MonitorConfig{
		AlertThresholds: AlertThresholds{MaxConsecutiveFailures: 2},
	})
	defer m.cancel()
	failure := errors.New("unavailable")
	m.healthChecks = []HealthCheck{{Name: "storage", Critical: true, Timeout: time.Second,
		Check: func() error { return failure },
	}}
	runRound := func() {
		t.Helper()
		done := make(chan struct{})
		go func() { m.runHealthChecks(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("health check deadlocked while changing alert state")
		}
	}
	runRound()
	if !m.IsHealthy() || m.consecutiveFails != 1 {
		t.Fatal("a single failure must not cross the configured threshold")
	}
	runRound()
	if m.IsHealthy() || m.consecutiveFails != 2 {
		t.Fatal("repeated critical failures must mark the monitor unhealthy")
	}
	var systemAlert bool
	for _, alert := range m.GetActiveAlerts() {
		if alert.Title == "system_unhealthy" && alert.Level == AlertLevelCritical {
			systemAlert = true
		}
	}
	if !systemAlert {
		t.Fatal("missing critical system alert")
	}

	failure = nil
	runRound()
	if !m.IsHealthy() || m.consecutiveFails != 0 {
		t.Fatal("a successful round must restore healthy state")
	}
	for _, alert := range m.alerts {
		if alert.Title == "system_unhealthy" && (!alert.Resolved || alert.ResolvedAt.IsZero()) {
			t.Fatal("recovery did not resolve the system alert")
		}
	}
	// Existing semantics resolve one matching alert per round. A second healthy
	// round clears the second recorded failure for this check.
	runRound()
	if len(m.GetActiveAlerts()) != 0 {
		t.Fatal("successful rounds left unresolved health-check alerts")
	}
	m.TriggerAlert(AlertLevelInfo, "manual", "verification", nil)
	m.ResolveAlert("manual")
	if len(m.GetActiveAlerts()) != 0 {
		t.Fatal("public alert methods no longer resolve alerts")
	}
}
