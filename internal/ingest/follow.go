package ingest

import (
	"context"
	"encoding/binary"
	"encoding/hex"
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

// Following an external database.
//
// A branch that follows a source has one writer: this file. It reads the
// source database's change stream and appends each change to the branch as a
// physical entry, exactly like capture on a checked-out branch, with three
// differences that all come from the source not being Argon's:
//
//   - Nothing is written to the source. Capture proves that a transaction's
//     events are complete by inserting a marker into the physical database;
//     here the proof is the stream's own position, read from its resume
//     token, having moved past the transaction's cluster time.
//   - Pre- and post-images are not switched on. An update without an attached
//     post-image is read back from the source as of the update's cluster
//     time, which MongoDB can serve for a bounded time (its snapshot history
//     window). Past that window the exact document is gone, and the follower
//     stops as degraded unless the operator allowed an inexact catch-up.
//   - The source and the Argon metadata may be different deployments. The
//     entries, the branch head and the resume token still commit in one
//     transaction, on the metadata deployment, so a restart neither skips
//     nor repeats a change.
//
// Heads are published only between cluster times. The branch at a published
// head is therefore the source database at one cluster time, except inside a
// range recorded as inexact.

const (
	// ModeExact: every entry is the source's document at its change.
	ModeExact = "exact"
	// ModeCatchUp: some entries carry a later state of their document than
	// the change they stand for. The branch is converging on the source and
	// is not yet the source at any single cluster time.
	ModeCatchUp = "catch_up"

	sourceActor = "source"

	// maxInexactRanges bounds the status document; older ranges are counted.
	maxInexactRanges = 32
)

// ConsistentPoint says that the branch at LSN is the source database at
// ClusterTime.
type ConsistentPoint struct {
	LSN         int64               `bson:"lsn" json:"lsn"`
	ClusterTime primitive.Timestamp `bson:"cluster_time" json:"cluster_time"`
	At          time.Time           `bson:"at" json:"at"`
}

// InexactRange is a span of a followed branch's history whose entries are
// not the source's documents at their changes. The state at ToLSN is exact;
// positions inside the range are not states the source was ever in.
type InexactRange struct {
	FromLSN     int64               `bson:"from_lsn" json:"from_lsn"`
	ToLSN       int64               `bson:"to_lsn" json:"to_lsn"`
	Reason      string              `bson:"reason" json:"reason"`
	ConvergedAt primitive.Timestamp `bson:"converged_at" json:"converged_at"`
}

// SourceStatus is the part of a branch's capture status that describes
// following: where the stream stands and how much of the history is exact.
type SourceStatus struct {
	Database string `bson:"database" json:"database"`
	Mode     string `bson:"mode" json:"mode"`
	// Position is the source cluster time the branch head has reached.
	Position primitive.Timestamp `bson:"position" json:"position"`
	// Consistent is the newest point at which the head equalled the source.
	Consistent *ConsistentPoint `bson:"consistent,omitempty" json:"consistent,omitempty"`
	// InexactFromLSN and InexactReason describe a catch-up still in progress.
	InexactFromLSN int64  `bson:"inexact_from_lsn,omitempty" json:"inexact_from_lsn,omitempty"`
	InexactReason  string `bson:"inexact_reason,omitempty" json:"inexact_reason,omitempty"`
	// InexactRanges are the finished ones, oldest first.
	InexactRanges        []InexactRange `bson:"inexact_ranges,omitempty" json:"inexact_ranges,omitempty"`
	InexactRangesDropped int            `bson:"inexact_ranges_dropped,omitempty" json:"inexact_ranges_dropped,omitempty"`
	HeartbeatAt          time.Time      `bson:"heartbeat_at,omitempty" json:"heartbeat_at,omitempty"`
	// Resync is set from the first write of a resync until the branch equals
	// the source again. While it is set the saved stream position is not one
	// to resume from, and only a resync may write to the branch.
	Resync *ResyncState `bson:"resync,omitempty" json:"resync,omitempty"`
}

// WriteBack identifies a merge plan whose changes Argon wrote to the source.
type WriteBack struct {
	PlanID         string
	SourceBranchID string
	SourceBranch   string
}

// WriteBackLedger lets the follower recognise the source transactions that
// Argon's own write-back made, so their entries are attributed to the merge
// and the plan is closed in the same transaction that stores them.
type WriteBackLedger interface {
	// Lookup returns the write-back that ran in a source session, or nil.
	Lookup(ctx context.Context, branchID string, session primitive.Binary) (*WriteBack, error)
	// Settle marks the plan applied. ctx is the storing transaction.
	Settle(ctx context.Context, planID string) error
	// Release returns to pending the plans that were claimed for write-back
	// before the given time and whose transaction never appeared in the
	// stream, which by then it would have.
	Release(ctx context.Context, branchID string, claimedBefore time.Time) error
}

// FollowOptions configures Follow.
type FollowOptions struct {
	// AllowInexact lets the follower continue past an update whose exact
	// post-image the source can no longer serve, by storing the document's
	// current state instead and recording the affected range as inexact.
	// Without it such an update stops the follower as degraded.
	AllowInexact bool
	// Ready is closed once the stream is open at the saved position.
	Ready chan<- struct{}
	// WriteBacks is optional.
	WriteBacks WriteBackLedger
	// Redact cleans error text before it is stored or printed. The caller
	// knows the connection string; this package never sees it.
	Redact func(string) string
	// Watch replaces src.Watch. Tests use it to supply server errors that a
	// fresh replica set cannot be made to produce, such as a resume position
	// its oplog no longer holds.
	Watch func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error)
	// resync marks the stream that ends a resync, the only one allowed to
	// run while the branch's status says a resync is unfinished.
	resync bool
}

