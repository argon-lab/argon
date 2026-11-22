package walcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/indexspec"
	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/migrate"
	"github.com/argon-lab/argon/v2/internal/pin"
	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/restore"
	"github.com/argon-lab/argon/v2/internal/sandbox"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/timetravel"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/argon-lab/argon/v2/internal/wireproxy"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
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
	// Sources records which external database a branch follows.
	Sources *source.Registry
	// Deprecated: Monitor does nothing; it remains for callers that stop it.
	Monitor  *wal.Monitor
	MongoURI string
	// Client is the deployment connection, exposed for tools that read
	// physical branch databases (e.g. convergence verification).
	Client *mongo.Client
}

// NewServices creates all WAL services against the deployment named by
// MONGODB_URI (default localhost) and the standard argon_wal metadata
// database.
func NewServices() (*Services, error) {
	return NewServicesAt(deploymentFromEnv())
}

func deploymentFromEnv() (mongoURI, metadataDB string) {
	mongoURI = os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	metadataDB = os.Getenv("ARGON_METADATA_DB")
	if metadataDB == "" {
		metadataDB = "argon_wal"
	}
	return mongoURI, metadataDB
}

// ErrUnreachable reports that the deployment named by MONGODB_URI did not
// answer: nothing is listening there, the connection settings cannot be used,
// the replica set has no primary, or the server turned the connection away.
var ErrUnreachable = errors.New("MongoDB is not reachable")

// Connect is NewServices for a process that can be started before its
// database, such as an MCP server a client launches only to list its tools.
// It stops waiting for the deployment to answer when ctx ends, instead of
// sitting out the driver's server selection (30 seconds by default), and
// reports that as ErrUnreachable. A failed call leaves nothing running, so
// the caller can simply call again later. Once the deployment has answered,
// setting up on it is not interrupted.
func Connect(ctx context.Context) (*Services, error) {
	mongoURI, metadataDB := deploymentFromEnv()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	// Nearly everything Argon does writes metadata, so a deployment without
	// a primary is not one it can use yet.
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		disconnect(client)
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return newServices(client, mongoURI, metadataDB)
}

// NewServicesAt creates all WAL services against an explicit deployment and
// metadata database — for embedding (REST server, tests) where the global
// environment must not decide.
func NewServicesAt(mongoURI, dbName string) (*Services, error) {
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(mongoURI))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	return newServices(client, mongoURI, dbName)
}

func disconnect(client *mongo.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = client.Disconnect(ctx)
}

