package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Resynchronising a followed branch.
//
// A follower that has ended as degraded holds a branch that equals the source
// at some past cluster time and a stream position it cannot continue from: a
// collection was dropped or renamed (the event carries no documents), or the
// source's oplog no longer reaches back to the position. Resync brings that
// branch to the source's current state in place, without a second project,
// and leaves it followable again.
//
// It is the online import's argument applied to a branch that already has
// documents. A stream boundary is taken first. Then the branch is compared
// with the source, in two passes:
//
//   - Source to branch: every source document is read and stored as a put
//     unless the branch's newest entry for it is a put of the same bytes.
//   - Branch to source: every document the branch holds is looked up on the
//     source by _id, and deleted from the branch if the source has none. A
//     collection that was dropped on the source has none of its documents
//     there, so all of them are deleted; a renamed one is that plus the first
//     pass's puts under the new name.
//
// Neither pass is a state of the source: the documents are read at different
// moments while the source is written. As in the import, the change stream is
// then replayed from the boundary until it has passed every read, and the
// branch at the end is the source at one cluster time. A change made during
// the passes is in what a pass read, in the replayed stream, or in both, and
// entries are whole documents, so applying it twice is harmless. That covers
// the second pass too: a document found missing and then inserted is put
// back by its insert event, which is stored after the delete.
//
// Both passes work in batches. The first follows a cursor over the source;
// the second follows the WAL's own (branch, collection, document) index, so
// no collection is held in memory, with one exception: where garbage
// collection has reclaimed part of a collection's history the branch's
// documents can only be listed from its snapshot, which is loaded whole, as
// every other reader of reclaimed history does.
//
// The branch is visible while this runs, so each batch moves its head. What
// keeps a half-compared branch from passing for a healthy one is its status,
// written before the first entry: mode catch_up, the LSN the inexact range
// starts at, and a resync marker. The follower refuses to start while the
// marker is set. A resync that is interrupted is simply run again: it takes
// a new boundary and compares whatever the branch holds by then. The marker
// is cleared, and the range recorded as inexact, in the transaction that
// stores the first head proven equal to the source.

// ResyncState marks a resync that has written to the branch and has not yet
// brought it to a state of the source.
type ResyncState struct {
	StartedAt time.Time `bson:"started_at" json:"started_at"`
	// Cause is the stored reason following had ended, already redacted.
	Cause string `bson:"cause,omitempty" json:"cause,omitempty"`
}

// BranchState reads a branch's documents. The materializer implements it.
type BranchState interface {
	CollectionsAtLSN(branch *wal.Branch, targetLSN int64) ([]string, error)
	MaterializeDocumentsAtLSN(branch *wal.Branch, collection string, keys []string, targetLSN int64) (map[string]bson.M, error)
	MaterializeCollectionAtLSN(branch *wal.Branch, collection string, targetLSN int64) (map[string]bson.M, error)
}

// ResyncProgress is reported after every batch of a resync's two passes.
type ResyncProgress struct {
	// Phase is "compare" (source to branch) or "prune" (branch to source).
	Phase      string `json:"phase"`
	Collection string `json:"collection"`
	Compared   int64  `json:"compared"`
	Puts       int64  `json:"puts"`
	Deletes    int64  `json:"deletes"`
}

// ResyncOptions configures Resync.
type ResyncOptions struct {
	// State reads the branch. Required.
	State BranchState
	// Force resyncs a branch whose follower did not end as degraded.
	Force bool
	// BatchSize is the number of documents per batch (default 500).
	BatchSize int
	// Progress is optional.
	Progress func(ResyncProgress)
	// WriteBacks and Redact are as in FollowOptions.
	WriteBacks WriteBackLedger
	Redact     func(string) string
	// Watch replaces src.Watch for the check that the source's change stream
	// can be opened, made before anything is written. Tests use it to supply
	// a refusal that a deployment without access control cannot produce.
	Watch func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error)
}

