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
	"github.com/argon-lab/argon/internal/merge"
	"github.com/argon-lab/argon/internal/migrate"
	"github.com/argon-lab/argon/internal/pin"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/sandbox"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/undo"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/argon-lab/argon/internal/wireproxy"
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
	Undo         *undo.Service
	Merge        *merge.Service
	Sandbox      *sandbox.Service
	Pins         *pin.Service
	Monitor      *wal.Monitor
	MongoURI     string
	// Client is the deployment connection, exposed for tools that read
	// physical branch databases (e.g. convergence verification).
	Client *mongo.Client
}

// NewServices creates all WAL services against the deployment named by
// MONGODB_URI (default localhost) and the standard argon_wal metadata
// database.
func NewServices() (*Services, error) {
	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	return NewServicesAt(mongoURI, "argon_wal")
}

// NewServicesAt creates all WAL services against an explicit deployment and
// metadata database — for embedding (REST server, tests) where the global
// environment must not decide.
func NewServicesAt(mongoURI, dbName string) (*Services, error) {
	// Connect to MongoDB
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	db := client.Database(dbName)

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
	undoService := undo.NewService(walService, branchService, client)
	mergeService := merge.NewService(db, walService, branchService, materializerService, client)
	sandboxService := sandbox.NewService(branchService, checkoutService)
	pinService, err := pin.NewService(db, branchService)
	if err != nil {
		return nil, fmt.Errorf("failed to create pin service: %w", err)
	}
	// Pinned history must survive GC, and pinned branches must survive
	// deletion.
	gcService.SetPinLookup(pinService.LSNsForBranch)
	branchService.SetDeleteGuard(pinService.RequireNoPins)
	// Snapshot immediately after imports: an imported history is otherwise
	// pure linear replay until something trips the auto-snapshot threshold.
	importerService.SetImportedHook(func(branch *wal.Branch) {
		if _, err := snapshotService.CreateSnapshot(context.Background(), branch.ID, branch.HeadLSN); err != nil {
			fmt.Printf("Warning: post-import snapshot failed for branch %s: %v\n", branch.ID, err)
		}
	})
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
		Undo:         undoService,
		Merge:        mergeService,
		Sandbox:      sandboxService,
		Pins:         pinService,
		Monitor:      monitor,
		MongoURI:     mongoURI,
		Client:       client,
	}, nil
}

// WriterFor returns a programmatic writer for a branch — the public write
// entry point for tools and benchmarks outside this module (which cannot
// import internal packages but can call methods on the returned value).
// Writers append explicit document states; see the walwriter package.
func (s *Services) WriterFor(projectName, branchName string) (*walwriter.Writer, error) {
	project, err := s.Projects.GetProjectByName(projectName)
	if err != nil {
		return nil, fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if branchName == "" {
		branchName = "main"
	}
	branch, err := s.Branches.GetBranch(project.ID, branchName)
	if err != nil {
		return nil, fmt.Errorf("branch %q not found: %w", branchName, err)
	}
	writer := walwriter.New(s.WAL, s.Branches, s.Materializer, branch)
	writer.SetAutoSnapshotter(s.Snapshots)
	return writer, nil
}

// BuildUndoPlan and ApplyUndoPlan wrap the undo service for CLI use (the
// cli module cannot import internal packages).
func (s *Services) BuildUndoPlan(branchID string, fromLSN, toLSN int64, actor string) (*undo.Plan, error) {
	branch, err := s.Branches.GetBranchByID(branchID)
	if err != nil {
		return nil, err
	}
	return s.Undo.BuildPlan(branch, fromLSN, toLSN, actor)
}

// ApplyUndoPlan executes a previously built plan.
func (s *Services) ApplyUndoPlan(ctx context.Context, branchID string, plan *undo.Plan) (restored, deleted int, err error) {
	branch, err := s.Branches.GetBranchByID(branchID)
	if err != nil {
		return 0, 0, err
	}
	return s.Undo.Apply(ctx, branch, plan)
}

// NewWireProxy builds the wire-protocol proxy over these services (the
// cli module cannot import internal packages).
func (s *Services) NewWireProxy(upstreamAddr string) *wireproxy.Proxy {
	return wireproxy.New(upstreamAddr, s.Projects, s.Branches)
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
