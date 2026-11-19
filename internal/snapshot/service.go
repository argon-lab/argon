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
	certifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
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

	// Certification is optional. Limit startup work when a historical
	// chunk store is offline or a large upgrade needs more than one pass.
	s.CertifyOldSnapshots(certifyCtx)
	mat.SetSnapshotSource(s)
	return s, nil
}

// CreateSnapshot materializes every collection of the branch at the given
// LSN and stores one snapshot per collection. Because materialization
// itself starts from the previous snapshot when one exists, successive
// snapshots are built incrementally, and unchanged collections re-serialize
// to identical chunks that deduplicate away in the chunk store.
func (s *Service) CreateSnapshot(ctx context.Context, branchID string, lsn int64) (snapshots []*Snapshot, err error) {
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

	collections, err := s.materializer.CollectionsAtLSN(branch, lsn)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize branch at LSN %d: %w", lsn, err)
	}

	// Materialize one collection at a time. Keep its compressed chunks until
	// the whole branch succeeds, so replay runs outside the publication lock.
	var staged []encodedCollection
	for _, name := range collections {
		docs, err := s.materializer.EditCollectionAtLSN(branch, name, lsn)
		if err != nil {
			return nil, err
		}
		chunks, count, err := encodeState(docs, s.compressor)
		if err != nil {
			return nil, fmt.Errorf("collection %s: %w", name, err)
		}
		staged = append(staged, encodedCollection{name: name, chunks: chunks, docCount: count})
	}
	snapshots = make([]*Snapshot, 0, len(staged))
	if len(staged) == 0 {
		return snapshots, nil
	}

	unlock, err := s.lockPublication(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	unpublished := make(map[string]bool)
	defer func() {
		if err != nil && len(unpublished) != 0 {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, cleanupErr := s.deleteUnreferencedChunks(cleanupCtx, unpublished)
			err = errors.Join(err, cleanupErr)
		}
	}()
	for i, collection := range staged {
		snap, err := s.storeCollectionChunks(ctx, branch, lsn, collection, unpublished)
		if err != nil {
			return nil, fmt.Errorf("collection %s: %w", collection.name, err)
		}
		staged[i].chunks = nil
		snapshots = append(snapshots, snap)
	}
	fresh, err := s.branches.GetBranchByIDAny(branch.ID)
	if err != nil {
		return nil, err
	}
	if fresh.IsDeleted {
		return nil, fmt.Errorf("branch was deleted while building its snapshot")
	}

	for _, snap := range snapshots {
		// An insert can succeed even if its acknowledgement is lost.
		// Only clean up chunks whose manifests were never attempted.
		for _, id := range snap.ChunkIDs {
			delete(unpublished, id)
		}
		if _, err := s.manifests.InsertOne(ctx, snap); err != nil {
			return nil, fmt.Errorf("failed to store snapshot manifest: %w", err)
		}
	}
	return snapshots, nil
}

type encodedCollection struct {
	name     string
	chunks   [][]byte
	docCount int64
}

func (s *Service) storeCollectionChunks(ctx context.Context, branch *wal.Branch, lsn int64, collection encodedCollection, unpublished map[string]bool) (*Snapshot, error) {
	chunkIDs := make([]string, 0, len(collection.chunks))
	var sizeBytes int64
	for _, chunk := range collection.chunks {
		id, err := s.store.Put(ctx, chunk)
		if err != nil {
			return nil, err
		}
		unpublished[id] = true
		chunkIDs = append(chunkIDs, id)
		sizeBytes += int64(len(chunk))
	}

	return &Snapshot{
		ProjectID:          branch.ProjectID,
		BranchID:           branch.ID,
		Collection:         collection.name,
		LSN:                lsn,
		RangesApplied:      len(branch.DiscardedRanges),
		VisibilityRules:    CurrentVisibilityRules,
		ExactOldBinaryKeys: true,
		OrderedImages:      true,
		ChunkIDs:           chunkIDs,
		DocCount:           collection.docCount,
		SizeBytes:          sizeBytes,
		CreatedAt:          time.Now(),
	}, nil
}

