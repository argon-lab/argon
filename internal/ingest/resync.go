package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
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
//     name that no longer denotes an ordinary source collection has none
//     of its documents there, even if a view now uses that name. A renamed
//     collection is that plus the first pass's puts under the new name.
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
	// BatchSize is the maximum number of documents per batch (default 500).
	BatchSize int
	// Progress is optional.
	Progress func(ResyncProgress)
	// WriteBacks and Redact are as in FollowOptions.
	WriteBacks WriteBackLedger
	Redact     func(string) string
	// Watch replaces src.Watch for the check that the source's change stream
	// can be opened, made before the resync is recorded. Tests use it to supply
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
	lease, err := s.acquireSourceLease(ctx, branchID, src.Name(), "resync")
	if err != nil {
		return nil, redacted(err, opts.Redact)
	}
	defer s.releaseSourceLease(lease)
	// Admission uses the snapshot from the lease transaction, before takeover
	// replaces the lost owner's report with an unverified checkpoint.
	stored := lease.prior
	probe, err := stored.Health.ResyncAdmission(opts.Force)
	if err != nil {
		return nil, err
	}
	var resumeAfter bson.Raw
	if probe {
		resumeAfter = stored.resumeToken
	}

	cfg := runConfig{actor: sourceActor, redact: opts.Redact, lease: lease, resync: true}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return nil, &ConfigurationError{fmt.Sprintf("the source is being followed for branch %s in this process; stop that first", branchID)}
	}
	result := &ResyncResult{}
	cfg.stream = func(ctx context.Context, run *running) error {
		return s.resync(ctx, branchID, src, opts, run, stored, result, resumeAfter)
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

func (s *Service) resync(ctx context.Context, branchID string, src *mongo.Database, opts ResyncOptions, run *running, stored *Status, result *ResyncResult, resumeAfter bson.Raw) error {
	branch := run.lease.branch
	var status SourceStatus
	if stored.Source != nil {
		status = *stored.Source
	}

	// Read the boundary before listing or reading any source collection.
	boundary, err := primaryTime(ctx, src)
	if err != nil {
		return err
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
		status.Resync = &ResyncState{StartedAt: time.Now()}
		status.Resync.Cause = stored.facts.StoredError
	}
	// Every read below is causally at or after the boundary, so the stream
	// starts with the cluster time after it.
	startAt := boundary
	if startAt.I++; startAt.I == 0 {
		startAt.T++
	}
	// Open the change stream once before recording the resync. The replay
	// only starts after the whole comparison; a user without the changeStream
	// privilege should learn that now, with the branch untouched and no
	// unfinished resync on record.
	probes := []*options.ChangeStreamOptions{sourceStreamOptions().SetStartAtOperationTime(&startAt)}
	if len(resumeAfter) != 0 {
		probes = append([]*options.ChangeStreamOptions{sourceStreamOptions().SetResumeAfter(resumeAfter)}, probes...)
	}
	for _, probe := range probes {
		err := probeStream(ctx, src, probe, opts.Watch)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if probe.ResumeAfter != nil {
			if err == nil {
				return &ConfigurationError{"`argon source follow` can resume from the saved position. Start it to catch up and verify a checkpoint; pass --force to compare the whole source now"}
			}
			var gap *CaptureError
			if errors.As(streamCaptureError(err, true), &gap) && gap.AutoResyncAllowed() {
				continue
			}
			return &ConfigurationError{fmt.Sprintf("could not verify whether `argon source follow` can resume from the saved position: %v. Nothing was written to the branch history", err)}
		}
		if err != nil {
			return &ConfigurationError{fmt.Sprintf("%v. Nothing was written to the branch history", err)}
		}
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
	stored.BranchID, stored.State, stored.Actor, stored.Error = branchID, "resyncing", sourceActor, ""
	stored.UpdatedAt, stored.Source = time.Now(), &status
	stored.Health = nil
	fields := bson.M{
		"start_at": startAt, "actor": sourceActor,
		"capture_status.source":    status,
		"capture_status.branch_id": stored.BranchID, "capture_status.state": stored.State,
		"capture_status.actor": stored.Actor, "capture_status.error": stored.Error, "capture_status.updated_at": stored.UpdatedAt,
	}
	// Record that the branch is inexact only after the source preflight. The
	// marker and the head check require the lease and binding to remain ours.
	if _, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if err := s.updateSourceLease(sc, run.lease, bson.M{"$set": fields, "$unset": bson.M{"resume_token": ""}}); err != nil {
			return nil, err
		}
		return nil, s.branches.FollowerCheckpoint(sc, branch.ID, branch.Follows, branch.HeadLSN)
	}); err != nil {
		if fenced := followerWriteError(branch, err); fenced != err {
			return fenced
		}
		return fmt.Errorf("failed to record the start of the resync: %w", err)
	}
	run.resyncStarted = true
	s.mu.Lock()
	s.statuses[branchID] = *stored
	s.mu.Unlock()
	result.FromLSN = from

	session, err := src.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(context.Background())
	readsFrom := boundary
	if err := session.AdvanceOperationTime(&readsFrom); err != nil {
		return err
	}
	r := &resyncer{s: s, branch: branch, src: src, opts: opts, result: result, lease: run.lease, reads: mongo.NewSessionContext(ctx, session)}
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
		deleted, err := r.prune(ctx, name, onSource[name])
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

	expectedHead := branch.HeadLSN
	point, err := s.followStream(ctx, branchID, src, FollowOptions{
		AllowInexact: true, WriteBacks: opts.WriteBacks, Redact: opts.Redact, resync: true, startAt: &startAt, expectedHead: &expectedHead,
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
	lease  *sourceLease
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

const (
	// Leave room below MongoDB's 16 MiB limit for the selector and command
	// envelope. Count encoded keys separately because typed keys expand.
	resyncBatchBytes = 15 * 1024 * 1024
	// Covers the BSON type, array index as a cstring, and string framing.
	resyncSelectorElementOverhead = 32
)

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
	var documentBytes, keyBytes int
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
		clear(batch)
		batch = batch[:0]
		documentBytes, keyBytes = 0, 0
		if err := r.commit(); err != nil {
			return err
		}
		r.progress("compare", name)
		return ctx.Err()
	}
	for cursor.Next(r.reads) {
		id, err := wal.DocumentIDFromImage(cursor.Current)
		if err != nil {
			return fmt.Errorf("invalid _id on a document in %s: %w", name, err)
		}
		key := wal.DocumentIDString(id)
		keySize := len(key) + resyncSelectorElementOverhead
		if len(batch) > 0 && (documentBytes+len(cursor.Current) > resyncBatchBytes || keyBytes+keySize > resyncBatchBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, read{key: key, doc: append(bson.Raw(nil), cursor.Current...)})
		documentBytes += len(cursor.Current)
		keyBytes += keySize
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

// pruneSourceKeys checks the source's ordinary collections in batches of
// encoded BSON IDs. Replay covers catalog changes after the collection list.
func (r *resyncer) pruneSourceKeys(name string, keys []string, live map[string]bson.M) (map[string]bool, error) {
	found := make(map[string]bool, len(keys))
	ids := make(bson.A, 0, len(keys))
	idBytes := 0
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		cursor, err := r.collection(name).Find(r.reads, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
			options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return err
		}
		defer func() { _ = cursor.Close(context.Background()) }()
		// MongoDB matches 1 and 1.0 as the same _id, so compare the
		// branch's document keys to keep them apart.
		for cursor.Next(r.reads) {
			id, err := wal.DocumentIDFromImage(cursor.Current)
			if err != nil {
				return err
			}
			found[wal.DocumentIDString(id)] = true
		}
		clear(ids)
		ids, idBytes = ids[:0], 0
		return cursor.Err()
	}
	for _, key := range keys {
		doc, ok := live[key]
		if !ok {
			continue
		}
		id := bson.RawValue{Type: bson.TypeNull}
		if value := doc["_id"]; value != nil {
			kind, data, err := bson.MarshalValue(value)
			if err != nil {
				return nil, err
			}
			id = bson.RawValue{Type: kind, Value: data}
		}
		idSize := len(id.Value) + resyncSelectorElementOverhead
		if len(ids) > 0 && idBytes+idSize > resyncBatchBytes {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		ids = append(ids, id)
		idBytes += idSize
	}
	return found, flush()
}

// prune deletes from the branch every document of one collection that the
// source no longer has, and returns how many that were.
func (r *resyncer) prune(ctx context.Context, name string, onSource bool) (int64, error) {
	branch := r.branch
	var deleted int64
	// A point read can refuse where a replay of the collection still works
	// (entries that name no document, history reclaimed meanwhile).
	replay := false
	keys := make([]string, 0, r.opts.BatchSize)
	keyBytes := 0
	flush := func(live map[string]bson.M) error {
		if len(keys) == 0 {
			return ctx.Err()
		}
		if live == nil {
			var err error
			if live, err = r.opts.State.MaterializeDocumentsAtLSN(branch, name, keys, branch.HeadLSN); err != nil {
				replay = true
				return err
			}
		}
		if len(live) == 0 {
			clear(keys)
			keys, keyBytes = keys[:0], 0
			return ctx.Err()
		}
		var found map[string]bool
		if onSource {
			var err error
			found, err = r.pruneSourceKeys(name, keys, live)
			if err != nil {
				return err
			}
		}
		for _, key := range keys {
			if _, ok := live[key]; ok && !found[key] {
				r.pending = append(r.pending, r.entry(wal.OpDelete, name, key, nil))
				r.result.Deletes++
				deleted++
			}
		}
		clear(keys)
		keys, keyBytes = keys[:0], 0
		if err := r.commit(); err != nil {
			return err
		}
		r.progress("prune", name)
		return ctx.Err()
	}
	addKey := func(key string, live map[string]bson.M) error {
		size := len(key) + resyncSelectorElementOverhead
		if len(keys) == r.opts.BatchSize || (len(keys) > 0 && keyBytes+size > resyncBatchBytes) {
			if err := flush(live); err != nil {
				return err
			}
		}
		keys = append(keys, key)
		keyBytes += size
		return nil
	}

	floor, err := r.s.wal.ReclaimFloor(branch.ID, name)
	if err != nil {
		return 0, err
	}
	if floor == 0 && branch.ParentID == "" {
		// The log names every document the branch has ever held here.
		upTo := branch.HeadLSN
		err := r.s.wal.ScanDocumentKeys(ctx, branch.ID, name, branch.BaseLSN+1, upTo, func(key string) error {
			return addKey(key, nil)
		})
		if err == nil {
			err = flush(nil)
		}
		if err == nil || !replay {
			return deleted, err
		}
	}
	clear(keys)
	keys, keyBytes = keys[:0], 0
	state, err := r.opts.State.MaterializeCollectionAtLSN(branch, name, branch.HeadLSN)
	if err != nil {
		return deleted, err
	}
	for key := range state {
		if err := addKey(key, state); err != nil {
			return deleted, err
		}
	}
	return deleted, flush(state)
}

// commit stores the pending entries, branch head, and progress under the
// resync's lease in one transaction.
func (r *resyncer) commit() error {
	if len(r.pending) == 0 {
		return nil
	}
	s, branch, batch := r.s, r.branch, r.pending
	ctx, cancel := context.WithTimeout(r.lease.ctx, 30*time.Second)
	defer cancel()
	at := time.Now()
	var head int64
	if _, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if err := s.renewSourceLease(sc, r.lease); err != nil {
			return nil, err
		}
		lsns, err := s.wal.AppendBatchContext(sc, batch)
		if err != nil {
			return nil, fmt.Errorf("failed to append resync entries: %w", err)
		}
		head = lsns[len(lsns)-1]
		if err := s.updateSourceLease(sc, r.lease, bson.M{"$set": bson.M{
			"updated_at": at, "capture_status.last_captured_at": at, "capture_status.head_lsn": head,
		}}); err != nil {
			return nil, err
		}
		return nil, s.branches.FollowerSetHead(sc, branch.ID, branch.Follows, branch.HeadLSN, head)
	}); err != nil {
		return followerWriteError(branch, err)
	}
	r.lease.recordRenewal()
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