// Follow applies the source database's changes to a branch until ctx ends,
// resuming from the position saved with the last applied change. Transient
// failures are retried; a gap that cannot be repaired stops it and is
// reported as degraded.
func (s *Service) Follow(ctx context.Context, branchID string, src *mongo.Database, opts FollowOptions) error {
	cfg := runConfig{ready: opts.Ready, actor: sourceActor, redact: opts.Redact, follower: true}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return fmt.Errorf("the source is already being followed for branch %s in this process", branchID)
	}
	cfg.stream = func(ctx context.Context, run *running) error {
		_, err := s.followStream(ctx, branchID, src, opts, run, false)
		return err
	}
	return redacted(s.execute(runCtx, branchID, cfg, run, true), opts.Redact)
}

// CatchUp applies the source's changes from startAt until the branch equals
// the source at one cluster time, and returns that point. It is the second
// half of an online import: the copy before it read the source while it was
// being written, so the branch only becomes a state of the source once every
// change made during the copy has been applied on top.
func (s *Service) CatchUp(ctx context.Context, branchID string, src *mongo.Database, startAt primitive.Timestamp, firstLSN int64, reason string, redact func(string) string) (*ConsistentPoint, error) {
	status := SourceStatus{Database: src.Name(), Mode: ModeCatchUp, Position: startAt, InexactFromLSN: firstLSN, InexactReason: reason}
	if _, err := s.state.UpdateOne(ctx, bson.M{"_id": branchID}, bson.M{"$set": bson.M{
		"start_at": startAt, "actor": sourceActor, "capture_status.source": status,
	}}, options.Update().SetUpsert(true)); err != nil {
		return nil, fmt.Errorf("failed to persist the source stream boundary: %w", err)
	}
	cfg := runConfig{actor: sourceActor, redact: redact, follower: true}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return nil, fmt.Errorf("the source is already being followed for branch %s in this process", branchID)
	}
	var point *ConsistentPoint
	cfg.stream = func(ctx context.Context, run *running) error {
		reached, err := s.followStream(ctx, branchID, src, FollowOptions{AllowInexact: true}, run, true)
		if reached != nil {
			point = reached
		}
		return err
	}
	if err := s.execute(runCtx, branchID, cfg, run, true); err != nil {
		return nil, redacted(err, redact)
	}
	if point == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("following stopped before the branch reached a consistent point")
	}
	return point, nil
}

// follower is one open stream's working state.
type follower struct {
	s        *Service
	branch   *wal.Branch
	src      *mongo.Database
	session  mongo.Session
	opts     FollowOptions
	status   SourceStatus
	branchID string

	// ready holds entries of complete cluster times, in order; readyToken
	// resumes after the last of them and readyAt is its cluster time.
	ready      []*wal.Entry
	readyToken bson.Raw
	readyAt    primitive.Timestamp
	settles    []*WriteBack

	// pending holds the cluster time still being received.
	pendingOpen  bool
	pending      []*wal.Entry
	pendingAt    primitive.Timestamp
	pendingToken bson.Raw
	pendingWB    *WriteBack

	// need is the cluster time the stream must pass before a catch-up can be
	// declared finished: no document read for it showed anything later.
	need   primitive.Timestamp
	looked bool
	// goneBefore skips exact reads that are known to fail: the snapshot
	// history ends somewhere after this cluster time.
	goneBefore uint32

	// opened is the position the stream was opened at, until the stream has
	// been seen to move past it; see reached.
	opened primitive.Timestamp

	// unpublished is set while an import is still catching up: the project
	// does not exist for anyone yet, and nothing derived from it is kept.
	unpublished bool
	run         *running
	lastRelease time.Time

	writeBacks map[string]*WriteBack
	lastCommit time.Time
	lastToken  bson.Raw
	lastBeat   time.Time
}

