package snapshot

import (
	"context"
	"fmt"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service creates and serves collection snapshots.
type Service struct {
	manifests    *mongo.Collection
	chunks       *mongo.Collection // raw handle for GC reference checks
	store        ChunkStore
	branches     *branchwal.BranchService
	materializer *materializer.Service
	compressor   *wal.Compressor

	// Auto-snapshot state (see auto.go).
	autoMu       sync.Mutex
	autoCfg      *AutoConfig
	autoBranches map[string]*autoState
}

// NewService creates a snapshot service with the default (MongoDB) chunk
// store. It registers itself as the materializer's snapshot source, so
// materialization transparently starts from the nearest usable snapshot
// instead of replaying from the branch root.
func NewService(db *mongo.Database, branches *branchwal.BranchService, mat *materializer.Service) (*Service, error) {
	return NewServiceWithStore(db, branches, mat, NewMongoChunkStore(db))
}

// NewServiceWithStore creates a snapshot service over a specific chunk
// store backend (MongoDB, filesystem or S3 — see NewChunkStoreFromEnv).
// Manifests always stay in MongoDB; only chunk data moves.
func NewServiceWithStore(db *mongo.Database, branches *branchwal.BranchService, mat *materializer.Service, store ChunkStore) (*Service, error) {
	compressor, err := wal.NewCompressor(nil)
	if err != nil {
		return nil, err
	}

	s := &Service{
		manifests:    db.Collection("wal_snapshots"),
		chunks:       db.Collection("wal_snapshot_chunks"),
		store:        store,
		branches:     branches,
		materializer: mat,
		compressor:   compressor,
	}

	ctx := context.Background()
	_, err = s.manifests.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys: bson.D{
				{Key: "branch_id", Value: 1},
				{Key: "collection", Value: 1},
				{Key: "lsn", Value: -1},
			},
		},
		{
			Keys: bson.M{"project_id": 1},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot indexes: %w", err)
	}

	mat.SetSnapshotSource(s)
	return s, nil
}

