package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// SourceHeartbeatInterval and SourceLeasePeriod control renewal and takeover.
var (
	SourceHeartbeatInterval = 5 * time.Second
	SourceLeasePeriod       = 60 * time.Second
)

const (
	sourceOwnerPath      = "capture_status.owner"
	sourceCleanStartPath = sourceOwnerPath + ".clean_start"
	sourceUnverifiedPath = "capture_status.unverified"
	leaseExpiredReason   = "lease_expired"
	missingOwnerReason   = "missing_owner"
)

var sourceLiveStates = []string{"starting", "running", "catching_up", "retrying", "resyncing"}

// SourceOwner identifies a run and when the metadata server last heard from it.
type SourceOwner struct {
	ID          string    `bson:"id" json:"-"`
	Kind        string    `bson:"kind" json:"kind"`
	Host        string    `bson:"host" json:"host"`
	PID         int       `bson:"pid" json:"pid"`
	StartedAt   time.Time `bson:"started_at" json:"started_at"`
	HeartbeatAt time.Time `bson:"heartbeat_at" json:"heartbeat_at"`
	CleanStart  bool      `bson:"clean_start,omitempty" json:"-"`
}

type unverifiedSource struct {
	Reason        string       `bson:"reason"`
	PreviousOwner *SourceOwner `bson:"previous_owner,omitempty"`
}

type sourceLease struct {
	branch      *wal.Branch
	id          string
	initialized bool
	prior       *Status
	verifyAt    primitive.Timestamp

	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	renewedAt time.Time
	err       error
}

func (l *sourceLease) filter() bson.M {
	return bson.M{"_id": l.branch.ID, sourceOwnerPath + ".id": l.id, "$expr": sourceLeaseFresh()}
}

func sourceLeaseFresh() bson.M {
	return bson.M{"$gt": bson.A{"$" + sourceOwnerPath + ".heartbeat_at",
		bson.M{"$subtract": bson.A{"$$NOW", SourceLeasePeriod.Milliseconds()}}}}
}

func (l *sourceLease) recordRenewal() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewedAt = time.Now()
}

func (l *sourceLease) renewal() (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renewedAt, l.err
}

func (l *sourceLease) revoke(err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = err
		l.cancel()
	}
	return l.err
}

func sourceOwnerLoss(owner *SourceOwner) string {
	if owner == nil {
		return "the source has no current lease owner; see the last saved position"
	}
	kind := "follower"
	if owner.Kind == "resync" {
		kind = "resync"
	}
	return fmt.Sprintf("the source %s on %s (pid %d) last renewed its lease at %s and has not been heard from since; see the last saved position",
		kind, owner.Host, owner.PID, owner.HeartbeatAt.UTC().Format(time.RFC3339))
}

// InitSourceState reserves an empty state document for a new import or a
// resync that must rebuild a missing source position.
func (s *Service) InitSourceState(ctx context.Context, branchID string) error {
	_, err := s.state.InsertOne(ctx, bson.M{"_id": branchID})
	return err
}