// ResyncResult describes a finished resync.
type ResyncResult struct {
	// FromLSN is the first LSN of the inexact range the resync closed, and
	// Point its end: the branch at Point.LSN is the source at
	// Point.ClusterTime.
	FromLSN int64            `json:"from_lsn"`
	Point   *ConsistentPoint `json:"consistent"`
	// Compared counts source documents read, Unchanged those the branch
	// already held byte for byte.
	Compared  int64 `json:"compared"`
	Unchanged int64 `json:"unchanged"`
	Puts      int64 `json:"puts"`
	Deletes   int64 `json:"deletes"`
	// SourceCollections are the collections the source had when it was
	// listed; RemovedCollections those the branch held documents of and the
	// source no longer had.
	SourceCollections  []string `json:"source_collections"`
	RemovedCollections []string `json:"removed_collections"`
}

// Resync brings a followed branch to the source's current state and leaves
// it ready to be followed from there. It writes nothing to the source.
func (s *Service) Resync(ctx context.Context, branchID string, src *mongo.Database, opts ResyncOptions) (*ResyncResult, error) {
	if opts.State == nil {
		return nil, errors.New("resync needs a reader for the branch's documents")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	stored, err := s.SourceStatusFor(ctx, branchID)
	if err != nil {
		return nil, err
	}
	unfinished := stored != nil && stored.Source != nil && stored.Source.Resync != nil
	degraded := stored != nil && stored.State == "degraded"
	if !degraded && !unfinished && !opts.Force {
		return nil, &ConfigurationError{"following has not ended as degraded, so there is nothing a resync has to repair; `argon source follow` continues from the saved position. A resync compares the whole source with the branch and records the range it writes as inexact history; pass --force to run one anyway"}
	}
	cause := ""
	if degraded {
		cause = stored.Error
	}

	cfg := runConfig{actor: sourceActor, redact: opts.Redact, follower: true}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return nil, fmt.Errorf("the source is being followed for branch %s in this process; stop that first", branchID)
	}
	result := &ResyncResult{}
	cfg.stream = func(ctx context.Context, run *running) error {
		return s.resync(ctx, branchID, src, opts, run, cause, result)
	}
	// No retries: every attempt compares the whole source again, and whether
	// that is worth repeating is the operator's call.
	if err := s.execute(runCtx, branchID, cfg, run, false); err != nil {
		return nil, redacted(err, opts.Redact)
	}
	if result.Point == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("the resync stopped before the branch reached a consistent point; run it again")
	}
	return result, nil
}