func (s *Service) followStream(ctx context.Context, branchID string, src *mongo.Database, opts FollowOptions, run *running, untilConsistent bool) (*ConsistentPoint, error) {
	branch, err := s.branches.GetBranchByIDContext(ctx, branchID)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, &ConfigurationError{fmt.Sprintf("branch %s not found", branchID)}
		}
		return nil, fmt.Errorf("branch %s not found: %w", branchID, err)
	}
	if !branch.IsFollower() {
		return nil, &ConfigurationError{fmt.Sprintf("branch %s does not follow an external database; import it with --online first", branch.Name)}
	}
	if branch.Follows != src.Name() {
		return nil, &ConfigurationError{fmt.Sprintf("branch %s follows database %q, not %q", branch.Name, branch.Follows, src.Name())}
	}
	if branch.IsLive() {
		return nil, &ConfigurationError{fmt.Sprintf("branch %s is checked out; a followed branch has no physical database", branch.Name)}
	}

	token, startAt, status, err := s.loadFollowState(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if token == nil && startAt == nil {
		return nil, &ConfigurationError{fmt.Sprintf("branch %s has no saved source position to resume from", branch.Name)}
	}
	// The saved position is only meaningful on top of the branch the
	// follower left. Something else that moved the head (a process that
	// predates following and does not know this branch is read-only), or
	// that replaced the saved state, means the branch no longer equals the
	// source at that position.
	if status.Database == "" {
		return nil, &CaptureError{fmt.Sprintf("the saved source position of branch %s was replaced by another process (one that does not know the branch follows a source); it cannot be resumed. Upgrade every process that uses this metadata database, then run `argon source resync`", branch.Name)}
	}
	if stored, err := s.storedHead(ctx, branchID); err != nil {
		return nil, err
	} else if stored != 0 && stored != branch.HeadLSN && !opts.resync {
		return nil, &CaptureError{fmt.Sprintf("the head of branch %s moved from LSN %d to %d without the follower: something else wrote to it, so it no longer equals the source. Upgrade every process that uses this metadata database, then run `argon source resync`", branch.Name, stored, branch.HeadLSN)}
	}
	if status.Resync != nil && !opts.resync {
		// Its saved position belongs to the resync's replay and is only valid
		// on top of a finished comparison with the source.
		return nil, &ConfigurationError{fmt.Sprintf("a resync of branch %s was started and has not finished: the branch is between its last consistent state and the source's current one. Run `argon source resync` again to complete it", branch.Name)}
	}
	if status.Mode == "" {
		status.Mode = ModeExact
	}
	status.Database = src.Name()

	csOpts := options.ChangeStream().
		SetBatchSize(maxBatch).
		SetFullDocument(options.WhenAvailable).
		SetFullDocumentBeforeChange(options.WhenAvailable).
		SetShowExpandedEvents(true).
		SetMaxAwaitTime(500 * time.Millisecond)
	// opened is the position the stream starts from.
	var opened primitive.Timestamp
	if token != nil {
		csOpts.SetResumeAfter(token)
		opened, _ = tokenTime(token)
	} else {
		csOpts.SetStartAtOperationTime(startAt)
		opened = *startAt
	}
	watch := opts.Watch
	if watch == nil {
		watch = func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		}
	}
	stream, err := watch(ctx, src, csOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to open the change stream on source database %s: %w", src.Name(), err)
	}
	defer func() { _ = stream.Close(context.Background()) }()

	now, err := primaryTime(ctx, src)
	if err != nil {
		return nil, err
	}
	// The stream's position is read from its resume tokens. Check once that
	// this server's tokens say what is expected of them before relying on it.
	if at, ok := tokenTime(stream.ResumeToken()); len(stream.ResumeToken()) > 0 && (!ok || at.T < 1_000_000_000 || at.T > now.T+600) {
		return nil, &ConfigurationError{"this MongoDB version's change-stream resume token is not in the format Argon reads the stream position from; following is not supported on it"}
	}

	session, err := src.Client().StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(context.Background())

	f := &follower{s: s, branch: branch, src: src, session: session, opts: opts, status: status, branchID: branchID,
		writeBacks: make(map[string]*WriteBack), lastCommit: time.Now(), opened: opened,
		unpublished: untilConsistent && !opts.resync, run: run}
	if f.status.Mode == ModeCatchUp {
		// Every document read for this catch-up so far happened before now.
		f.need = now
	}

	if err := s.report(branchID, f.stateName(), sourceActor, ""); err != nil {
		return nil, err
	}
	run.readyOnce.Do(func() {
		close(run.ready)
		if opts.Ready != nil {
			close(opts.Ready)
		}
	})

	for {
		if ctx.Err() != nil {
			// Complete cluster times are stored; the one still open is read
			// again after the saved position.
			return nil, f.flushReady()
		}
		if stream.TryNext(ctx) {
			if err := f.receive(ctx, stream.Current); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return nil, f.flushReady()
				}
				// The event that failed is not stored and not passed over:
				// only what completed before it is.
				if commitErr := f.flushReady(); commitErr != nil {
					return nil, commitErr
				}
				return nil, err
			}
			// One commit per server batch keeps the head close behind the
			// stream without waiting for it to fall idle.
			if len(f.ready) >= maxBatch || (len(f.ready) > 0 && stream.RemainingBatchLength() == 0) {
				if point, err := f.commitAndCheck(ctx, f.readyToken, f.readyAt); err != nil {
					return nil, err
				} else if point != nil && untilConsistent {
					return point, nil
				}
			}
			continue
		}
		if err := stream.Err(); err != nil {
			if ctx.Err() != nil {
				return nil, f.flushReady()
			}
			return nil, fmt.Errorf("change stream error: %w", err)
		}

		// The server had nothing more. Its resume token now marks how far the
		// stream has read, whether or not anything there concerned this
		// database.
		at, ok := tokenTime(stream.ResumeToken())
		if !ok {
			return nil, &ConfigurationError{"the change stream returned a resume token Argon cannot read the stream position from"}
		}
		if f.pendingOpen && at.After(f.pendingAt) {
			f.promote()
		}
		if f.pendingOpen {
			// The open cluster time is complete only once the stream has
			// moved past it. On an idle deployment that takes until its next
			// oplog entry (MongoDB writes one every ten seconds).
			if len(f.ready) > 0 {
				if point, err := f.commitAndCheck(ctx, f.readyToken, f.readyAt); err != nil {
					return nil, err
				} else if point != nil && untilConsistent {
					return point, nil
				}
			}
			f.heartbeat()
			continue
		}
		token := append(bson.Raw(nil), stream.ResumeToken()...)
		if len(f.ready) == 0 && f.status.Mode == ModeExact && (string(token) == string(f.lastToken) || time.Since(f.lastCommit) < time.Second) {
			continue
		}
		if at, err = f.reached(ctx, at); err != nil {
			return nil, err
		}
		if point, err := f.commitAndCheck(ctx, token, at); err != nil {
			return nil, err
		} else if point != nil && untilConsistent {
			return point, nil
		}
	}
}