// newServices builds the services on a client and owns it from then on.
func newServices(client *mongo.Client, mongoURI, dbName string) (*Services, error) {
	// If the services cannot be built the client is closed: a caller that
	// tries again would otherwise leave one behind per attempt, each still
	// probing the deployment.
	built := false
	defer func() {
		if !built {
			disconnect(client)
		}
	}()
	ctx := context.Background()
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
	sourceRegistry, err := source.NewRegistry(db)
	if err != nil {
		return nil, err
	}
	checkoutService := checkout.NewService(client, db, branchService, materializerService)
	// A followed source database is never one of Argon's physical databases;
	// this is the second, independent check behind checkout's own.
	checkoutService.SetDropGuard(sourceRegistry.GuardDrop)
	ingestService := ingest.NewService(client, db, walService, branchService)
	indexStore := indexspec.NewStore(db)
	checkoutService.SetIndexStore(indexStore)
	importerService.SetIndexStore(indexStore)
	ingestService.SetIndexStore(indexStore)
	importerService.EnableOnline(db, sourceRegistry, ingestService)
	// A removed import leaves no snapshot of the data it copied.
	projectService.SetImportAbortHook(func(ctx context.Context, branchIDs []string) error {
		for _, branchID := range branchIDs {
			if _, _, err := snapshotService.CleanupBranch(ctx, branchID); err != nil {
				return err
			}
			if err := indexStore.Forget(ctx, branchID); err != nil {
				return err
			}
		}
		return nil
	})
	ingestService.SetAutoSnapshotter(snapshotService)
	checkoutService.SetBeforeRelease(func(ctx context.Context, branchID string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return ingestService.Stop(ctx, branchID)
	})
	checkoutService.SetBeforeDiscard(ingestService.Cancel)
	// A checkout writes the branch's whole state through the oplog. A parent
	// that is checked out but not being watched would have all of it to scan
	// the next time its capture is drained, which is the first diff or merge
	// preview of the new branch. Reading the parent's stream while the copy
	// runs spreads that scan over the copy instead. This is an optimization
	// only. A parent without a persisted actor is left for its first watcher
	// to label, and a parent whose capture cannot start is left for the later
	// drain to report.
	checkoutService.SetDuringLoad(func(ctx context.Context, parentID string) func(context.Context) {
		parent, err := branchService.GetBranchByIDContext(ctx, parentID)
		if err != nil || !parent.IsLive() || parent.IsFollower() {
			return nil
		}
		startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, found, err := ingestService.PersistedActor(startCtx, parentID); err != nil || !found {
			return nil
		}
		if err := ingestService.Start(startCtx, parentID); err != nil {
			return nil
		}
		return func(ctx context.Context) {
			drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_ = ingestService.Drain(drainCtx, parentID)
		}
	})
	undoService := undo.NewService(walService, branchService, client, materializerService)
	mergeService := merge.NewService(db, walService, branchService, materializerService, client)
	if err := mergeService.EnsureIndexes(ctx); err != nil {
		return nil, fmt.Errorf("failed to create merge plan indexes: %w", err)
	}
	sandboxService := sandbox.NewService(branchService, checkoutService)
	pinService, err := pin.NewService(db, branchService)
	if err != nil {
		return nil, fmt.Errorf("failed to create pin service: %w", err)
	}
	// Pinned history must survive GC, and pinned branches must survive
	// deletion. A pin is only accepted for a state that can still be read.
	pinService.SetReadableCheck(materializerService.ValidateReadable)
	gcService.SetPinLookup(pinService.LSNsForBranch)
	branchService.SetDeleteGuard(pinService.RequireNoPins)
	// Snapshot immediately after imports: an imported history is otherwise
	// pure linear replay until something trips the auto-snapshot threshold.
	importerService.SetImportedHook(func(branch *wal.Branch) {
		if _, err := snapshotService.CreateSnapshot(context.Background(), branch.ID, branch.HeadLSN); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: post-import snapshot failed for branch %s: %v\n", branch.ID, err)
		}
	})
	// Reclaim a deleted branch's WAL entries and snapshots. Safe because
	// regular deletion refuses branches with children.
	branchService.SetDeleteHook(func(branchID string) {
		if err := ingestService.ClearResumeState(context.Background(), branchID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to reclaim capture state for %s: %v\n", branchID, err)
		}
		if err := indexStore.Forget(context.Background(), branchID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to forget index definitions of branch %s: %v\n", branchID, err)
		}
		if _, _, _, err := gcService.ReclaimDeletedBranch(context.Background(), branchID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to reclaim storage for branch %s: %v\n", branchID, err)
		}
	})

	built = true
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
		Sources:      sourceRegistry,
		Monitor:      &wal.Monitor{},
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

// UndoPlan is an undo plan, for callers outside the module.
type UndoPlan = undo.Plan

// BuildUndoPlan and ApplyUndoPlan wrap the undo service for CLI use (the
// cli module cannot import internal packages).
func (s *Services) BuildUndoPlan(branchID string, fromLSN, toLSN int64, actor string) (*undo.Plan, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Ingest.Drain(ctx, branchID); err != nil {
		return nil, err
	}
	branch, err := s.Branches.GetBranchByID(branchID)
	if err != nil {
		return nil, err
	}
	return s.Undo.BuildPlan(branch, fromLSN, toLSN, actor)
}