func (s *Service) resync(ctx context.Context, branchID string, src *mongo.Database, opts ResyncOptions, run *running, cause string, result *ResyncResult) error {
	branch, err := s.branches.GetBranchByIDContext(ctx, branchID)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return &ConfigurationError{fmt.Sprintf("branch %s not found", branchID)}
		}
		return fmt.Errorf("branch %s not found: %w", branchID, err)
	}
	if !branch.IsFollower() {
		return &ConfigurationError{fmt.Sprintf("branch %s does not follow an external database", branch.Name)}
	}
	if branch.Follows != src.Name() {
		return &ConfigurationError{fmt.Sprintf("branch %s follows database %q, not %q", branch.Name, branch.Follows, src.Name())}
	}
	if branch.IsLive() {
		return &ConfigurationError{fmt.Sprintf("branch %s is checked out; a followed branch has no physical database", branch.Name)}
	}
	_, _, status, err := s.loadFollowState(ctx, branchID)
	if err != nil {
		return err
	}

	// Boundary, then the list of collections, then the reads: the order the
	// online import uses, for the same reason.
	boundary, err := primaryTime(ctx, src)
	if err != nil {
		return err
	}
	collections, err := source.Collections(ctx, src.Client().Database(src.Name(), options.Database().SetReadPreference(readpref.Primary())))
	if err != nil {
		return fmt.Errorf("failed to list the source's collections: %w", err)
	}
	result.SourceCollections = collections
	if series, err := source.TimeSeries(ctx, src); err != nil {
		return fmt.Errorf("failed to list the source's collections: %w", err)
	} else if len(series) > 0 {
		return &ConfigurationError{fmt.Sprintf("the source has time-series collection(s) %s; their measurements cannot be followed, so the branch could not be made equal to the source. A source with time-series collections cannot be followed", strings.Join(series, ", "))}
	}

	// The status goes first. From here until the replay ends the branch is
	// reported as catching up, and nothing but a resync may continue it.
	from := branch.HeadLSN + 1
	if status.Mode == ModeCatchUp && status.InexactFromLSN > 0 {
		// An earlier catch-up or resync never finished; the range it opened
		// is still open.
		from = status.InexactFromLSN
	}
	status.Database, status.Mode = src.Name(), ModeCatchUp
	status.InexactFromLSN = from
	status.InexactReason = "resync: the branch was compared with the source while the source was written"
	if status.Resync == nil {
		status.Resync = &ResyncState{StartedAt: time.Now(), Cause: cause}
	}
	// Every read below is causally at or after the boundary, so the stream
	// starts with the cluster time after it.
	startAt := boundary
	if startAt.I++; startAt.I == 0 {
		startAt.T++
	}
	// Open the change stream once before anything is written. The replay
	// only starts after the whole comparison; a user without the changeStream
	// privilege should learn that now, with the branch untouched and no
	// unfinished resync on record.
	if err := probeStream(ctx, src, startAt, opts.Watch); err != nil {
		return err
	}
	// The marker is written in a transaction that also requires the branch
	// to follow this source still. A detach by another process between the
	// check above and this write then makes the write fail, instead of
	// leaving a status document behind for a branch that follows nothing.
	if _, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if err := s.branches.FollowerCheckpoint(sc, branch.ID, branch.Follows, branch.HeadLSN); err != nil {
			return nil, err
		}
		_, err := s.state.UpdateOne(sc, bson.M{"_id": branchID}, bson.M{
			"$set":   bson.M{"start_at": startAt, "actor": sourceActor, "capture_status.source": status},
			"$unset": bson.M{"resume_token": ""},
		}, options.Update().SetUpsert(true))
		return nil, err
	}); err != nil {
		if fenced := followerWriteError(branch, err); fenced != err {
			return fenced
		}
		return fmt.Errorf("failed to record the start of the resync: %w", err)
	}
	result.FromLSN = from
	if err := s.report(branchID, "resyncing", sourceActor, ""); err != nil {
		return err
	}

	session, err := src.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(context.Background())
	readsFrom := boundary
	if err := session.AdvanceOperationTime(&readsFrom); err != nil {
		return err
	}
	r := &resyncer{s: s, branch: branch, src: src, opts: opts, result: result, reads: mongo.NewSessionContext(ctx, session)}
	for _, name := range collections {
		if err := r.compare(ctx, name); err != nil {
			return fmt.Errorf("failed to compare source collection %s with the branch: %w", name, err)
		}
	}
	held, err := opts.State.CollectionsAtLSN(branch, branch.HeadLSN)
	if err != nil {
		return err
	}
	onSource := make(map[string]bool, len(collections))
	for _, name := range collections {
		onSource[name] = true
	}
	for _, name := range held {
		deleted, err := r.prune(ctx, name)
		if err != nil {
			return fmt.Errorf("failed to compare the branch's collection %s with the source: %w", name, err)
		}
		if deleted > 0 && !onSource[name] {
			result.RemovedCollections = append(result.RemovedCollections, name)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	point, err := s.followStream(ctx, branchID, src, FollowOptions{
		AllowInexact: true, WriteBacks: opts.WriteBacks, Redact: opts.Redact, resync: true,
	}, run, true)
	if err != nil {
		return err
	}
	result.Point = point
	return nil
}

// resyncer is the working state of a resync's two passes.
type resyncer struct {
	s      *Service
	branch *wal.Branch
	src    *mongo.Database
	opts   ResyncOptions
	result *ResyncResult
	// reads is a causally consistent session that starts at the boundary.
	reads   mongo.SessionContext
	pending []*wal.Entry
}

func (r *resyncer) collection(name string) *mongo.Collection {
	return r.src.Collection(name, options.Collection().
		SetReadConcern(readconcern.Majority()).SetReadPreference(readpref.Primary()))
}

func (r *resyncer) entry(operation wal.OperationType, collection, key string, post bson.Raw) *wal.Entry {
	return &wal.Entry{
		ProjectID: r.branch.ProjectID, BranchID: r.branch.ID,
		Operation: operation, Collection: collection, DocumentID: key, PostImage: post,
		Actor: sourceActor, Metadata: map[string]interface{}{"capture_operation": "resync"},
	}
}

func (r *resyncer) progress(phase, collection string) {
	if r.opts.Progress != nil {
		r.opts.Progress(ResyncProgress{Phase: phase, Collection: collection,
			Compared: r.result.Compared, Puts: r.result.Puts, Deletes: r.result.Deletes})
	}
}

// compare stores every document of one source collection that the branch
// does not already hold byte for byte.
func (r *resyncer) compare(ctx context.Context, name string) error {
	cursor, err := r.collection(name).Find(r.reads, bson.D{})
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(context.Background()) }()
	type read struct {
		key string
		doc bson.Raw
	}
	batch := make([]read, 0, r.opts.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		keys := make([]string, len(batch))
		for i, item := range batch {
			keys[i] = item.key
		}
		same, err := r.unchanged(name, keys)
		if err != nil {
			return err
		}
		for _, item := range batch {
			if held, ok := same[item.key]; ok && bytes.Equal(held, item.doc) {
				r.result.Unchanged++
				continue
			}
			r.pending = append(r.pending, r.entry(wal.OpPut, name, item.key, item.doc))
			r.result.Puts++
		}
		r.result.Compared += int64(len(batch))
		batch = batch[:0]
		if err := r.commit(); err != nil {
			return err
		}
		r.progress("compare", name)
		return ctx.Err()
	}
	for cursor.Next(r.reads) {
		var id interface{}
		if err := cursor.Current.Lookup("_id").Unmarshal(&id); err != nil || id == nil {
			return fmt.Errorf("a document in %s has no _id", name)
		}
		batch = append(batch, read{key: wal.DocumentIDString(id), doc: append(bson.Raw(nil), cursor.Current...)})
		if len(batch) >= r.opts.BatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return err
	}
	return flush()
}

