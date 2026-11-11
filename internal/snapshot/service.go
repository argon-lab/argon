package snapshot

import (
	"context"
	"fmt"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Service creates and serves collection snapshots.
type Service struct {
	manifests        *mongo.Collection
	publicationLocks *mongo.Collection
	chunks           *mongo.Collection // raw handle for GC reference checks
	store            ChunkStore
	branches         *branchwal.BranchService
	materializer     *materializer.Service
	compressor       *wal.Compressor

	// Auto-snapshot state (see auto.go).
	autoMu       sync.Mutex
	autoCfg      *AutoConfig
	autoBranches map[string]*autoState
	autoStopping bool
	autoRunning  int
	autoIdle     chan struct{}
	autoContext  context.Context
	autoCancel   context.CancelFunc
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
		manifests:        db.Collection("wal_snapshots"),
		publicationLocks: db.Collection("wal_snapshot_locks"),
		chunks:           db.Collection("wal_snapshot_chunks"),
		store:            store,
		branches:         branches,
		materializer:     mat,
		compressor:       compressor,
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
	if branch.IsDeleted {
		return nil, fmt.Errorf("cannot create a snapshot for a deleted branch")
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
	unlock, err := s.lockPublication(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	fresh, err := s.branches.GetBranchByIDAny(branch.ID)
	if err != nil {
		return nil, err
	}
	if fresh.IsDeleted {
		return nil, fmt.Errorf("branch was deleted while building its snapshot")
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
		ProjectID:       branch.ProjectID,
		BranchID:        branch.ID,
		Collection:      collection,
		LSN:             lsn,
		RangesApplied:   len(branch.DiscardedRanges),
		VisibilityRules: CurrentVisibilityRules,
		ChunkIDs:        chunkIDs,
		DocCount:        docCount,
		SizeBytes:       sizeBytes,
		CreatedAt:       time.Now(),
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
// Usability honors discarded ranges with the same visibility rule replay
// applies to individual entries; see usable.
func (s *Service) FindUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (map[string]bson.M, int64, bool, error) {
	ctx := context.Background()
	snap, err := s.newestUsable(ctx, branch, collection, minLSN, maxLSN, readUpperBound)
	if err != nil || snap == nil {
		return nil, 0, false, err
	}
	state, err := s.load(ctx, snap)
	if err != nil {
		return nil, 0, false, err
	}
	return state, snap.LSN, true, nil
}

// UsableLSN implements materializer.SnapshotSource: the LSN FindUsable
// would start from, decided from manifests alone without loading chunks.
func (s *Service) UsableLSN(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (int64, bool, error) {
	snap, err := s.newestUsable(context.Background(), branch, collection, minLSN, maxLSN, readUpperBound)
	if err != nil || snap == nil {
		return 0, false, err
	}
	return snap.LSN, true, nil
}

// newestUsable returns the manifest of the newest snapshot of (branch,
// collection) with an LSN in [minLSN, maxLSN] that is usable for a read at
// readUpperBound, or nil.
func (s *Service) newestUsable(ctx context.Context, branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (*Snapshot, error) {
	cursor, err := s.manifests.Find(ctx,
		bson.M{
			"branch_id":  branch.ID,
			"collection": collection,
			"lsn":        bson.M{"$gte": minLSN, "$lte": maxLSN},
		},
		options.Find().SetSort(bson.M{"lsn": -1}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query snapshots: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	for cursor.Next(ctx) {
		var snap Snapshot
		if err := cursor.Decode(&snap); err != nil {
			return nil, fmt.Errorf("failed to decode snapshot manifest: %w", err)
		}
		if s.usable(&snap, branch, readUpperBound) {
			return &snap, nil
		}
	}
	return nil, cursor.Err()
}

// usable reports whether a snapshot holds exactly the history a reader with
// the given upper bound must see up to the snapshot's LSN.
//
// A snapshot at LSN S is itself a read with upper bound S, so it excluded a
// discarded range only if the range was already recorded when it was built
// and ended below S. A reader at bound U excludes a range iff U lies above
// its end. The two agree on every range except one that overlaps the
// snapshot's history (From <= S), that the build did not exclude, and that
// the reader must skip (U > To). That covers both ways a snapshot can hold
// discarded writes:
//
//   - the reset happened after the snapshot was built;
//   - the snapshot was taken at an LSN inside an already discarded window
//     (it captured the pre-reset history, like a pin at that position).
//
// In either case the snapshot still serves readers at or below the range's
// end — children, pins and historical reads that keep the pre-reset view.
func (s *Service) usable(snap *Snapshot, branch *wal.Branch, readUpperBound int64) bool {
	effective := branch.EffectiveDiscardedRanges()
	for idx, r := range effective {
		if r.From > snap.LSN {
			continue // Entirely above the snapshot: nothing of it is inside.
		}
		if idx < snap.RangesApplied {
			if snap.LSN > r.To {
				continue // Excluded by the build, and by every reader at or above S.
			}
			// Releases up to 2.1.5 ended a range at the head of the moment,
			// which after back-to-back resets is below the true end. A
			// manifest from then that sits between the two ends excluded
			// history its readers are entitled to see: unusable for anyone.
			if snap.VisibilityRules < CurrentVisibilityRules && snap.LSN > branch.DiscardedRanges[idx].To {
				return false
			}
		}
		if readUpperBound > r.To {
			return false
		}
	}
	return true
}

// load fetches and decodes a snapshot's chunks in parallel. Chunks hold
// disjoint, contiguous key ranges (the encoder cuts a sorted stream), so
// each decodes into a private map and the merge is a plain union.
// Decompression and per-document BSON decoding dominate snapshot reads —
// benchmark feedback measured them at ~70% of a warm read — and they
// parallelize embarrassingly.
func (s *Service) load(ctx context.Context, snap *Snapshot) (map[string]bson.M, error) {
	if len(snap.ChunkIDs) == 0 {
		return make(map[string]bson.M), nil
	}
	if len(snap.ChunkIDs) == 1 {
		state := make(map[string]bson.M, snap.DocCount)
		data, err := s.store.Get(ctx, snap.ChunkIDs[0])
		if err != nil {
			return nil, err
		}
		if err := decodeChunk(data, s.compressor, state); err != nil {
			return nil, fmt.Errorf("chunk %s: %w", snap.ChunkIDs[0], err)
		}
		return state, nil
	}

	partials := make([]map[string]bson.M, len(snap.ChunkIDs))
	errs := make([]error, len(snap.ChunkIDs))
	var wg sync.WaitGroup
	for i, chunkID := range snap.ChunkIDs {
		wg.Add(1)
		go func(i int, chunkID string) {
			defer wg.Done()
			data, err := s.store.Get(ctx, chunkID)
			if err != nil {
				errs[i] = err
				return
			}
			partial := make(map[string]bson.M)
			if err := decodeChunk(data, s.compressor, partial); err != nil {
				errs[i] = fmt.Errorf("chunk %s: %w", chunkID, err)
				return
			}
			partials[i] = partial
		}(i, chunkID)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	state := make(map[string]bson.M, snap.DocCount)
	for _, partial := range partials {
		for k, v := range partial {
			state[k] = v
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
	snap, err := s.newestUsable(context.Background(), branch, collection, 0, maxLSN, readUpperBound)
	if err != nil || snap == nil {
		return 0, err
	}
	return snap.LSN, nil
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