// BuildMergeUndoPlan plans the undo of one applied merge, on the branch it
// was merged into, and returns that branch's ID with the plan. Capture is
// drained first, so a merge into a checked-out branch is undone only once
// all of it is recorded.
func (s *Services) BuildMergeUndoPlan(ctx context.Context, planID string) (string, *undo.Plan, error) {
	id, err := primitive.ObjectIDFromHex(planID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid merge plan id %q", planID)
	}
	mergePlan, err := s.Merge.GetPlan(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if mergePlan.Status != merge.StatusApplied {
		return "", nil, fmt.Errorf("merge plan %s is %s, not applied: there is nothing to undo", planID, mergePlan.Status)
	}
	drain, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.Ingest.Drain(drain, mergePlan.TargetBranchID); err != nil {
		return "", nil, err
	}
	branch, err := s.Branches.GetBranchByID(mergePlan.TargetBranchID)
	if err != nil {
		return "", nil, err
	}
	plan, err := s.Undo.BuildMergePlan(branch, planID, mergePlan.MergeFromLSN, mergePlan.MergeChanges)
	if err != nil {
		return "", nil, err
	}
	return branch.ID, plan, nil
}

// ApplyUndoPlan executes a previously built plan.
func (s *Services) ApplyUndoPlan(ctx context.Context, branchID string, plan *undo.Plan) (restored, deleted int, err error) {
	branch, err := s.Branches.GetBranchByID(branchID)
	if err != nil {
		return 0, 0, err
	}
	restored, deleted, err = s.Undo.Apply(ctx, branch, plan)
	if err != nil {
		return 0, 0, err
	}
	if branch.IsLive() {
		if err := s.Ingest.Drain(ctx, branchID); err != nil {
			return restored, deleted, err
		}
	}
	return restored, deleted, nil
}

// StartCapture configures a branch/run label, not per-writer authentication.
func (s *Services) StartCapture(ctx context.Context, branchID, actor string) error {
	return s.Ingest.Start(ctx, branchID, ingest.WithActor(actor))
}

// RunCapture is the blocking CLI capture entry point with a branch/run actor.
func (s *Services) RunCapture(ctx context.Context, branchID, actor string) error {
	return s.Ingest.Run(ctx, branchID, ingest.WithActor(actor))
}

// PrepareCollection must precede application writes to a new collection;
// enabling images after its first update cannot recover missing images.
func (s *Services) PrepareCollection(ctx context.Context, branchID, name string) error {
	branch, err := s.Branches.GetBranchByID(branchID)
	if err != nil {
		return err
	}
	if !branch.IsLive() {
		return fmt.Errorf("branch is not checked out; run checkout first")
	}
	if name == "" || strings.HasPrefix(name, "__argon_") {
		return fmt.Errorf("choose a non-empty collection name outside the reserved __argon_ namespace")
	}
	own, err := branch.OwnDatabase()
	if err != nil {
		return err
	}
	return checkout.EnablePrePostImages(ctx, s.Client.Database(own), name)
}

// SyncBranch makes completed native writes visible to versioned control-plane
// operations. A merge also compares the parent, so drain both live databases.
func (s *Services) SyncBranch(ctx context.Context, branchID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.Ingest.Drain(ctx, branchID); err != nil {
		return err
	}
	branch, err := s.Branches.GetBranchByID(branchID)
	if err != nil {
		return err
	}
	if branch.ParentID != "" {
		return s.Ingest.Drain(ctx, branch.ParentID)
	}
	return nil
}

// RunSandboxSweeper reaps expired sandboxes while a managed API/MCP server runs.
// CLI-only deployments can continue to schedule `argon sandbox sweep` themselves.
func (s *Services) RunSandboxSweeper(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			projects, err := s.Projects.ListProjects()
			if err != nil {
				fmt.Fprintf(os.Stderr, "sandbox sweep: %v\n", err)
				continue
			}
			for _, project := range projects {
				passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				report, err := s.Sandbox.Sweep(passCtx, project.ID)
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "sandbox sweep: %v\n", err)
				} else {
					for _, skipped := range report.Skipped {
						fmt.Fprintf(os.Stderr, "sandbox sweep skipped: %s\n", skipped)
					}
				}
			}
		}
	}
}

// SynchronousSnapshots keeps automatic snapshot work inside short-lived CLI
// commands, so process exit cannot abandon a background snapshot's storage lock.
func (s *Services) SynchronousSnapshots() {
	cfg := snapshot.DefaultAutoConfig()
	cfg.Synchronous = true
	s.Snapshots.EnableAuto(cfg)
}