// unchanged returns, for the keys whose newest entry on the branch is a put,
// that put's document. Only an entry that is certain to be the document's
// newest counts: one at or below the collection's reclaim floor may have had
// a newer one deleted by garbage collection, and a document whose state
// lives only in a snapshot or on an ancestor is not found here at all. Such
// documents are stored again, which costs an entry and changes nothing.
func (r *resyncer) unchanged(collection string, keys []string) (map[string]bson.Raw, error) {
	branch := r.branch
	held := make(map[string]bson.Raw, len(keys))
	entries, err := r.s.wal.LatestDocumentEntries(branch.ID, collection, keys, branch.BaseLSN+1, branch.HeadLSN,
		func(lsn int64) bool { return !branch.IsDiscardedForRead(lsn, branch.HeadLSN) })
	if errors.Is(err, wal.ErrLegacyDocumentKeys) {
		return held, nil
	}
	if err != nil {
		return nil, err
	}
	// The floor is read after the entries: garbage collection records it
	// before it deletes.
	floor, err := r.s.wal.ReclaimFloor(branch.ID, collection)
	if err != nil {
		return nil, err
	}
	for key, entry := range entries {
		if entry.LSN > floor && entry.Operation == wal.OpPut {
			held[key] = entry.PostImage
		}
	}
	return held, nil
}