func (s *Service) acquireSourceLease(ctx context.Context, branchID, database, kind string) (*sourceLease, error) {
	id := primitive.NewObjectID().Hex()
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown host"
	}
	result, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		branch, prior, err := s.CaptureStatusFor(sc, branchID)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, &ConfigurationError{fmt.Sprintf("branch %s not found", branchID)}
			}
			return nil, err
		}
		if !branch.IsFollower() || branch.Follows != database {
			return nil, sourceDetached(branch.Name, database)
		}
		if prior.Health.Reason == ReasonCheckedOut {
			return nil, prior.Health.FollowAdmission()
		}
		// Decide admission and ownership at the same snapshot. An owner's
		// deadline can pass between this read and a subsequent command.
		if prior.facts.Owner == OwnerLive {
			return nil, prior.Owner.conflict(branch.ProjectID)
		}
		ownerKind := kind
		switch kind {
		case "follow":
			if err := prior.Health.FollowAdmission(); err != nil {
				return nil, err
			}
		case "import":
			if prior.Health.Kind != SourceImporting {
				return nil, &ConfigurationError{"online import replay requires an unpublished project"}
			}
			ownerKind = "follow"
		}
		lease := &sourceLease{branch: branch, id: id, prior: prior}
		if kind == "resync" && !prior.checkpointExists {
			// Rebuilding a missing checkpoint must serialize with detach or checkout.
			if err := s.branches.FollowerCheckpoint(sc, branchID, database, branch.HeadLSN); err != nil {
				return nil, followerWriteError(branch, err)
			}
			if err := s.InitSourceState(sc, branchID); err != nil {
				return nil, err
			}
			lease.initialized = true
		}

		// The branch and state must be read in the same snapshot. Checkout
		// can create a different state document after a detach deletes this one.
		filter := bson.M{"_id": branchID, "$or": bson.A{
			bson.M{sourceOwnerPath: bson.M{"$exists": false}},
			bson.M{"$expr": bson.M{"$not": bson.A{sourceLeaseFresh()}}},
		}}
		owner := bson.M{"id": id, "kind": ownerKind, "host": bson.M{"$literal": host}, "pid": os.Getpid(),
			"started_at": "$$NOW", "heartbeat_at": "$$NOW"}
		// Ownership alone cannot verify the previous run's checkpoint. Keep
		// the loss across attempts, including live reports left without an owner.
		previousOwner := "$" + sourceOwnerPath
		liveReport := bson.M{"$in": bson.A{"$capture_status.state", sourceLiveStates}}
		var nextState any = bson.M{"$cond": bson.A{liveReport, "starting", "$capture_status.state"}}
		if kind == "follow" && prior.Health.Kind == SourceHealthy {
			// Write-back must wait from acquisition until the stream opens.
			nextState = "starting"
			owner["clean_start"] = true
		}
		unverified := bson.M{"$cond": bson.A{
			bson.M{"$ne": bson.A{bson.M{"$ifNull": bson.A{previousOwner, nil}}, nil}},
			bson.M{"reason": leaseExpiredReason, "previous_owner": previousOwner},
			bson.M{"$ifNull": bson.A{"$" + sourceUnverifiedPath, bson.M{"$cond": bson.A{
				liveReport,
				bson.M{"reason": missingOwnerReason}, "$$REMOVE",
			}}}},
		}}
		err = s.state.FindOneAndUpdate(sc, filter, mongo.Pipeline{{{Key: "$set", Value: bson.M{
			sourceOwnerPath: owner, sourceUnverifiedPath: unverified,
			"capture_status.state": nextState,
		}}}}).Err()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, err
		}
		if prior.Owner != nil {
			return nil, prior.Owner.conflict(branch.ProjectID)
		}
		return nil, sourcePositionError(branch.Name)
	})
	if err != nil {
		return nil, err
	}
	lease := result.(*sourceLease)
	// A revocation cancels metadata work too. An ordinary stop can still
	// finish storing complete groups from the stream before releasing.
	lease.ctx, lease.cancel = context.WithCancel(context.Background())
	lease.recordRenewal()
	return lease, nil
}

func (o *SourceOwner) conflict(project string) *ConfigurationError {
	return &ConfigurationError{fmt.Sprintf("source %s is already running for project %s on %s (pid %d) since %s",
		o.Kind, project, o.Host, o.PID, o.StartedAt.UTC().Format(time.RFC3339))}
}

func sourceDetached(branch, database string) *ConfigurationError {
	return &ConfigurationError{fmt.Sprintf("branch %s no longer follows database %q (it was detached, rebound or removed)", branch, database)}
}

func sourceCheckoutError(branch string) *CaptureError {
	return &CaptureError{Message: fmt.Sprintf("followed branch %s was checked out by another process. Upgrade every process that uses this metadata database, stop writers to the branch's physical database, run `argon release`, then run `argon source resync`", branch)}
}

func sourcePositionError(branch string) *CaptureError {
	return &CaptureError{Message: fmt.Sprintf("the saved source position of branch %s is missing or was replaced by another process. Upgrade every process that uses this metadata database, then run `argon source resync`", branch)}
}

func (s *Service) sourceLeaseMiss(ctx context.Context, lease *sourceLease) error {
	branch, status, err := s.CaptureStatusFor(ctx, lease.branch.ID)
	if errors.Is(err, mongo.ErrNoDocuments) || (err == nil && branch.Follows != lease.branch.Follows) {
		return sourceDetached(lease.branch.Name, lease.branch.Follows)
	}
	if err != nil {
		return err
	}
	if status.Health.Reason == ReasonCheckedOut {
		return status.Health.FollowAdmission()
	}
	if !status.checkpointExists {
		return &ConfigurationError{fmt.Sprintf("the checkpoint for branch %s was removed while this run held the lease", branch.Name)}
	}
	// A legacy checkout replaces the position before publishing the physical
	// database. This is observable even while that checkout is still running.
	if status.Owner == nil && status.Health.Reason == ReasonPositionMissing {
		return sourcePositionError(branch.Name)
	}
	if status.Owner != nil && status.Owner.ID == lease.id {
		return lease.revoke(&ConfigurationError{fmt.Sprintf("the source lease for branch %s expired; run `argon source follow` again", branch.Name)})
	}
	return &ConfigurationError{fmt.Sprintf("another process took over following branch %s for project %s", branch.Name, branch.ProjectID)}
}

