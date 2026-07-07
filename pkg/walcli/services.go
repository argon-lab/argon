package walcli

import (
	"context"
	"fmt"
	"os"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/gc"
	"github.com/argon-lab/argon/internal/importer"
	"github.com/argon-lab/argon/internal/ingest"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/migrate"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/snapshot"
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
	Importer     *importer.ImportService
	Migrate      *migrate.Service
	Snapshots    *snapshot.Service
	GC           *gc.Service
	Checkout     *checkout.Service
	Ingest       *ingest.Service
	Monitor      *wal.Monitor
	MongoURI     string
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

	materializerService := materializer.NewService(walService, branchService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	importerService := importer.NewImportService(walService, projectService, branchService)
	migrateService, err := migrate.NewService(db, branchService, materializerService)
	if err != nil {
		return nil, fmt.Errorf("failed to create migration service: %w", err)
	}
	// Chunk store backend from the environment: mongodb (default),
	// s3 (recommended for cloud) or filesystem (self-hosted).
	chunkStore, storeDesc, err := snapshot.NewChunkStoreFromEnv(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("failed to configure snapshot store: %w", err)
	}
	_ = storeDesc
	// Registers itself as the materializer's snapshot source.
	snapshotService, err := snapshot.NewServiceWithStore(db, branchService, materializerService, chunkStore)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot service: %w", err)
	}
	snapshotService.EnableAuto(snapshot.DefaultAutoConfig())
	gcService := gc.NewService(walService, branchService, snapshotService)
	checkoutService := checkout.NewService(client, db, branchService, materializerService)
	ingestService := ingest.NewService(client, db, walService, branchService)
	// Reclaim a deleted branch's WAL entries and snapshots. Safe because
	// regular deletion refuses branches with children.
	branchService.SetDeleteHook(func(branchID string) {
		if _, _, _, err := gcService.ReclaimDeletedBranch(context.Background(), branchID); err != nil {
			fmt.Printf("Warning: failed to reclaim storage for branch %s: %v\n", branchID, err)
		}
	})

	// Create monitor with production-ready configuration
	monitorConfig := wal.MonitorConfig{
		HealthCheckInterval:   30 * time.Second,
		MetricsReportInterval: 60 * time.Second,
		EnableLogging:         true,
		EnableMetricsExport:   true,
		AlertThresholds: wal.AlertThresholds{
			MaxErrorRate:           0.05, // 5% error rate
			MaxLatency:             1 * time.Second,
			MaxConsecutiveFailures: 3,
			MinSuccessRate:         0.95, // 95% success rate
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
		Importer:     importerService,
		Migrate:      migrateService,
		Snapshots:    snapshotService,
		GC:           gcService,
		Checkout:     checkoutService,
		Ingest:       ingestService,
		Monitor:      monitor,
		MongoURI:     mongoURI,
	}, nil
}

// BranchConnectionString renders the URI applications use to reach a
// checked-out branch's physical database.
func (s *Services) BranchConnectionString(physicalDB string) string {
	return checkout.ConnectionString(s.MongoURI, physicalDB)
}

// RunGC wraps garbage collection for CLI use: the cli module cannot import
// internal packages, so the config type stays behind this boundary.
func (s *Services) RunGC(ctx context.Context, projectID string, retention time.Duration, dryRun bool) (*gc.Report, error) {
	return s.GC.RunProject(ctx, projectID, gc.Config{RetentionWindow: retention, DryRun: dryRun})
}

// ImportPreview wraps the importer preview functionality for CLI use
func (s *Services) ImportPreview(ctx context.Context, mongoURI, databaseName string) (interface{}, error) {
	return s.Importer.PreviewImport(ctx, mongoURI, databaseName)
}

// ImportDatabase wraps the importer database functionality for CLI use
func (s *Services) ImportDatabase(ctx context.Context, mongoURI, databaseName, projectName string, dryRun bool, batchSize int) (interface{}, error) {
	// Use a map to avoid importing the internal types
	opts := map[string]interface{}{
		"mongo_uri":     mongoURI,
		"database_name": databaseName,
		"project_name":  projectName,
		"dry_run":       dryRun,
		"batch_size":    batchSize,
	}
	
	// Create a struct that matches the internal ImportOptions
	return s.callImportDatabase(ctx, opts)
}

// callImportDatabase creates the proper options struct and calls the service
func (s *Services) callImportDatabase(ctx context.Context, opts map[string]interface{}) (interface{}, error) {
	// Import the internal package here where it's allowed
	importOpts := importer.ImportOptions{
		MongoURI:     opts["mongo_uri"].(string),
		DatabaseName: opts["database_name"].(string),
		ProjectName:  opts["project_name"].(string),
		DryRun:       opts["dry_run"].(bool),
		BatchSize:    opts["batch_size"].(int),
	}
	
	return s.Importer.ImportDatabase(ctx, importOpts)
}