// FindUsable implements materializer.SnapshotSource: it returns the loaded
// state of the newest usable snapshot of (branch, collection) whose LSN
// lies in [minLSN, maxLSN], for a read whose segment upper bound is
// readUpperBound.
//
// Usability honors discarded ranges with the same visibility rule replay
// applies to individual entries; see usable.
func (s *Service) FindUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (map[string]bson.M, int64, bool, error) {
	var state map[string]bson.M
	snap, err := s.loadUsable(branch, collection, minLSN, maxLSN, readUpperBound, func(snap *Snapshot) (retry bool, err error) {
		state, err = s.load(context.Background(), snap)
		return true, err
	})
	if err != nil || snap == nil {
		return nil, 0, false, err
	}
	return state, snap.LSN, true, nil
}

// VisitUsableFrames lends each decoded snapshot value and its stored BSON
// together. A failed chunk can follow frames already visited, so reset
// discards that candidate's partial state before starting another one.
func (s *Service) VisitUsableFrames(branch *wal.Branch, collection string, from, to int64, reset func(), visit func(string, *wal.DecodedImage) error) (int64, bool, bool, error) {
	snap, err := s.loadUsable(branch, collection, from, to, to, func(snap *Snapshot) (bool, error) {
		reset()
		var visitErr error
		err := s.visitFrames(snap, func(f frame) error {
			key, doc, err := f.decoded()
			if err != nil {
				return err
			}
			visitErr = visit(key, doc)
			return visitErr
		})
		return visitErr == nil, err
	})
	if err != nil || snap == nil {
		return 0, false, false, err
	}
	return snap.LSN, true, bool(snap.OrderedImages), nil
}

// A failed chunk can only be replaced by a copy of the same snapshot position.
func (s *Service) loadUsable(branch *wal.Branch, collection string, from, to, upper int64, load func(*Snapshot) (retry bool, err error)) (*Snapshot, error) {
	var failed []primitive.ObjectID
	var firstErr error
	for {
		snap, err := s.newestUsable(branch, collection, from, to, upper, forReplay, failed...)
		if err != nil || snap == nil {
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, err
		}
		retry, err := load(snap)
		if err == nil {
			return snap, nil
		}
		if !retry {
			return nil, err
		}
		if firstErr == nil {
			firstErr = err
		}
		from, to = snap.LSN, snap.LSN
		failed = append(failed, snap.ID)
	}
}

// OrderedSnapshotFrames selects the latest usable ordered snapshot in the LSN
// interval. A nil walker means that none exists. The walker reads chunks on
// demand and lends each raw document to the callback until it returns.
func (s *Service) OrderedSnapshotFrames(branch *wal.Branch, collection string, from, to, readUpperBound int64) (int64, func(func(string, bson.Raw) error) error, error) {
	snap, err := s.newestUsable(branch, collection, from, to, readUpperBound, forOrderedFrames)
	if err != nil || snap == nil {
		return 0, nil, err
	}
	return snap.LSN, func(visit func(string, bson.Raw) error) error {
		return s.visitFrames(snap, func(f frame) error {
			id, err := wal.DocumentIDFromImage(f.Doc)
			if err != nil {
				return nil // A frame without its own identity cannot supply order.
			}
			return visit(wal.DocumentIDString(id), f.Doc)
		})
	}, nil
}

func (s *Service) visitFrames(snap *Snapshot, visit func(frame) error) error {
	ctx := context.Background()
	for _, chunkID := range snap.ChunkIDs {
		data, err := s.store.Get(ctx, chunkID)
		if err != nil {
			return err
		}
		err = walkChunk(ctx, data, s.compressor, visit)
		if err != nil {
			return fmt.Errorf("chunk %s: %w", chunkID, err)
		}
	}
	return nil
}

// UsableLSN implements materializer.SnapshotSource: the LSN FindUsable
// would start from. Current manifests need no chunk reads.
func (s *Service) UsableLSN(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64) (int64, bool, error) {
	snap, err := s.newestUsable(branch, collection, minLSN, maxLSN, readUpperBound, forCoverage)
	if err != nil || snap == nil {
		return 0, false, err
	}
	return snap.LSN, true, nil
}