// prune deletes from the branch every document of one collection that the
// source no longer has, and returns how many that were.
func (r *resyncer) prune(ctx context.Context, name string) (int64, error) {
	branch := r.branch
	var deleted int64
	// A point read can refuse where a replay of the collection still works
	// (entries that name no document, history reclaimed meanwhile).
	replay := false
	// check takes a batch of the branch's document keys.
	check := func(keys []string, live map[string]bson.M) error {
		if live == nil {
			var err error
			if live, err = r.opts.State.MaterializeDocumentsAtLSN(branch, name, keys, branch.HeadLSN); err != nil {
				replay = true
				return err
			}
		}
		ids := make(bson.A, 0, len(keys))
		for _, key := range keys {
			if doc, ok := live[key]; ok {
				ids = append(ids, doc["_id"])
			}
		}
		if len(ids) == 0 {
			return ctx.Err()
		}
		cursor, err := r.collection(name).Find(r.reads, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
			options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return err
		}
		defer func() { _ = cursor.Close(context.Background()) }()
		// Keys are compared, not values: MongoDB matches 1 and 1.0 as the
		// same _id, and the branch keeps them apart.
		found := make(map[string]bool, len(ids))
		for cursor.Next(r.reads) {
			var id interface{}
			if err := cursor.Current.Lookup("_id").Unmarshal(&id); err != nil {
				return err
			}
			found[wal.DocumentIDString(id)] = true
		}
		if err := cursor.Err(); err != nil {
			return err
		}
		for _, key := range keys {
			if _, ok := live[key]; ok && !found[key] {
				r.pending = append(r.pending, r.entry(wal.OpDelete, name, key, nil))
				r.result.Deletes++
				deleted++
			}
		}
		if err := r.commit(); err != nil {
			return err
		}
		r.progress("prune", name)
		return ctx.Err()
	}

	floor, err := r.s.wal.ReclaimFloor(branch.ID, name)
	if err != nil {
		return 0, err
	}
	if floor == 0 && branch.ParentID == "" {
		// The log names every document the branch has ever held here.
		upTo := branch.HeadLSN
		keys := make([]string, 0, r.opts.BatchSize)
		err := r.s.wal.ScanDocumentKeys(ctx, branch.ID, name, branch.BaseLSN+1, upTo, func(key string) error {
			keys = append(keys, key)
			if len(keys) < r.opts.BatchSize {
				return nil
			}
			err := check(keys, nil)
			keys = keys[:0]
			return err
		})
		if err == nil {
			err = check(keys, nil)
		}
		if err == nil || !replay {
			return deleted, err
		}
	}
	state, err := r.opts.State.MaterializeCollectionAtLSN(branch, name, branch.HeadLSN)
	if err != nil {
		return deleted, err
	}
	keys := make([]string, 0, r.opts.BatchSize)
	for key := range state {
		keys = append(keys, key)
		if len(keys) == r.opts.BatchSize {
			if err := check(keys, state); err != nil {
				return deleted, err
			}
			keys = keys[:0]
		}
	}
	return deleted, check(keys, state)
}

// commit stores the pending entries and the branch head in one transaction.
// The status is not touched: it already says the branch is catching up.
func (r *resyncer) commit() error {
	if len(r.pending) == 0 {
		return nil
	}
	s, branch, batch := r.s, r.branch, r.pending
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	at := time.Now()
	var head int64
	if _, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		lsns, err := s.wal.AppendBatchContext(sc, batch)
		if err != nil {
			return nil, fmt.Errorf("failed to append resync entries: %w", err)
		}
		head = lsns[len(lsns)-1]
		if err := s.branches.FollowerSetHead(sc, branch.ID, branch.Follows, branch.HeadLSN, head); err != nil {
			return nil, err
		}
		stored, err := s.state.UpdateOne(sc, bson.M{"_id": branch.ID}, bson.M{"$set": bson.M{
			"updated_at": at, "capture_status.last_captured_at": at, "capture_status.head_lsn": head,
		}})
		if err == nil && stored.MatchedCount != 1 {
			err = branchwal.ErrNoLongerFollows
		}
		return nil, err
	}); err != nil {
		return followerWriteError(branch, err)
	}
	branch.HeadLSN = head
	r.pending = nil
	if s.autoSnapshot != nil {
		for range batch {
			copy := *branch
			s.autoSnapshot.MaybeSnapshot(&copy)
		}
	}
	return nil
}

// probeStream opens and closes the source's change stream at a position.
func probeStream(ctx context.Context, src *mongo.Database, startAt primitive.Timestamp, watch func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error)) error {
	if watch == nil {
		watch = func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		}
	}
	stream, err := watch(ctx, src, options.ChangeStream().SetStartAtOperationTime(&startAt).SetMaxAwaitTime(100*time.Millisecond))
	if err != nil {
		return &ConfigurationError{fmt.Sprintf("the source's change stream cannot be opened, so the changes made during the comparison could not be replayed (the source user needs the changeStream action on database %s): %v. Nothing was written", src.Name(), err)}
	}
	stream.TryNext(ctx)
	err = stream.Err()
	_ = stream.Close(context.Background())
	if err != nil && ctx.Err() == nil {
		return &ConfigurationError{fmt.Sprintf("the source's change stream cannot be read: %v. Nothing was written", err)}
	}
	return ctx.Err()
}