func (s *Service) updateSourceLease(ctx context.Context, lease *sourceLease, update bson.M) error {
	if _, err := lease.renewal(); err != nil {
		return err
	}
	result, err := s.state.UpdateOne(ctx, lease.filter(), update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return s.sourceLeaseMiss(ctx, lease)
	}
	return nil
}

func (s *Service) renewSourceLease(ctx context.Context, lease *sourceLease) error {
	return s.updateSourceLease(ctx, lease, bson.M{"$currentDate": bson.M{sourceOwnerPath + ".heartbeat_at": true}})
}

func (s *Service) refreshSourceLease(ctx context.Context, lease *sourceLease) error {
	if err := s.renewSourceLease(ctx, lease); err != nil {
		return err
	}
	lease.recordRenewal()
	return nil
}

func (s *Service) releaseSourceLease(lease *sourceLease) {
	defer lease.cancel()
	if _, err := lease.renewal(); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if lease.initialized {
		// A resync that never reached its marker leaves no state behind.
		unused := lease.filter()
		unused["capture_status.source"] = bson.M{"$exists": false}
		if deleted, err := s.state.DeleteOne(ctx, unused); err == nil && deleted.DeletedCount != 0 {
			return
		}
	}
	_, _ = s.state.UpdateOne(ctx, lease.filter(), bson.M{"$unset": bson.M{sourceOwnerPath: ""}})
}

// keepSourceLease renews ownership even during slow source reads or retries.
func (s *Service) keepSourceLease(ctx context.Context, run *running) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		timer := time.NewTimer(SourceHeartbeatInterval)
		defer timer.Stop()
		var renewalFailure error
		stop := func(err error) {
			done <- run.lease.revoke(err)
			run.cancel()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				last, _ := run.lease.renewal()
				remaining := time.Until(last.Add(SourceLeasePeriod))
				if renewalFailure != nil && remaining <= 0 {
					stop(&ConfigurationError{fmt.Sprintf("could not renew the source lease on the metadata deployment for %s; run `argon source follow` again: %v", SourceLeasePeriod, renewalFailure)})
					return
				}
				timeout := min(5*time.Second, remaining)
				if timeout <= 0 {
					// A suspended process must ask the metadata server whether
					// its owner is still current before it can renew.
					timeout = min(5*time.Second, SourceHeartbeatInterval)
				}
				beatCtx, cancel := context.WithTimeout(ctx, timeout)
				err := s.refreshSourceLease(beatCtx, run.lease)
				cancel()
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					renewalFailure = nil
					timer.Reset(SourceHeartbeatInterval)
					continue
				}
				var lost *ConfigurationError
				var gap *CaptureError
				if errors.As(err, &lost) || errors.As(err, &gap) {
					stop(err)
					return
				}
				renewalFailure = err
				last, _ = run.lease.renewal()
				timer.Reset(min(SourceHeartbeatInterval, time.Until(last.Add(SourceLeasePeriod))))
			}
		}
	}()
	return done
}

func (s *Service) reportSource(ctx context.Context, lease *sourceLease, fields bson.M, state string, admitted *Verdict, stored *Status) error {
	if _, err := lease.renewal(); err != nil {
		return err
	}
	// Starting a stream or canceling its open cannot clear a capture error.
	// A successful open reports running or catching_up and clears it there.
	if (state == "starting" || state == "stopped") && fields["capture_status.error"] == "" {
		delete(fields, "capture_status.error")
	}
	for field, value := range fields {
		literal := bson.M{"$literal": value}
		fields[field] = literal
		if state != "degraded" && (field == "capture_status.state" || field == "capture_status.error") {
			// Keep the known gap visible until a healthy checkpoint replaces it.
			fields[field] = bson.M{"$cond": bson.A{
				bson.M{"$eq": bson.A{"$capture_status.state", "degraded"}}, "$" + field, literal,
			}}
		}
	}
	fields[sourceCleanStartPath] = "$$REMOVE"
	if state == "starting" && admitted != nil && admitted.Kind == SourceHealthy {
		fields[sourceCleanStartPath] = true
	}
	var doc struct {
		Status Status `bson:"capture_status"`
	}
	err := s.state.FindOneAndUpdate(ctx, lease.filter(), mongo.Pipeline{{{Key: "$set", Value: fields}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return s.sourceLeaseMiss(ctx, lease)
	}
	*stored = doc.Status
	return err
}
