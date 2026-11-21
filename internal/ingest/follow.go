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
	// Lookup returns the write-back that ran this source transaction, or nil.
	Lookup(ctx context.Context, branchID string, session primitive.Binary, txnNumber int64) (*WriteBack, error)
	// Settle marks the plan applied. ctx is the storing transaction.
	Settle(ctx context.Context, planID string) error
}

// FollowOptions configures Follow.
type FollowOptions struct {
	// AllowInexact lets the follower continue past an update whose exact
	// post-image the source can no longer serve, by storing the document's
	// current state instead and recording the affected range as inexact.
	// Without it such an update stops the follower as degraded.
	AllowInexact bool
	// OnReady runs once when the stream and any source preparation are ready.
	OnReady func()
	// WriteBacks is optional.
	WriteBacks WriteBackLedger
	// Redact cleans error text before it is stored or printed. The caller
	// knows the connection string; this package never sees it.
	Redact func(string) string
	// PrepareSource runs once with the lease held, after validating the saved
	// position and opening the source stream. A degraded follower must first
	// replay far enough to clear its stored gap. Preparation checks the lease
	// before each source alteration.
	PrepareSource func(ctx context.Context, checkLease func(context.Context) error) error
	// Watch replaces src.Watch. Tests use it to supply server errors that a
	// fresh replica set cannot be made to produce, such as a resume position
	// its oplog no longer holds.
	Watch func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error)
	// resync marks the stream that ends a resync, the only one allowed to
	// run while the branch's status says a resync is unfinished.
	resync bool
	// startAt is the owning resync's boundary, taken before its comparison.
	startAt *primitive.Timestamp
	// expectedHead is the head handed to replay by the comparison or copy.
	// A committed replay checkpoint advances it for the next attempt.
	expectedHead *int64
	// Publish full source copies only with the completed resync checkpoint.
	baselines []wal.CollectionBaseline
}