// reached returns the cluster time an idle stream's token stands for. That is
// the token's own time, with one exception: a stream opened at a position
// reports that position until the oplog moves, and the position may be a
// cluster time the source has not reached yet (a stream that starts right
// after the primary's current time does). Nothing has happened there, so the
// branch equals the source at the primary's current time, and that is the
// time to report: a write could still be given the later one.
func (f *follower) reached(ctx context.Context, at primitive.Timestamp) (primitive.Timestamp, error) {
	if f.opened.IsZero() {
		return at, nil
	}
	if at.After(f.opened) {
		f.opened = primitive.Timestamp{}
		return at, nil
	}
	now, err := primaryTime(ctx, f.src)
	if err != nil {
		return at, err
	}
	if at.After(now) {
		return now, nil
	}
	f.opened = primitive.Timestamp{}
	return at, nil
}

// flushReady stores complete cluster times that have not been stored yet. It
// never moves the saved position without entries: an earlier commit may have
// saved a later one.
func (f *follower) flushReady() error {
	if len(f.ready) == 0 {
		return nil
	}
	return f.commit(f.readyToken, f.readyAt)
}

func redacted(err error, redact func(string) string) error {
	if err == nil || redact == nil {
		return err
	}
	if message := redact(err.Error()); message != err.Error() {
		return &redactedError{message: message, cause: err}
	}
	return err
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

func (f *follower) stateName() string {
	if f.status.Mode == ModeCatchUp {
		return "catching_up"
	}
	return "running"
}

// promote closes the open cluster time: its entries may now be published.
func (f *follower) promote() {
	if f.pendingWB != nil && len(f.pending) > 0 {
		f.pending = append(f.pending, &wal.Entry{
			ProjectID: f.branch.ProjectID, BranchID: f.branch.ID,
			Operation: wal.OpMerge, Actor: "merge", TxnID: "merge:" + f.pendingWB.PlanID,
			Metadata: map[string]interface{}{
				"plan_id": f.pendingWB.PlanID, "source_branch_id": f.pendingWB.SourceBranchID,
				"source_branch": f.pendingWB.SourceBranch, "changes": len(f.pending),
				"write_back": true, "source_time": f.pendingAt,
			},
		})
		f.settles = append(f.settles, f.pendingWB)
	}
	f.ready = append(f.ready, f.pending...)
	f.readyToken, f.readyAt = f.pendingToken, f.pendingAt
	f.pending, f.pendingToken, f.pendingWB, f.pendingOpen = nil, nil, nil, false
}

// receive converts one change event and adds it to the open cluster time.
func (f *follower) receive(ctx context.Context, raw bson.Raw) error {
	var event changeEvent
	if err := bson.Unmarshal(raw, &event); err != nil {
		return fmt.Errorf("failed to decode change event: %w", err)
	}
	if f.pendingOpen && !event.ClusterTime.Equal(f.pendingAt) {
		f.promote()
	}
	entry, err := f.convert(ctx, raw, &event)
	if err != nil {
		return err
	}
	f.pendingOpen, f.pendingAt = true, event.ClusterTime
	if id, ok := raw.Lookup("_id").DocumentOK(); ok {
		f.pendingToken = append(bson.Raw(nil), id...)
	}
	if entry == nil {
		return nil
	}
	if wb, err := f.writeBack(ctx, &event); err != nil {
		return err
	} else if wb != nil {
		f.pendingWB = wb
		entry.Actor = "merge:" + wb.SourceBranch
		entry.TxnID = "merge:" + wb.PlanID
	}
	f.pending = append(f.pending, entry)
	return nil
}

// writeBack reports whether an event belongs to a transaction that Argon's
// write-back ran. Such a transaction uses a session of its own, recorded with
// the plan before the transaction starts.
func (f *follower) writeBack(ctx context.Context, event *changeEvent) (*WriteBack, error) {
	if f.opts.WriteBacks == nil || len(event.LSID) == 0 {
		return nil, nil
	}
	subtype, data, ok := event.LSID.Lookup("id").BinaryOK()
	if !ok {
		return nil, nil
	}
	key := string(data)
	if wb, seen := f.writeBacks[key]; seen {
		return wb, nil
	}
	wb, err := f.opts.WriteBacks.Lookup(ctx, f.branch.ID, primitive.Binary{Subtype: subtype, Data: data})
	if err != nil {
		return nil, fmt.Errorf("failed to check a source transaction against pending write-backs: %w", err)
	}
	if len(f.writeBacks) > 4096 {
		clear(f.writeBacks)
	}
	f.writeBacks[key] = wb
	return wb, nil
}

func (f *follower) convert(ctx context.Context, raw bson.Raw, event *changeEvent) (*wal.Entry, error) {
	switch event.OperationType {
	case "insert", "update", "replace", "delete":
	case "drop", "dropDatabase", "rename":
		if event.OperationType == "drop" && source.SkipCollection(event.NS.Collection) {
			// Neither copied nor followed, so its disappearance changes nothing.
			return nil, nil
		}
		return nil, &CaptureError{fmt.Sprintf("%s of %s on the source is not followed: collection-level changes carry no documents, so the branch would stop matching the source; history is incomplete from here. Run `argon source resync` to bring the branch to the source's current state and follow it from there", event.OperationType, event.NS.Collection)}
	case "invalidate":
		return nil, &CaptureError{"the source change stream was invalidated (database dropped?); the branch cannot follow it past this point. Run `argon source resync` to bring the branch to the source's current state"}
	case "create":
		// A time-series collection stores its data in system.buckets.*,
		// which a change stream does not deliver as documents. The import
		// refuses a source that has one; a follower that passed its creation
		// would go on reporting the branch as equal to a source that holds
		// data the branch can never receive. A view has no data of its own
		// and is not followed.
		if on, ok := event.OperationDescription.Lookup("viewOn").StringValueOK(); ok && strings.HasPrefix(on, "system.buckets.") {
			return nil, &CaptureError{fmt.Sprintf("time-series collection %s was created on the source; its measurements cannot be followed, so the branch no longer equals the source from here. A source with time-series collections cannot be followed", event.NS.Collection)}
		}
		return nil, nil
	default:
		return nil, nil
	}
	if source.SkipCollection(event.NS.Collection) {
		return nil, nil
	}
	metadata := map[string]interface{}{
		"capture_operation": event.OperationType, "capture_wall_time": event.WallTime, "source_time": event.ClusterTime,
	}
	entry := &wal.Entry{
		ProjectID: f.branch.ProjectID, BranchID: f.branch.ID,
		Operation: wal.OpPut, Collection: event.NS.Collection,
		DocumentID: wal.DocumentIDString(event.DocumentKey.ID),
		PreImage:   event.FullDocumentBeforeChange,
		TxnID:      event.txnID(), Actor: sourceActor, Metadata: metadata,
	}
	if event.OperationType == "delete" {
		entry.Operation = wal.OpDelete
		return entry, nil
	}
	post := event.FullDocument
	switch {
	case len(post) > 0 && event.OperationType != "update":
		// Inserts and replacements always carry the document.
	case len(post) > 0:
		metadata["capture_images"] = "attached"
		// MongoDB 7.0 and 8.x can attach a post-image that names a moved
		// field twice. Outside a transaction the stored document can be read
		// back in its place; otherwise the attached one is kept and marked.
		// It still decodes to the right values, the newer occurrence last.
		if hasDuplicateFields(post) {
			metadata["capture_images"] = "attached_unverified"
			if id, err := raw.LookupErr("documentKey", "_id"); err == nil && event.txnID() == "" {
				stored, err := documentAt(ctx, f.src, event.NS.Collection, id, event.ClusterTime)
				if err != nil && !errors.Is(err, errSnapshotGone) {
					return nil, err
				}
				if err == nil && len(stored) > 0 {
					post = stored
					metadata["capture_images"] = "snapshot"
				}
			}
		}
	default:
		// The source collection does not record post-images. Ask the source
		// for the document as of the update.
		read, exact, err := f.readPostImage(ctx, raw, event)
		if err != nil {
			return nil, err
		}
		if len(read) == 0 {
			// The document does not exist as of the read: a later change at
			// or after this one removed it, and that change's event follows.
			return nil, nil
		}
		post = read
		metadata["capture_images"] = "current"
		if exact {
			metadata["capture_images"] = "snapshot"
		}
	}
	entry.PostImage = post
	return entry, nil
}

// readPostImage returns an update's post-image and whether it is exact. An
// exact one is the document at the update's cluster time. In a catch-up an
// update whose time the source no longer serves gets the document's current
// state instead, which is exact only for the document's last change; the
// catch-up ends once the stream has passed everything such reads could see.
func (f *follower) readPostImage(ctx context.Context, raw bson.Raw, event *changeEvent) (bson.Raw, bool, error) {
	id, err := raw.LookupErr("documentKey", "_id")
	if err != nil {
		return nil, false, &CaptureError{fmt.Sprintf("update on %s carries no document key; history is incomplete", event.NS.Collection)}
	}
	if f.status.Mode != ModeCatchUp || event.ClusterTime.T >= f.goneBefore {
		// A snapshot read shows the document after everything that committed
		// at this cluster time. Heads are published only between cluster
		// times, so that is the state every published head needs.
		doc, err := documentAt(ctx, f.src, event.NS.Collection, id, event.ClusterTime)
		if err == nil {
			return doc, true, nil
		}
		if !errors.Is(err, errSnapshotGone) {
			return nil, false, err
		}
		unsupported := errors.Is(err, errSnapshotUnsupported)
		// Asked on every run, whatever mode an earlier run left behind: a
		// catch-up that was allowed once is not allowed for good.
		if !f.opts.AllowInexact {
			if unsupported {
				return nil, false, &CaptureError{fmt.Sprintf("exact post-image unavailable for an update on %s: %v. This is not a matter of keeping up: the source will not serve this collection at the update's cluster time. That is so for a capped collection on MongoDB 7.0 and earlier, and on MongoDB 6.0 for updates made before the collection was last altered (an index build, a collMod such as enabling images, a rename or a drop). Such an update can only be followed exactly when it carries its own image, which changeStreamPreAndPostImages provides for updates made after it is enabled. The branch is not advanced past this change. Restart following with --allow-inexact to apply it with the document's current state and record the range as inexact history, or run `argon source resync`", event.NS.Collection, err)}
			}
			return nil, false, &CaptureError{fmt.Sprintf("exact post-image unavailable for an update on %s: %v. The branch is not advanced past this change. Either enable changeStreamPreAndPostImages on the source collections so updates carry their own images, or restart following with --allow-inexact to converge on the source's current state and record the range in between as inexact history", event.NS.Collection, err)}
		}
		if f.status.Mode != ModeCatchUp {
			f.status.Mode = ModeCatchUp
			f.status.InexactFromLSN = f.branch.HeadLSN + 1
			f.status.InexactReason = "updates older than the source's snapshot history were applied with the document's later state"
			if unsupported {
				f.status.InexactReason = fmt.Sprintf("updates to %s, which the source does not serve at a past cluster time, were applied with the document's later state", event.NS.Collection)
			}
			_ = f.s.report(f.branchID, f.stateName(), sourceActor, "")
		}
		if unsupported {
			// Age has nothing to do with it; later updates to other
			// collections are still read exactly.
			return f.readCurrent(ctx, event, id)
		}
		// Updates a little newer than this one are gone too; probing each
		// would double the reads of a long catch-up.
		f.goneBefore = event.ClusterTime.T + 30
	}
	return f.readCurrent(ctx, event, id)
}

// readCurrent returns the document as the source holds it now.
func (f *follower) readCurrent(ctx context.Context, event *changeEvent, id bson.RawValue) (bson.Raw, bool, error) {
	// The read must not show less than this change, whichever node serves it.
	notBefore := event.ClusterTime
	if err := f.session.AdvanceOperationTime(&notBefore); err != nil {
		return nil, false, err
	}
	current := f.src.Collection(event.NS.Collection, options.Collection().
		SetReadConcern(readconcern.Majority()).SetReadPreference(readpref.Primary()))
	doc, err := current.FindOne(mongo.NewSessionContext(ctx, f.session), bson.D{{Key: "_id", Value: id}}).Raw()
	f.looked = true
	if err == mongo.ErrNoDocuments {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return doc, false, nil
}

// commitAndCheck stores what is ready and, in a catch-up, declares it over
// once the stored prefix has passed every document read made for it.
func (f *follower) commitAndCheck(ctx context.Context, token bson.Raw, at primitive.Timestamp) (*ConsistentPoint, error) {
	if err := f.commit(token, at); err != nil {
		return nil, err
	}
	if f.status.Mode != ModeCatchUp {
		return nil, nil
	}
	if f.looked {
		now, err := primaryTime(ctx, f.src)
		if err != nil {
			return nil, err
		}
		f.need, f.looked = now, false
	}
	if at.Before(f.need) {
		return nil, nil
	}
	point := &ConsistentPoint{LSN: f.branch.HeadLSN, ClusterTime: at, At: time.Now()}
	finished := InexactRange{FromLSN: f.status.InexactFromLSN, ToLSN: f.branch.HeadLSN, Reason: f.status.InexactReason, ConvergedAt: at}
	previous := f.status
	f.status.Mode, f.status.Consistent = ModeExact, point
	f.status.InexactFromLSN, f.status.InexactReason = 0, ""
	// A resync ends in the same transaction that publishes its result.
	f.status.Resync = nil
	f.goneBefore = 0
	if finished.ToLSN >= finished.FromLSN {
		ranges := append([]InexactRange(nil), f.status.InexactRanges...)
		if n := len(ranges); n > 0 && ranges[n-1].Reason == finished.Reason && ranges[n-1].ToLSN+1 >= finished.FromLSN {
			// One cause, one range: a collection whose every update is
			// inexact would otherwise add a range per update.
			finished.FromLSN = ranges[n-1].FromLSN
			ranges = ranges[:n-1]
		}
		f.status.InexactRanges = append(ranges, finished)
		if extra := len(f.status.InexactRanges) - maxInexactRanges; extra > 0 {
			f.status.InexactRanges = f.status.InexactRanges[extra:]
			f.status.InexactRangesDropped += extra
		}
	}
	if err := f.commit(token, at); err != nil {
		f.status = previous
		return nil, err
	}
	_ = f.s.report(f.branchID, f.stateName(), sourceActor, "")
	return point, nil
}

// commit stores the ready entries, the branch head, the stream position and
// any write-back settlement in one transaction on the metadata deployment.
// It runs on its own context so that events already received are not lost to
// a cancellation arriving mid-commit.
func (f *follower) commit(token bson.Raw, at primitive.Timestamp) error {
	if len(token) == 0 {
		return nil
	}
	s, branch := f.s, f.branch
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	batch := f.ready
	capturedAt := time.Now()
	var eventAt time.Time
	for _, entry := range batch {
		if wall, ok := entry.Metadata["capture_wall_time"].(time.Time); ok && wall.After(eventAt) {
			eventAt = wall
		}
	}
	lagMS := int64(0)
	if !eventAt.IsZero() && capturedAt.After(eventAt) {
		lagMS = capturedAt.Sub(eventAt).Milliseconds()
	}
	status := f.status
	status.Position = at
	head := branch.HeadLSN
	_, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		head = branch.HeadLSN
		if len(batch) > 0 {
			lsns, err := s.wal.AppendBatchContext(sc, batch)
			if err != nil {
				return nil, fmt.Errorf("failed to append followed entries: %w", err)
			}
			head = lsns[len(lsns)-1]
			if err := s.branches.FollowerSetHead(sc, branch.ID, branch.Follows, branch.HeadLSN, head); err != nil {
				return nil, err
			}
		} else if err := s.branches.FollowerCheckpoint(sc, branch.ID, branch.Follows, branch.HeadLSN); err != nil {
			return nil, err
		}
		if status.Mode == ModeExact {
			status.Consistent = &ConsistentPoint{LSN: head, ClusterTime: at, At: capturedAt}
		}
		fields := bson.M{
			"resume_token": token, "updated_at": capturedAt,
			"capture_status.last_captured_at": capturedAt, "capture_status.head_lsn": head,
			"capture_status.source": status,
		}
		if !eventAt.IsZero() {
			fields["capture_status.last_event_at"] = eventAt
			fields["capture_status.last_event_lag_ms"] = lagMS
		}
		// No upsert: the state exists for as long as the branch follows, and
		// a follower must not bring back what a detach removed.
		stored, err := s.state.UpdateOne(sc, bson.M{"_id": branch.ID}, bson.M{"$set": fields})
		if err != nil {
			return nil, fmt.Errorf("failed to persist the source position: %w", err)
		}
		if stored.MatchedCount != 1 {
			return nil, branchwal.ErrNoLongerFollows
		}
		for _, wb := range f.settles {
			if err := f.opts.WriteBacks.Settle(sc, wb.PlanID); err != nil {
				return nil, fmt.Errorf("failed to settle write-back of plan %s: %w", wb.PlanID, err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return followerWriteError(branch, err)
	}
	if f.run != nil {
		f.run.progress.Add(1)
	}
	f.status = status
	f.lastCommit, f.lastToken = capturedAt, append(f.lastToken[:0], token...)
	s.mu.Lock()
	current := s.statuses[branch.ID]
	current.LastCapturedAt, current.HeadLSN = capturedAt, head
	if !eventAt.IsZero() {
		current.LastEventAt, current.LastEventLagMS = eventAt, lagMS
	}
	current.Source = &status
	s.statuses[branch.ID] = current
	s.mu.Unlock()
	f.ready, f.settles = nil, nil
	if len(batch) > 0 {
		branch.HeadLSN = head
		// Not for an import that is still catching up: a snapshot is a copy
		// of the data, and if the import fails nothing of it may remain. The
		// import takes its first snapshot once it is published.
		if s.autoSnapshot != nil && !f.unpublished {
			for range batch {
				copy := *branch
				s.autoSnapshot.MaybeSnapshot(&copy)
			}
		}
	}
	f.releaseStaleWriteBacks(at)
	return nil
}

// followerWriteError names the two ways a follower's write can find that the
// branch is no longer its own. Neither is worth retrying.
func followerWriteError(branch *wal.Branch, err error) error {
	switch {
	case errors.Is(err, branchwal.ErrNoLongerFollows):
		return &ConfigurationError{fmt.Sprintf("branch %s no longer follows database %q (it was detached, checked out or removed); following has stopped and nothing more was written to it", branch.Name, branch.Follows)}
	case errors.Is(err, branchwal.ErrBranchChanged):
		return &CaptureError{fmt.Sprintf("the head of branch %s moved without this follower. If a second follower is running for the project, stop one of them and start following again. Otherwise something else wrote to the branch and it no longer equals the source: make sure every process that uses this metadata database is current, then run `argon source resync`", branch.Name)}
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", errCommitTimeout, err)
	}
	return err
}

// writeBackGrace is how long after a plan was claimed its transaction can
// still appear in the stream: the driver gives a commit up to two minutes,
// and the clocks of the writing host and the source may differ.
const writeBackGrace = 10 * time.Minute

// releaseStaleWriteBacks returns claimed plans to pending once the branch has
// provably passed the time their transaction could have committed. Only an
// exact follower can say that.
func (f *follower) releaseStaleWriteBacks(at primitive.Timestamp) {
	if f.opts.WriteBacks == nil || f.status.Mode != ModeExact || f.pendingOpen || time.Since(f.lastRelease) < 30*time.Second {
		return
	}
	f.lastRelease = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = f.opts.WriteBacks.Release(ctx, f.branchID, time.Unix(int64(at.T), 0).Add(-writeBackGrace))
}

// heartbeat records that the follower is alive while it waits for the stream
// to move past an open cluster time. When the metadata lives on the source's
// own deployment this write is also the oplog entry that moves it.
func (f *follower) heartbeat() {
	if time.Since(f.lastBeat) < time.Second {
		return
	}
	f.lastBeat = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = f.s.state.UpdateOne(ctx, bson.M{"_id": f.branchID}, bson.M{"$set": bson.M{"capture_status.source.heartbeat_at": f.lastBeat}})
}

// storedHead is the branch head the follower last stored with its position.
func (s *Service) storedHead(ctx context.Context, branchID string) (int64, error) {
	var doc struct {
		Status struct {
			HeadLSN int64 `bson:"head_lsn"`
		} `bson:"capture_status"`
	}
	err := s.state.FindOne(ctx, bson.M{"_id": branchID}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return 0, nil
	}
	return doc.Status.HeadLSN, err
}

func (s *Service) loadFollowState(ctx context.Context, branchID string) (bson.Raw, *primitive.Timestamp, SourceStatus, error) {
	var doc struct {
		ResumeToken bson.Raw             `bson:"resume_token"`
		StartAt     *primitive.Timestamp `bson:"start_at"`
		Status      struct {
			Source SourceStatus `bson:"source"`
		} `bson:"capture_status"`
	}
	err := s.state.FindOne(ctx, bson.M{"_id": branchID}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, nil, SourceStatus{}, nil
	}
	if err != nil {
		return nil, nil, SourceStatus{}, fmt.Errorf("failed to load the source position: %w", err)
	}
	return doc.ResumeToken, doc.StartAt, doc.Status.Source, nil
}

// SourceStatusFor returns the stored status of a followed branch, whether or
// not a follower is running in this process.
func (s *Service) SourceStatusFor(ctx context.Context, branchID string) (*Status, error) {
	var doc struct {
		Status Status `bson:"capture_status"`
	}
	err := s.state.FindOne(ctx, bson.M{"_id": branchID}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc.Status, nil
}

// primaryTime is the primary's current cluster time. Nothing any node has
// majority-committed is later. It is read from hello: every replica set
// answers hello with operationTime, while some hosted deployments (Atlas
// free clusters) answer ping without it.
func primaryTime(ctx context.Context, db *mongo.Database) (primitive.Timestamp, error) {
	var reply bson.Raw
	if err := db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}, options.RunCmd().SetReadPreference(readpref.Primary())).Decode(&reply); err != nil {
		return primitive.Timestamp{}, fmt.Errorf("failed to read the source's cluster time: %w", err)
	}
	t, i, ok := reply.Lookup("operationTime").TimestampOK()
	if !ok {
		return primitive.Timestamp{}, &ConfigurationError{"the source's hello reply has no operationTime; a replica set is required"}
	}
	return primitive.Timestamp{T: t, I: i}, nil
}

// PrimaryTime exposes primaryTime to the online import, which starts its
// stream boundary from it.
func PrimaryTime(ctx context.Context, db *mongo.Database) (primitive.Timestamp, error) {
	return primaryTime(ctx, db)
}

// tokenTime reads the cluster time a change-stream resume token stands at.
// MongoDB documents resume tokens as opaque; their encoding has nevertheless
// started with the event's cluster time since 4.0.7, and the follower checks
// the decoded value against the server's clock before it relies on it.
func tokenTime(token bson.Raw) (primitive.Timestamp, bool) {
	data, ok := token.Lookup("_data").StringValueOK()
	if !ok || len(data) < 18 {
		return primitive.Timestamp{}, false
	}
	head, err := hex.DecodeString(data[:18])
	if err != nil || head[0] != 0x82 {
		return primitive.Timestamp{}, false
	}
	return primitive.Timestamp{T: binary.BigEndian.Uint32(head[1:5]), I: binary.BigEndian.Uint32(head[5:9])}, true
}