// WaitAuto stops new automatic snapshots and waits for their publication/lock
// cleanup. Shutdown callers must stop producers first and keep Mongo connected
// until this returns; an error reports an incomplete bounded shutdown.
func (s *Services) WaitAuto(ctx context.Context) error {
	return s.Snapshots.WaitAuto(ctx)
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

// ResolveBranch loads a branch by its project and branch names.
func (s *Services) ResolveBranch(projectName, branchName string) (*wal.Branch, error) {
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
	return branch, nil
}

// CreateSandbox returns the URI to hand to the caller at creation time.
func (s *Services) CreateSandbox(ctx context.Context, projectID, parentID, name string, ttl time.Duration, credential bool) (*sandbox.Info, string, error) {
	if credential {
		info, err := s.Sandbox.CreateWithCredential(ctx, projectID, parentID, name, ttl, s.MongoURI)
		if err != nil {
			return nil, "", err
		}
		return info, info.CredentialURI, nil
	}
	info, err := s.Sandbox.Create(ctx, projectID, parentID, name, ttl)
	if err != nil {
		return nil, "", err
	}
	return info, s.BranchConnectionString(info.PhysicalDB), nil
}

// SandboxCaptureFailure removes a credential sandbox if capture failed before
// its one-time URI was handed out. An ordinary sandbox can resume capture later.
func (s *Services) SandboxCaptureFailure(info *sandbox.Info, cause error) error {
	if info.CredentialURI == "" {
		return cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Sandbox.Discard(ctx, info.BranchID); err != nil {
		return fmt.Errorf("capture failed: %w; discard sandbox %q to finish cleanup: %v", cause, info.BranchName, err)
	}
	return fmt.Errorf("capture failed and sandbox %q was discarded: %w", info.BranchName, cause)
}

// ErrBranchExists reports a branch or sandbox name already used in the project.
var ErrBranchExists = branchwal.ErrBranchExists

// ErrCredentialUnavailable reports a deployment that cannot issue sandbox
// credentials: it does not enforce authentication, or the engine may not
// manage users.
var ErrCredentialUnavailable = checkout.ErrCredentialUnavailable

// ErrSandboxURIOnce is returned when a caller asks for a scoped URI again.
var ErrSandboxURIOnce = errors.New("the sandbox connection string was returned at creation and cannot be retrieved again")

// RunGC wraps garbage collection for CLI use: the cli module cannot import
// internal packages, so the config type stays behind this boundary.
func (s *Services) RunGC(ctx context.Context, projectID string, retention time.Duration, dryRun bool) (*gc.Report, error) {
	return s.GC.RunProject(ctx, projectID, gc.Config{RetentionWindow: retention, DryRun: dryRun})
}

// ImportPreview estimates what an import would copy without opening metadata services.
func ImportPreview(ctx context.Context, mongoURI, databaseName string, online bool) (interface{}, error) {
	return importer.PreviewImport(ctx, mongoURI, databaseName, online)
}

// ImportPreview wraps the quiesced preview for existing callers.
func (s *Services) ImportPreview(ctx context.Context, mongoURI, databaseName string) (interface{}, error) {
	return ImportPreview(ctx, mongoURI, databaseName, false)
}

// ImportDatabase wraps the importer database functionality for CLI use
func (s *Services) ImportDatabase(ctx context.Context, mongoURI, databaseName, projectName string, dryRun bool, batchSize int, sourceQuiesced ...bool) (interface{}, error) {
	// The optional acknowledgment preserves source compatibility while failing
	// closed for writes until a caller confirms the source is quiesced.
	importOpts := importer.ImportOptions{
		MongoURI: mongoURI, DatabaseName: databaseName, ProjectName: projectName,
		DryRun: dryRun, BatchSize: batchSize,
		SourceQuiesced: len(sourceQuiesced) > 0 && sourceQuiesced[0],
	}

	return s.Importer.ImportDatabase(ctx, importOpts)
}