// CheckReplayCoverage checks whether an old-key snapshot can be bypassed
// during resync. Certified manifests need no document or chunk reads.
func (s *Service) CheckReplayCoverage(branch *wal.Branch, collection string, from, to int64) error {
	_, err := s.newestUsable(branch, collection, from, to, to, forReplay)
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

type snapshotLookup int

const (
	forCoverage snapshotLookup = iota
	forReplay
	forOrderedFrames
)

// newestUsable checks both reads and GC coverage against the same rules.
func (s *Service) newestUsable(branch *wal.Branch, collection string, minLSN, maxLSN, readUpperBound int64, lookup snapshotLookup, failed ...primitive.ObjectID) (*Snapshot, error) {
	ctx := context.Background()
	filter := bson.M{
		"branch_id":  branch.ID,
		"collection": collection,
		"lsn":        bson.M{"$gte": minLSN, "$lte": maxLSN},
	}
	if len(failed) != 0 {
		filter["_id"] = bson.M{"$nin": failed}
	}
	if lookup == forOrderedFrames {
		filter["ordered_images"] = true
	}
	cursor, err := s.manifests.Find(ctx, filter, options.Find().SetSort(bson.M{"lsn": -1}))
	if err != nil {
		return nil, fmt.Errorf("failed to query snapshots: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var selected *Snapshot
	var tieLSN int64
	var candidateErr error
	var rejectedLSN, unrecordedLSN int64
	var ancestorGap *wal.ReclaimedOldBinaryError
	var pass *certificationPass
	for cursor.Next(ctx) {
		// Only a tie can replace an unreadable manifest or improve a
		// usable sorted one. Inspect the index key and marker first.
		var lsn int64
		if err := cursor.Current.Lookup("lsn").Unmarshal(&lsn); err != nil {
			if selected != nil || candidateErr != nil {
				break
			}
			return nil, fmt.Errorf("failed to decode snapshot manifest LSN: %w", err)
		}
		if (selected != nil || candidateErr != nil) && lsn != tieLSN {
			break
		}
		tieLSN = lsn
		if selected != nil || lookup == forOrderedFrames {
			if ordered, _ := cursor.Current.Lookup("ordered_images").BooleanOK(); !ordered {
				continue
			}
		}
		var snap Snapshot
		if err := cursor.Decode(&snap); err != nil {
			if candidateErr == nil {
				candidateErr = fmt.Errorf("failed to decode snapshot manifest: %w", err)
			}
			continue
		}
		if !s.usable(&snap, branch, readUpperBound) {
			continue
		}
		if !snap.ExactOldBinaryKeys && snap.OldKeyCheck == "" {
			if pass == nil {
				pass = s.newCertificationPass()
			}
			if err := pass.certify(ctx, branch, &snap); err != nil {
				if _, ok := errors.AsType[candidateChunkError](err); !ok {
					return nil, err
				}
				if candidateErr == nil {
					candidateErr = err
				}
				continue
			}
		}
		usable := snap.ExactOldBinaryKeys || snap.OldKeyCheck == keysTrusted
		if usable {
			selected = &snap
			if snap.OrderedImages {
				break
			}
			continue
		}
		if lookup != forCoverage {
			rejectedLSN = max(rejectedLSN, snap.LSN)
			if gap := snap.OldKeyGap; ancestorGap == nil && gap != nil && gap.BranchID != branch.ID {
				ancestorGap = gap
			}
			if snap.VisibilityRules < CurrentVisibilityRules {
				unrecordedLSN = max(unrecordedLSN, snap.LSN)
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	if selected == nil && candidateErr != nil {
		return nil, candidateErr
	}
	if rejectedLSN != 0 {
		from := minLSN
		if selected != nil {
			from = selected.LSN + 1
		}
		if err := s.materializer.CheckRejectedSnapshotCoverage(branch, collection, from, readUpperBound, rejectedLSN, unrecordedLSN); err != nil {
			return nil, err
		}
	}
	if selected == nil && ancestorGap != nil {
		if err := s.materializer.CheckAncestorSnapshotCoverage(branch, collection, readUpperBound, ancestorGap); err != nil {
			return nil, err
		}
	}
	return selected, nil
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
func (s *Service) load(ctx context.Context, snap *Snapshot) (map[string]bson.M, error) {
	if len(snap.ChunkIDs) == 0 {
		return make(map[string]bson.M), nil
	}
	loadChunk := func(id string, capacity int64) (map[string]bson.M, error) {
		data, err := s.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		state := make(map[string]bson.M, capacity)
		if err := decodeChunk(ctx, data, s.compressor, state); err != nil {
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