// Follow applies the source database's changes to a branch until ctx ends,
// resuming from the position saved with the last applied change. Transient
// failures are retried; a gap that cannot be repaired stops it and is
// reported as degraded.
func (s *Service) Follow(ctx context.Context, branchID string, src *mongo.Database, opts FollowOptions) error {
	lease, err := s.acquireSourceLease(ctx, branchID, src.Name(), "follow")
	if err != nil {
		return redacted(err, opts.Redact)
	}
	defer s.releaseSourceLease(lease)
	if prepare := opts.PrepareSource; prepare != nil {
		opts.PrepareSource = func(ctx context.Context, checkLease func(context.Context) error) error {
			if err := prepare(ctx, checkLease); err != nil {
				return err
			}
			opts.PrepareSource = nil
			return nil
		}
	}
	cfg := runConfig{actor: sourceActor, redact: opts.Redact, lease: lease}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return &ConfigurationError{fmt.Sprintf("source follow is already running for branch %s in this process", branchID)}
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
func (s *Service) CatchUp(ctx context.Context, branchID string, src *mongo.Database, startAt primitive.Timestamp, expectedHead, firstLSN int64, reason string, redact func(string) string) (*ConsistentPoint, error) {
	lease, err := s.acquireSourceLease(ctx, branchID, src.Name(), "import")
	if err != nil {
		return nil, redacted(err, redact)
	}
	defer s.releaseSourceLease(lease)
	cfg := runConfig{actor: sourceActor, redact: redact, lease: lease}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return nil, &ConfigurationError{fmt.Sprintf("source follow is already running for branch %s in this process", branchID)}
	}
	var point *ConsistentPoint
	var initialized bool
	cfg.stream = func(ctx context.Context, run *running) error {
		if !initialized {
			status := SourceStatus{Database: src.Name(), Mode: ModeCatchUp, Position: startAt, InexactFromLSN: firstLSN, InexactReason: reason}
			if err := s.updateSourceLease(ctx, run.lease, bson.M{"$set": bson.M{
				"start_at": startAt, "actor": sourceActor, "capture_status.source": sourceCheckpoint{status, positionSHA256(nil, &startAt)},
			}}); err != nil {
				return fmt.Errorf("failed to persist the source stream boundary: %w", err)
			}
			initialized = true
		}
		reached, err := s.followStream(ctx, branchID, src, FollowOptions{AllowInexact: true, expectedHead: &expectedHead}, run, true)
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
	s       *Service
	branch  *wal.Branch
	src     *mongo.Database
	session mongo.Session
	opts    FollowOptions
	status  SourceStatus

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

	// Change events for a transaction are contiguous. Its first lookup also
	// covers its other events, including when the transaction has no claim.
	lastTransaction struct {
		session string
		number  int64
		claim   *WriteBack
	}
	lastCommit time.Time
	lastToken  bson.Raw
	recoverAt  primitive.Timestamp
	unverified bool
	// waitingReported is set once the status records that received changes
	// wait for the source's next oplog entry; the next commit updates or
	// clears that record.
	waitingReported bool
}

func (s *Service) followStream(ctx context.Context, branchID string, src *mongo.Database, opts FollowOptions, run *running, untilConsistent bool) (*ConsistentPoint, error) {
	branch, saved, err := s.CaptureStatusFor(ctx, branchID)
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
	token, startAt := saved.resumeToken, saved.startAt
	if untilConsistent {
		// Only the importing copy or the owning resync may replay its own
		// boundary. Fence the handoff against the head that writer produced.
		if opts.expectedHead == nil {
			return nil, &ConfigurationError{"source replay requires the copy or comparison head"}
		}
		if *opts.expectedHead != branch.HeadLSN {
			return nil, sourceHeadMismatch(branch, *opts.expectedHead)
		}
		if saved.facts.CheckedOut {
			return nil, sourceCheckoutError(branch.Name)
		}
		if opts.startAt != nil {
			token, startAt = nil, opts.startAt
		}
		if saved.Source == nil || saved.Source.Database != src.Name() || (len(token) == 0 && startAt == nil) {
			return nil, sourcePositionError(branch.Name)
		}
	} else if err := saved.Health.FollowAdmission(); err != nil {
		return nil, err
	}
	status := *saved.Source
	// Keep reporting a capture error until the source stream opens successfully.
	if !opts.resync && (saved.facts.State != FollowerRetrying || !saved.facts.HasError) {
		if err := s.report(branchID, "starting", sourceActor, "", saved.Health); err != nil {
			return nil, err
		}
	}
	status.Database = src.Name()

	csOpts := sourceStreamOptions()
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
		watch = watchSource
	}
	stream, err := watch(ctx, src, csOpts)
	if err != nil {
		if unsupportedSourceStreamOptions(err) {
			return nil, &ConfigurationError{fmt.Sprintf("the source's MongoDB version does not support the change-stream options Argon follows with: %v", err)}
		}
		return nil, streamCaptureError(fmt.Errorf("failed to open the change stream on source database %s: %w", src.Name(), err), true)
	}
	defer func() { _ = stream.Close(context.Background()) }()

	now, err := primaryTime(ctx, src)
	if err != nil {
		return nil, err
	}
	if err := validateSourceStreamToken(stream, now); err != nil {
		return nil, err
	}

	session, err := src.Client().StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(context.Background())

	f := &follower{s: s, branch: branch, src: src, session: session, opts: opts, status: status,
		lastCommit: time.Now(), opened: opened,
		unpublished: untilConsistent && !opts.resync, run: run, unverified: saved.facts.Unverified}
	if saved.facts.State == FollowerDegraded && !opts.resync {
		f.recoverAt = now
	}
	if f.unverified && !opts.resync && run.lease.verifyAt.IsZero() {
		// A stream retry keeps the verification boundary of this owner.
		run.lease.verifyAt = now
	}
	if f.status.Mode == ModeCatchUp {
		// Every document read for this catch-up so far happened before now.
		f.need = now
	}

	if err := s.report(branchID, f.stateName(), sourceActor, "", nil); err != nil {
		return nil, err
	}
	ready := func() {
		run.readyOnce.Do(func() {
			close(run.ready)
			if opts.OnReady != nil {
				opts.OnReady()
			}
		})
	}
	if opts.PrepareSource == nil {
		ready()
	}

	for {
		if ctx.Err() != nil {
			// Complete cluster times are stored; the one still open is read
			// again after the saved position.
			return nil, f.flushReady()
		}
		if opts.PrepareSource != nil && f.recoverAt.IsZero() {
			if _, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
				if err := s.renewSourceLease(sc, run.lease); err != nil {
					return nil, err
				}
				return nil, s.branches.FollowerCheckpoint(sc, branch.ID, branch.Follows, branch.HeadLSN)
			}); err != nil {
				return nil, followerWriteError(branch, err)
			}
			run.lease.recordRenewal()
			checkLease := func(ctx context.Context) error { return s.refreshSourceLease(ctx, run.lease) }
			if err := opts.PrepareSource(ctx, checkLease); err != nil {
				if _, lost := run.lease.renewal(); lost != nil && !errors.Is(err, lost) {
					return nil, fmt.Errorf("%w: %v", lost, err)
				}
				var configuration *ConfigurationError
				if errors.As(err, &configuration) {
					return nil, err
				}
				return nil, &ConfigurationError{err.Error()}
			}
			opts.PrepareSource = nil
			ready()
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
			return nil, streamCaptureError(fmt.Errorf("change stream error: %w", err), true)
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
			// Any later write on the deployment closes this cluster time.
			// On an idle source the next oplog entry takes one to two idle
			// check intervals (ten to twenty seconds by default). The
			// reported time to store the change includes this wait.
			if len(f.ready) > 0 {
				if point, err := f.commitAndCheck(ctx, f.readyToken, f.readyAt); err != nil {
					return nil, err
				} else if point != nil && untilConsistent {
					return point, nil
				}
			}
			if len(f.pending) > 0 && !f.waitingReported {
				if err := f.reportWaiting(ctx); err != nil {
					if ctx.Err() != nil {
						return nil, f.flushReady()
					}
					return nil, err
				}
			}
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

func streamCaptureError(err error, followedSource bool) error {
	var command mongo.CommandError
	if errors.As(err, &command) {
		switch command.Code {
		case 260, 280, 286, 40573:
			return &CaptureError{Message: fmt.Sprintf("capture cannot resume safely: %v", err), streamGap: followedSource}
		case 10334:
			if followedSource {
				return &CaptureError{Message: fmt.Sprintf("the source cannot be followed past this position: %v", err), streamGap: true}
			}
		}
	}
	return err
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

// waitingSince is when the oldest received change that is not yet stored
// happened on the source. It waits for the stream to pass its cluster time.
func (f *follower) waitingSince() (time.Time, bool) {
	if !f.pendingOpen || len(f.pending) == 0 {
		return time.Time{}, false
	}
	if wall, ok := f.pending[0].Metadata["capture_wall_time"].(time.Time); ok {
		return wall, true
	}
	return time.Now(), true
}

// reportWaiting records in the status that received changes wait for the
// source's next oplog entry, so that `argon source status` can say why the
// head has not moved. It writes once per wait; commit updates or clears it.
func (f *follower) reportWaiting(ctx context.Context) error {
	since, waiting := f.waitingSince()
	if !waiting {
		return nil
	}
	if err := f.s.updateSourceLease(ctx, f.run.lease, bson.M{"$set": bson.M{waitingSincePath: since}}); err != nil {
		return fmt.Errorf("failed to record the change that waits for the source's next write: %w", err)
	}
	f.waitingReported = true
	f.s.mu.Lock()
	current := f.s.statuses[f.branch.ID]
	current.WaitingSince = since
	f.s.statuses[f.branch.ID] = current
	f.s.mu.Unlock()
	return nil
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
	if err := wal.UnmarshalDocument(raw, &event); err != nil {
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
// write-back ran. The driver may reuse a session across many transactions.
func (f *follower) writeBack(ctx context.Context, event *changeEvent) (*WriteBack, error) {
	if f.opts.WriteBacks == nil || len(event.LSID) == 0 {
		return nil, nil
	}
	subtype, data, ok := event.LSID.Lookup("id").BinaryOK()
	if !ok {
		return nil, nil
	}
	key := string(event.LSID)
	if f.lastTransaction.session == key && f.lastTransaction.number == event.TxnNumber {
		return f.lastTransaction.claim, nil
	}
	wb, err := f.opts.WriteBacks.Lookup(ctx, f.branch.ID, primitive.Binary{Subtype: subtype, Data: data}, event.TxnNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to check a source transaction against pending write-backs: %w", err)
	}
	f.lastTransaction.session, f.lastTransaction.number, f.lastTransaction.claim = key, event.TxnNumber, wb
	return wb, nil
}

func (f *follower) convert(ctx context.Context, raw bson.Raw, event *changeEvent) (*wal.Entry, error) {
	switch event.OperationType {
	case "insert", "update", "replace", "delete":
	case "drop", "dropDatabase", "rename":
		if event.OperationType == "drop" && (raw.Lookup("collectionUUID").Type == 0 || source.SkipCollection(event.NS.Collection)) {
			// Expanded view drops have no collection UUID. Views and
			// system collections are outside the followed data.
			return nil, nil
		}
		return nil, &CaptureError{Message: fmt.Sprintf("%s of %s on the source is not followed: collection-level changes carry no documents, so the branch would stop matching the source; history is incomplete from here. Run `argon source resync` to bring the branch to the source's current state and follow it from there", event.OperationType, event.NS.Collection), streamGap: true}
	case "invalidate":
		return nil, &CaptureError{Message: "the source change stream was invalidated (database dropped?); the branch cannot follow it past this point. Run `argon source resync` to bring the branch to the source's current state", streamGap: true}
	case "create":
		// A time-series collection stores its data in system.buckets.*,
		// which a change stream does not deliver as documents. The import
		// refuses a source that has one; a follower that passed its creation
		// would go on reporting the branch as equal to a source that holds
		// data the branch can never receive. A view has no data of its own
		// and is not followed.
		if on, ok := event.OperationDescription.Lookup("viewOn").StringValueOK(); ok && strings.HasPrefix(on, "system.buckets.") {
			return nil, &CaptureError{Message: fmt.Sprintf("time-series collection %s was created on the source; its measurements cannot be followed, so the branch no longer equals the source from here. A source with time-series collections cannot be followed", event.NS.Collection)}
		}
		return nil, nil
	case "createIndexes", "dropIndexes":
		if source.SkipCollection(event.NS.Collection) {
			return nil, nil
		}
		return nil, f.s.recordIndexEvent(ctx, f.branch, event)
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
		if HasDuplicateFields(post) {
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
		return nil, false, &CaptureError{Message: fmt.Sprintf("update on %s carries no document key; history is incomplete", event.NS.Collection), streamGap: true}
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
				return nil, false, &CaptureError{Message: fmt.Sprintf("exact post-image unavailable for an update on %s: %v. This is not a matter of keeping up: the source will not serve this collection at the update's cluster time. That is so for a capped collection on MongoDB 7.0 and earlier, and on MongoDB 6.0 for updates made before the collection was last altered (an index build, a collMod such as enabling images, a rename or a drop). Such an update can only be followed exactly when it carries its own image, which changeStreamPreAndPostImages provides for updates made after it is enabled. The branch is not advanced past this change. Restart following with --allow-inexact to apply it with the document's current state and record the range as inexact history, or run `argon source resync`", event.NS.Collection, err), streamGap: true}
			}
			return nil, false, &CaptureError{Message: fmt.Sprintf("exact post-image unavailable for an update on %s: %v. The branch is not advanced past this change. Either enable changeStreamPreAndPostImages on the source collections so updates carry their own images, or restart following with --allow-inexact to converge on the source's current state and record the range in between as inexact history", event.NS.Collection, err), streamGap: true}
		}
		if f.status.Mode != ModeCatchUp {
			f.status.Mode = ModeCatchUp
			f.status.InexactFromLSN = f.branch.HeadLSN + 1
			f.status.InexactReason = "updates older than the source's snapshot history were applied with the document's later state"
			if unsupported {
				f.status.InexactReason = fmt.Sprintf("updates to %s, which the source does not serve at a past cluster time, were applied with the document's later state", event.NS.Collection)
			}
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
			// Adjacent ranges with the same reason can share one entry.
			finished.FromLSN = ranges[n-1].FromLSN
			ranges = ranges[:n-1]
		}
		// A recurring reason should displace its own older ranges first.
		for len(ranges) >= maxInexactRanges {
			drop := 0
			for i, older := range ranges {
				if older.Reason == finished.Reason {
					drop = i
					break
				}
			}
			copy(ranges[drop:], ranges[drop+1:])
			ranges = ranges[:len(ranges)-1]
			f.status.InexactRangesDropped++
		}
		f.status.InexactRanges = append(ranges, finished)
	}
	if err := f.commit(token, at); err != nil {
		f.status = previous
		return nil, err
	}
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
	ctx, cancel := context.WithTimeout(f.run.lease.ctx, 30*time.Second)
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
	waitingSince, waiting := f.waitingSince()
	// A restarted follower clears a stored gap only after replay reaches the
	// source time it observed on opening. A gap that still needs a resync
	// stops replay before then; an old report or a repaired gap does not.
	recovered := !at.Before(f.recoverAt)
	// A replacement verifies only a complete, exact checkpoint. Resync has
	// its own replay boundary and reaches exact mode only at completion.
	verified := f.unverified && !at.Before(f.run.lease.verifyAt) && status.Mode == ModeExact && status.Resync == nil
	_, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if err := s.renewSourceLease(sc, f.run.lease); err != nil {
			return nil, err
		}
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
		if len(f.opts.baselines) > 0 && status.Mode == ModeExact {
			if err := s.branches.PublishCollectionBaselines(sc, branch.ID, branch.Follows, head, f.opts.baselines); err != nil {
				return nil, err
			}
		}
		if status.Mode == ModeExact {
			status.Consistent = &ConsistentPoint{LSN: head, ClusterTime: at, At: capturedAt}
		}
		fields := bson.M{
			"resume_token": token, "updated_at": capturedAt,
			"capture_status.last_captured_at": capturedAt, "capture_status.head_lsn": head,
			"capture_status.source": sourceCheckpoint{status, positionSHA256(token, nil)},
		}
		if recovered {
			fields["capture_status.state"] = f.stateName()
			fields["capture_status.error"] = ""
		}
		if !eventAt.IsZero() {
			fields["capture_status.last_event_at"] = eventAt
			fields["capture_status.last_event_lag_ms"] = lagMS
		}
		unset := bson.M{sourceCleanStartPath: ""}
		if f.waitingReported {
			if waiting {
				fields[waitingSincePath] = waitingSince
			} else {
				unset[waitingSincePath] = ""
			}
		}
		if verified {
			unset[sourceUnverifiedPath] = ""
		}
		update := bson.M{"$set": fields, "$unset": unset}
		if err := s.updateSourceLease(sc, f.run.lease, update); err != nil {
			return nil, fmt.Errorf("failed to persist the source position: %w", err)
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
	if f.opts.expectedHead != nil {
		*f.opts.expectedHead = head
	}
	f.run.lease.recordRenewal()
	f.run.progress.Add(1)
	if recovered {
		f.recoverAt = primitive.Timestamp{}
	}
	if verified {
		f.unverified = false
	}
	f.status = status
	f.lastCommit, f.lastToken = capturedAt, append(f.lastToken[:0], token...)
	s.mu.Lock()
	current := s.statuses[branch.ID]
	if recovered {
		current.State, current.Error = f.stateName(), ""
	}
	if verified {
		current.SourceUnverified = nil
	}
	current.LastCapturedAt, current.HeadLSN = capturedAt, head
	if !eventAt.IsZero() {
		current.LastEventAt, current.LastEventLagMS = eventAt, lagMS
	}
	if f.waitingReported {
		current.WaitingSince = time.Time{}
		if waiting {
			current.WaitingSince = waitingSince
		}
		f.waitingReported = waiting
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
	return nil
}

// followerWriteError reports when the branch has changed under a follower.
func followerWriteError(branch *wal.Branch, err error) error {
	switch {
	case errors.Is(err, branchwal.ErrFollowerCheckedOut):
		return sourceCheckoutError(branch.Name)
	case errors.Is(err, branchwal.ErrNoLongerFollows):
		return sourceDetached(branch.Name, branch.Follows)
	case errors.Is(err, branchwal.ErrBranchChanged):
		return &CaptureError{Message: fmt.Sprintf("the head of branch %s moved without this follower. Something else wrote to the branch and it no longer equals the source: make sure every process that uses this metadata database is current, then run `argon source resync`", branch.Name)}
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", errCommitTimeout, err)
	}
	return err
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
