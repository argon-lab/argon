package walcli

import (
	"context"
	"fmt"
	"os"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Services holds all WAL-related services for CLI use
type Services struct {
	WAL          *wal.Service
	Branches     *branchwal.BranchService
	Projects     *projectwal.ProjectService
	Materializer *materializer.Service
	TimeTravel   *timetravel.Service
	Restore      *restore.Service
	Monitor      *wal.Monitor
}

// NewServices creates and returns all WAL services
func NewServices() (*Services, error) {
	// Get MongoDB URI from environment or use default
	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}

	// Connect to MongoDB
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	// Use argon_wal database for WAL data
	db := client.Database("argon_wal")

	// Create services
	walService, err := wal.NewService(db)
	if err != nil {
		return nil, fmt.Errorf("failed to create WAL service: %w", err)
	}

	branchService, err := branchwal.NewBranchService(db, walService)
	if err != nil {
		return nil, fmt.Errorf("failed to create branch service: %w", err)
	}

	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	if err != nil {
		return nil, fmt.Errorf("failed to create project service: %w", err)
	}

	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	
	// Create monitor with production-ready configuration
	monitorConfig := wal.MonitorConfig{
		HealthCheckInterval:   30 * time.Second,
		MetricsReportInterval: 60 * time.Second,
		EnableLogging:         true,
		EnableMetricsExport:   true,
		AlertThresholds: wal.AlertThresholds{
			MaxErrorRate:           0.05,  // 5% error rate
			MaxLatency:            1 * time.Second,
			MaxConsecutiveFailures: 3,
			MinSuccessRate:        0.95, // 95% success rate
		},
	}
	monitor := wal.NewMonitor(wal.GlobalMetrics, monitorConfig)
	monitor.Start()

	return &Services{
		WAL:          walService,
		Branches:     branchService,
		Projects:     projectService,
		Materializer: materializerService,
		TimeTravel:   timeTravelService,
		Restore:      restoreService,
		Monitor:      monitor,
	}, nil
}