// CreateSnapshot materializes every collection of the branch at the given
// LSN and stores one snapshot per collection. Because materialization
// itself starts from the previous snapshot when one exists, successive
// snapshots are built incrementally, and unchanged collections re-serialize
// to identical chunks that deduplicate away in the chunk store.
func (s *Service) CreateSnapshot(ctx context.Context, branchID string, lsn int64) ([]*Snapshot, error) {
	branch, err := s.branches.GetBranchByIDAny(branchID)
	if err != nil {
		return nil, fmt.Errorf("branch %s not found: %w", branchID, err)
	}
	if lsn <= branch.BaseLSN || lsn > branch.HeadLSN {
		return nil, fmt.Errorf("snapshot LSN %d outside branch range (%d, %d]", lsn, branch.BaseLSN, branch.HeadLSN)
	}

	state, err := s.materializer.MaterializeBranchAtLSN(branch, lsn)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize branch at LSN %d: %w", lsn, err)
	}

	snapshots := make([]*Snapshot, 0, len(state))
	for collection, docs := range state {
		snap, err := s.storeCollectionSnapshot(ctx, branch, collection, lsn, docs)
		if err != nil {
			return nil, fmt.Errorf("collection %s: %w", collection, err)
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

func (s *Service) storeCollectionSnapshot(ctx context.Context, branch *wal.Branch, collection string, lsn int64, docs map[string]bson.M) (*Snapshot, error) {
	chunks, docCount, err := encodeState(docs, s.compressor)
	if err != nil {
		return nil, err
	}

	chunkIDs := make([]string, 0, len(chunks))
	var sizeBytes int64
	for _, chunk := range chunks {
		id, err := s.store.Put(ctx, chunk)
		if err != nil {
			return nil, err
		}
		chunkIDs = append(chunkIDs, id)
		sizeBytes += int64(len(chunk))
	}

	snap := &Snapshot{
		ProjectID:     branch.ProjectID,
		BranchID:      branch.ID,
		Collection:    collection,
		LSN:           lsn,
		RangesApplied: len(branch.DiscardedRanges),
		ChunkIDs:      chunkIDs,
		DocCount:      docCount,
		SizeBytes:     sizeBytes,
		CreatedAt:     time.Now(),
	}

	if _, err := s.manifests.InsertOne(ctx, snap); err != nil {
		return nil, fmt.Errorf("failed to store snapshot manifest: %w", err)
	}
	return snap, nil
}

// FindUsable implements materializer.SnapshotSource: it returns the loaded
// state of the newest usable snapshot of (branch, collection) whose LSN
// lies in [minLSN, maxLSN], for a read whose segment upper bound is
// readUpperBound.
//
// Usability honors discarded ranges: ranges recorded before the snapshot
// was built (index < RangesApplied) were already excluded from its state;
// a range recorded afterwards invalidates the snapshot iff it overlaps the
// snapshot's history (From <= LSN) and the reader must skip it
// (readUpperBound > To) — the same visibility rule replay uses for
// individual entries.
func (s *Service) FindUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (map[string]bson.M, int64, bool, error) {
	ctx := context.Background()
	cursor, err := s.manifests.Find(ctx,
		bson.M{
			"branch_id":  branch.ID,
			"collection": collection,
			"lsn":        bson.M{"$gte": minLSN, "$lte": maxLSN},
		},
		options.Find().SetSort(bson.M{"lsn": -1}),
	)
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to query snapshots: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	for cursor.Next(ctx) {
		var snap Snapshot
		if err := cursor.Decode(&snap); err != nil {
			return nil, 0, false, fmt.Errorf("failed to decode snapshot manifest: %w", err)
		}
		if !s.usable(&snap, branch, readUpperBound) {
			continue
		}
		state, err := s.load(ctx, &snap)
		if err != nil {
			return nil, 0, false, err
		}
		return state, snap.LSN, true, nil
	}
	return nil, 0, false, cursor.Err()
}

func (s *Service) usable(snap *Snapshot, branch *wal.Branch, readUpperBound int64) bool {
	for idx := snap.RangesApplied; idx < len(branch.DiscardedRanges); idx++ {
		r := branch.DiscardedRanges[idx]
		if r.From <= snap.LSN && readUpperBound > r.To {
			return false
		}
	}
	return true
}

func (s *Service) load(ctx context.Context, snap *Snapshot) (map[string]bson.M, error) {
	state := make(map[string]bson.M, snap.DocCount)
	for _, chunkID := range snap.ChunkIDs {
		data, err := s.store.Get(ctx, chunkID)
		if err != nil {
			return nil, err
		}
		if err := decodeChunk(data, s.compressor, state); err != nil {
			return nil, fmt.Errorf("chunk %s: %w", chunkID, err)
		}
	}
	return state, nil
}

// NewestUsableLSN returns the LSN of the newest snapshot of (branch,
// collection) at or below maxLSN that is usable for a read whose segment
// upper bound is readUpperBound, or 0 if none. GC uses this to compute how
// far a collection's entries are covered: pass math.MaxInt64 as
// readUpperBound to require validity for every possible future reader.
func (s *Service) NewestUsableLSN(branch *wal.Branch, collection string, maxLSN, readUpperBound int64) (int64, error) {
	ctx := context.Background()
	cursor, err := s.manifests.Find(ctx,
		bson.M{
			"branch_id":  branch.ID,
			"collection": collection,
			"lsn":        bson.M{"$lte": maxLSN},
		},
		options.Find().SetSort(bson.M{"lsn": -1}),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to query snapshots: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	for cursor.Next(ctx) {
		var snap Snapshot
		if err := cursor.Decode(&snap); err != nil {
			return 0, fmt.Errorf("failed to decode snapshot manifest: %w", err)
		}
		if s.usable(&snap, branch, readUpperBound) {
			return snap.LSN, nil
		}
	}
	return 0, cursor.Err()
}

// CollectionsUpTo implements materializer.SnapshotSource: collections that
// have a snapshot for this branch within the LSN window.
func (s *Service) CollectionsUpTo(branchID string, minLSN, maxLSN int64) ([]string, error) {
	ctx := context.Background()
	values, err := s.manifests.Distinct(ctx, "collection", bson.M{
		"branch_id": branchID,
		"lsn":       bson.M{"$gte": minLSN, "$lte": maxLSN},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshot collections: %w", err)
	}
	collections := make([]string, 0, len(values))
	for _, v := range values {
		if name, ok := v.(string); ok {
			collections = append(collections, name)
		}
	}
	return collections, nil
}

// ListSnapshots returns the manifests for a branch, newest first.
func (s *Service) ListSnapshots(ctx context.Context, branchID string) ([]*Snapshot, error) {
	cursor, err := s.manifests.Find(ctx,
		bson.M{"branch_id": branchID},
		options.Find().SetSort(bson.M{"lsn": -1}),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var snaps []*Snapshot
	if err := cursor.All(ctx, &snaps); err != nil {
		return nil, err
	}
	return snaps, nil
}
