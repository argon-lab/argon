// Package checkout materializes branches into real MongoDB databases —
// "mongod as compute". A checked-out branch gets a physical database that
// applications connect to with any unmodified MongoDB driver: queries,
// indexes, aggregation and transactions are executed by mongod itself
// instead of an in-process reimplementation. The WAL remains the versioned
// source of truth; while a branch is live, its WAL is fed from the
// database's change stream (see the ingest package) rather than the SDK
// write path.
package checkout

import (
	"context"
	"fmt"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// insertBatchSize bounds bulk-insert batches during materialization.
const insertBatchSize = 1000

// Service checks branches out into physical databases and back.
type Service struct {
	client       *mongo.Client
	branches     *branchwal.BranchService
	materializer *materializer.Service
}

// NewService creates a checkout service. The client must be the same
// deployment that holds the Argon metadata: physical branch databases live
// alongside it.
func NewService(client *mongo.Client, branches *branchwal.BranchService, mat *materializer.Service) *Service {
	return &Service{client: client, branches: branches, materializer: mat}
}

// PhysicalDBName is the database a branch materializes into. Branch IDs
// are globally unique, so the project doesn't need to appear; the fixed
// prefix keeps Argon-owned databases recognizable and clear of user names.
func PhysicalDBName(branchID string) string {
	return "argon_br_" + branchID
}

// Info describes a completed checkout.
type Info struct {
	BranchID    string
	PhysicalDB  string
	LSN         int64
	Collections int
	Documents   int64
}

// Checkout materializes the branch's state at its current head into its
// physical database and marks the branch live. Re-running refreshes the
// database to the branch's current WAL state (any direct writes since the
// previous checkout that have already been ingested are preserved by
// definition; un-ingested ones would be lost, so refresh while the
// ingester is stopped or drained).
func (s *Service) Checkout(ctx context.Context, branchID string) (*Info, error) {
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return nil, fmt.Errorf("branch %s not found: %w", branchID, err)
	}

	state, err := s.materializer.MaterializeBranch(branch)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize branch: %w", err)
	}

	dbName := PhysicalDBName(branch.ID)
	physical := s.client.Database(dbName)

	// Idempotent refresh: rebuild from the WAL state.
	if err := physical.Drop(ctx); err != nil {
		return nil, fmt.Errorf("failed to reset physical database: %w", err)
	}

	info := &Info{BranchID: branch.ID, PhysicalDB: dbName, LSN: branch.HeadLSN}
	for collection, docs := range state {
		count, err := s.loadCollection(ctx, physical, collection, docs)
		if err != nil {
			return nil, fmt.Errorf("collection %s: %w", collection, err)
		}
		info.Collections++
		info.Documents += count
	}

	if err := s.branches.SetCheckoutState(branch.ID, dbName, wal.BranchStateLive, branch.HeadLSN); err != nil {
		return nil, fmt.Errorf("failed to mark branch live: %w", err)
	}
	return info, nil
}

// loadCollection bulk-inserts one collection's state and prepares it for
// change-stream capture.
func (s *Service) loadCollection(ctx context.Context, physical *mongo.Database, collection string, docs map[string]bson.M) (int64, error) {
	// Pre/post images give the ingester exact document images on update
	// and delete events. Best effort: unsupported deployments still work
	// through updateLookup, with pre-images absent.
	if err := EnablePrePostImages(ctx, physical, collection); err != nil {
		return 0, err
	}
	if len(docs) == 0 {
		return 0, nil
	}

	coll := physical.Collection(collection)
	batch := make([]interface{}, 0, insertBatchSize)
	var total int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := coll.InsertMany(ctx, batch, options.InsertMany().SetOrdered(false)); err != nil {
			return fmt.Errorf("failed to load documents: %w", err)
		}
		total += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	for _, doc := range docs {
		batch = append(batch, doc)
		if len(batch) >= insertBatchSize {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

// EnablePrePostImages turns on change-stream pre/post images for a
// collection, creating it if needed. Failures on deployments that don't
// support the option (pre-6.0) are ignored — capture degrades to
// updateLookup post-images.
func EnablePrePostImages(ctx context.Context, physical *mongo.Database, collection string) error {
	err := physical.RunCommand(ctx, bson.D{
		{Key: "create", Value: collection},
		{Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": true}},
	}).Err()
	if err == nil {
		return nil
	}
	// Collection may already exist: collMod instead.
	modErr := physical.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: collection},
		{Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": true}},
	}).Err()
	if modErr == nil {
		return nil
	}
	// Unsupported server or option: proceed without pre-images.
	return nil
}

// Release drops the physical database and returns the branch to
// metadata-only state. The WAL keeps everything; a later checkout rebuilds
// the database. Un-ingested direct writes are lost — drain the ingester
// first.
func (s *Service) Release(ctx context.Context, branchID string) error {
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return fmt.Errorf("branch %s not found: %w", branchID, err)
	}
	if branch.PhysicalDB != "" {
		if err := s.client.Database(branch.PhysicalDB).Drop(ctx); err != nil {
			return fmt.Errorf("failed to drop physical database: %w", err)
		}
	}
	return s.branches.SetCheckoutState(branch.ID, "", "", 0)
}

// ConnectionString renders the URI applications use to reach a checked-out
// branch, based on the deployment URI Argon itself uses.
func ConnectionString(baseURI, physicalDB string) string {
	if baseURI == "" {
		baseURI = "mongodb://localhost:27017"
	}
	// Insert the database into the URI: mongodb://host[:port][/db][?opts]
	// Keep it simple and robust for the common forms.
	base := baseURI
	var query string
	if i := indexByte(base, '?'); i >= 0 {
		query = base[i:]
		base = base[:i]
	}
	// Strip a trailing default database path if present.
	if i := indexByteAfterScheme(base, '/'); i >= 0 {
		base = base[:i]
	}
	return base + "/" + physicalDB + query
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// indexByteAfterScheme finds the first occurrence of c after "://".
func indexByteAfterScheme(s string, c byte) int {
	start := 0
	for i := 0; i+2 < len(s); i++ {
		if s[i] == ':' && s[i+1] == '/' && s[i+2] == '/' {
			start = i + 3
			break
		}
	}
	for i := start; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
