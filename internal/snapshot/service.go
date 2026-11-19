package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

	oldKeyMu     sync.Mutex
	oldKeyChecks *[256]oldKeyCheck

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

// A manifest and its covered WAL prefix are immutable. Remember a bounded
// number of old manifests so repeated coverage checks do not reread chunks.
// The low byte of the ObjectID counter distributes adjacent manifests.
type oldKeyCheck struct {
	id      primitive.ObjectID
	trusted bool
}

func (s *Service) oldKeysTrusted(id primitive.ObjectID) (bool, bool) {
	s.oldKeyMu.Lock()
	defer s.oldKeyMu.Unlock()
	if s.oldKeyChecks == nil {
		return false, false
	}
	check := s.oldKeyChecks[id[len(id)-1]]
	return check.trusted, !id.IsZero() && check.id == id
}

func (s *Service) rememberOldKeys(id primitive.ObjectID, trusted bool) {
	s.oldKeyMu.Lock()
	defer s.oldKeyMu.Unlock()
	if s.oldKeyChecks == nil {
		s.oldKeyChecks = new([256]oldKeyCheck)
	}
	s.oldKeyChecks[id[len(id)-1]] = oldKeyCheck{id: id, trusted: trusted}
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
	// Equality to null matches an absent field. Current and certified
	// manifests have no entries in this index.
	unchecked := bson.D{{Key: "exact_old_binary_keys", Value: nil}, {Key: "old_keys_trusted", Value: nil}}
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
		{
			Keys:    bson.D{{Key: "old_keys_trusted", Value: 1}},
			Options: options.Index().SetPartialFilterExpression(unchecked),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot indexes: %w", err)
	}

	// Certification is optional. Limit startup work when a historical
	// chunk store is offline or a large upgrade needs more than one pass.
	certifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	s.certifyOldSnapshots(certifyCtx, unchecked)
	cancel()
	mat.SetSnapshotSource(s)
	return s, nil
}

// certifyOldSnapshots inspects only the unclassified portion of the catalog.
// Failures leave a manifest pending, so ordinary selection can still check
// it on demand or a later startup can finish the upgrade.
func (s *Service) certifyOldSnapshots(ctx context.Context, unchecked bson.D) {
	cursor, err := s.manifests.Find(ctx, unchecked, options.Find().SetHint(bson.D{{Key: "old_keys_trusted", Value: 1}}))
	if err != nil {
		return
	}
	defer func() { _ = cursor.Close(ctx) }()
	for ctx.Err() == nil && cursor.Next(ctx) {
		var snap Snapshot
		if cursor.Decode(&snap) != nil {
			continue
		}
		branch, err := s.branches.GetBranchByIDAny(snap.BranchID)
		if err != nil {
			continue
		}
		_, trusted, err := s.readCandidate(ctx, branch, &snap, false)
		if err != nil {
			continue
		}
		// Concurrent initializers can certify the same immutable manifest.
		// Leave any result that another initializer has already persisted.
		_, err = s.manifests.UpdateOne(ctx, bson.M{"_id": snap.ID, "old_keys_trusted": nil}, bson.M{"$set": bson.M{"old_keys_trusted": trusted}})
		if err != nil {
			return
		}
	}
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
		ProjectID:          branch.ProjectID,
		BranchID:           branch.ID,
		Collection:         collection,
		LSN:                lsn,
		RangesApplied:      len(branch.DiscardedRanges),
		VisibilityRules:    CurrentVisibilityRules,
		ExactOldBinaryKeys: true,
		ChunkIDs:           chunkIDs,
		DocCount:           docCount,
		SizeBytes:          sizeBytes,
		CreatedAt:          time.Now(),
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
	return s.newestUsable(branch, collection, minLSN, maxLSN, readUpperBound, true, true)
}

// UsableLSN implements materializer.SnapshotSource: the LSN FindUsable
// would start from. Current manifests need no chunk reads.
func (s *Service) UsableLSN(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (int64, bool, error) {
	_, lsn, ok, err := s.newestUsable(branch, collection, minLSN, maxLSN, readUpperBound, false, false)
	return lsn, ok, err
}

// CheckReplayCoverage checks whether an old-key snapshot can be bypassed
// during resync. Certified manifests need no document or chunk reads.
func (s *Service) CheckReplayCoverage(branch *wal.Branch, collection string, from, to int64) error {
	_, _, _, err := s.newestUsable(branch, collection, from, to, to, false, true)
	return err
}

// UnrecordedReclaimBound implements materializer.SnapshotSource. Garbage
// collection in releases up to 2.1.5 deleted entries without recording a
// reclaim floor. It could delete only what a snapshot covered, and the
// manifests those releases wrote are the ones without VisibilityRules, so
// the newest of them bounds what such a run can have deleted: nothing above
// its LSN. Zero means no such manifest exists. An empty collection name
// asks about every collection of the branch.
func (s *Service) UnrecordedReclaimBound(branchID, collection string) (int64, error) {
	var snap struct {
		LSN int64 `bson:"lsn"`
	}
	filter := bson.M{
		"branch_id":        branchID,
		"visibility_rules": bson.M{"$not": bson.M{"$gte": CurrentVisibilityRules}},
	}
	if collection != "" {
		filter["collection"] = collection
	}
	err := s.manifests.FindOne(context.Background(),
		filter,
		options.FindOne().SetSort(bson.M{"lsn": -1}).SetProjection(bson.M{"lsn": 1}),
	).Decode(&snap)
	if err == mongo.ErrNoDocuments {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to query snapshots: %w", err)
	}
	return snap.LSN, nil
}

// newestUsable checks both reads and GC coverage against the same rules.
func (s *Service) newestUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64, wantState, checkReplay bool) (map[string]bson.M, int64, bool, error) {
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

	var selected map[string]bson.M
	var selectedLSN, rejectedLSN, unrecordedLSN int64
	var found bool
	for cursor.Next(ctx) {
		var snap Snapshot
		if err := cursor.Decode(&snap); err != nil {
			return nil, 0, false, fmt.Errorf("failed to decode snapshot manifest: %w", err)
		}
		if !s.usable(&snap, branch, readUpperBound) {
			continue
		}
		state, usable, err := s.readCandidate(ctx, branch, &snap, wantState)
		if err != nil {
			return nil, 0, false, err
		}
		if usable {
			selected, selectedLSN, found = state, snap.LSN, true
			break
		}
		if checkReplay {
			rejectedLSN = max(rejectedLSN, snap.LSN)
			if snap.VisibilityRules < CurrentVisibilityRules {
				unrecordedLSN = max(unrecordedLSN, snap.LSN)
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, 0, false, err
	}
	if rejectedLSN != 0 {
		from := minLSN
		if found {
			from = selectedLSN + 1
		}
		if err := s.materializer.CheckRejectedSnapshotCoverage(branch, collection, from, readUpperBound, rejectedLSN, unrecordedLSN); err != nil {
			return nil, 0, false, err
		}
	}
	return selected, selectedLSN, found, nil
}

func (s *Service) readCandidate(ctx context.Context, branch *wal.Branch, snap *Snapshot, wantState bool) (map[string]bson.M, bool, error) {
	trusted, checked := snap.ExactOldBinaryKeys, snap.ExactOldBinaryKeys
	if !checked {
		if snap.OldKeysTrusted != nil {
			trusted, checked = *snap.OldKeysTrusted, true
		} else {
			trusted, checked = s.oldKeysTrusted(snap.ID)
		}
	}
	if checked {
		if !trusted || !wantState {
			return nil, trusted, nil
		}
	} else {
		// The shared delete could have erased the last row. Even an empty
		// snapshot cannot cover that history.
		err := s.materializer.CheckOldBinarySnapshot(branch, snap.Collection, snap.LSN)
		var ambiguous *wal.AmbiguousOldBinaryError
		if errors.As(err, &ambiguous) || errors.Is(err, wal.ErrOldBinarySnapshotReclaimed) {
			s.rememberOldKeys(snap.ID, false)
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	state, err := s.load(ctx, snap, wantState, trusted)
	if errors.Is(err, errSharedSnapshotKey) {
		s.rememberOldKeys(snap.ID, false)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !checked {
		s.rememberOldKeys(snap.ID, true)
	}
	return state, true, nil
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
	if !snap.ExactOldBinaryKeys {
		// Old processes cannot apply a collection resync baseline, even
		// if its checkpoint was published just after the manifest was built.
		if start := branch.CollectionStart(snap.Collection, readUpperBound); start != 0 && snap.LSN >= start {
			return false
		}
	}
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
func (s *Service) load(ctx context.Context, snap *Snapshot, wantState, trusted bool) (map[string]bson.M, error) {
	if len(snap.ChunkIDs) == 0 {
		return make(map[string]bson.M), nil
	}
	loadChunk := func(id string, capacity int64) (map[string]bson.M, error) {
		data, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		var state map[string]bson.M
		if wantState {
			state = make(map[string]bson.M, capacity)
		}
		if err := decodeChunk(data, s.compressor, state, trusted); err != nil {
			return nil, fmt.Errorf("chunk %s: %w", id, err)
		}
		return state, nil
	}
	if len(snap.ChunkIDs) == 1 {
		return loadChunk(snap.ChunkIDs[0], snap.DocCount)
	}

	partials := make([]map[string]bson.M, len(snap.ChunkIDs))
	errs := make([]error, len(snap.ChunkIDs))
	var wg sync.WaitGroup
	for i, chunkID := range snap.ChunkIDs {
		wg.Add(1)
		go func(i int, chunkID string) {
			defer wg.Done()
			partials[i], errs[i] = loadChunk(chunkID, 0)
		}(i, chunkID)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if !wantState {
		return nil, nil
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
	lsn, _, err := s.UsableLSN(branch, collection, 0, maxLSN, readUpperBound)
	return lsn, err
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
