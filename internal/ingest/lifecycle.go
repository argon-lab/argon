package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CaptureError marks a gap that retries cannot repair. Its event is never
// checkpointed, so an incomplete history cannot masquerade as healthy capture.
type CaptureError struct {
	Message   string
	streamGap bool
}

func (e *CaptureError) Error() string { return e.Message }

// AutoResyncAllowed reports whether the source stream has a gap that can
// warrant an automatic resync.
func (e *CaptureError) AutoResyncAllowed() bool { return e.streamGap }

type ConfigurationError struct{ Message string }

func (e *ConfigurationError) Error() string { return e.Message }

// ErrUnfinishedResync indicates that the saved position belongs to a resync
// that must finish before ordinary following can resume.
var ErrUnfinishedResync = errors.New("unfinished resync")

type unfinishedResyncError struct{ *ConfigurationError }

func (e unfinishedResyncError) Is(target error) bool { return target == ErrUnfinishedResync }
func (e unfinishedResyncError) Unwrap() error        { return e.ConfigurationError }

const barrierCollection = "__argon_capture_barriers"

// maxStuckAttempts is how many times in a row a follower may fail on
// something other than the network without storing anything in between.
// With the backoff below that is about a minute.
const maxStuckAttempts = 12

// errCommitTimeout marks a follower's storing transaction that ran out of
// time; a timeout there is not the source being unreachable.
var errCommitTimeout = errors.New("storing the followed changes took too long")

// Status describes observed capture health, not merely a goroutine's existence.
type Status struct {
	BranchID       string    `bson:"branch_id" json:"branch_id"`
	State          string    `bson:"state" json:"state"`
	Actor          string    `bson:"actor" json:"actor"`
	Error          string    `bson:"error,omitempty" json:"error,omitempty"`
	UpdatedAt      time.Time `bson:"updated_at" json:"updated_at"`
	LastCapturedAt time.Time `bson:"last_captured_at,omitempty" json:"last_captured_at,omitempty"`
	LastEventAt    time.Time `bson:"last_event_at,omitempty" json:"last_event_at,omitempty"`
	LastEventLagMS int64     `bson:"last_event_lag_ms,omitempty" json:"last_event_lag_ms,omitempty"`
	HeadLSN        int64     `bson:"head_lsn,omitempty" json:"head_lsn,omitempty"`
	// Source is set for a branch that follows an external database.
	Source *SourceStatus `bson:"source,omitempty" json:"source,omitempty"`
	Owner  *SourceOwner  `bson:"owner,omitempty" json:"-"`
	// SourceUnverified records a lost owner until a replacement verifies a checkpoint.
	SourceUnverified *unverifiedSource `bson:"unverified,omitempty" json:"-"`
	// SourceLeaseExpired is evaluated against the metadata server's clock.
	SourceLeaseExpired bool `bson:"-" json:"-"`

	hasResumeToken         bool
	resumeToken            bson.Raw
	synthesizedDegradation bool
}

type running struct {
	ready     chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
	readyOnce sync.Once
	err       error
	// progress counts what a follower stored; see execute.
	progress      atomic.Int64
	actor         string
	barriers      map[string]chan struct{}
	lease         *sourceLease
	resyncStarted bool
}

func (s *Service) register(ctx context.Context, branchID string, cfg runConfig) (*running, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run := s.runs[branchID]; run != nil {
		return run, ctx, false
	}
	ctx, cancel := context.WithCancel(ctx)
	run := &running{ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel, actor: cfg.actor, barriers: make(map[string]chan struct{}), lease: cfg.lease}
	s.runs[branchID] = run
	return run, ctx, true
}

// Run captures until cancellation, retrying transient failures. Start provides
// the same recovery behavior with managed readiness, health and drain controls.
func (s *Service) Run(ctx context.Context, branchID string, opts ...RunOption) error {
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	run, runCtx, created := s.register(ctx, branchID, cfg)
	if !created {
		return fmt.Errorf("capture already running for branch %s", branchID)
	}
	return s.execute(runCtx, branchID, cfg, run, true)
}

// Start starts one managed ingester and waits for a durable stream checkpoint.
// The caller's context bounds readiness; the ingester then lives until Stop.
func (s *Service) Start(ctx context.Context, branchID string, opts ...RunOption) error {
	if err := s.start(ctx, branchID, opts...); err != nil {
		return err
	}
	return s.Drain(ctx, branchID)
}

func (s *Service) start(ctx context.Context, branchID string, opts ...RunOption) error {
	var cfg runConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	run, runCtx, created := s.register(context.Background(), branchID, cfg)
	if current := s.currentActor(run); !created && cfg.actor != "" && current != "" && current != cfg.actor {
		return fmt.Errorf("capture uses a different persisted actor; create a new sandbox for a new run label")
	}
	if created {
		go func() { _ = s.execute(runCtx, branchID, cfg, run, true) }()
	}
	select {
	case <-run.done:
		return s.runError(branchID, run)
	case <-run.ready:
		for {
			s.mu.Lock()
			status := s.statuses[branchID]
			s.mu.Unlock()
			if status.State == "running" {
				if cfg.actor != "" && s.currentActor(run) != cfg.actor {
					return fmt.Errorf("capture uses a different persisted actor; create a new sandbox for a new run label")
				}
				return nil
			}
			if status.State == "degraded" || status.State == "stopped" {
				return fmt.Errorf("capture %s: %s", status.State, status.Error)
			}
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-run.done:
				timer.Stop()
				return s.runError(branchID, run)
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	case <-ctx.Done():
		if created {
			run.cancel()
		}
		return fmt.Errorf("waiting for capture readiness: %w", ctx.Err())
	}
}

func (s *Service) execute(ctx context.Context, branchID string, cfg runConfig, run *running, retry bool) (err error) {
	var heartbeat <-chan error
	if run.lease != nil {
		heartbeat = s.keepSourceLease(ctx, run)
	}
	defer func() {
		run.cancel()
		if heartbeat != nil {
			if lost := <-heartbeat; lost != nil && !errors.Is(err, lost) {
				err = lost
			}
		}
		state := "stopped"
		message := ""
		var gap *CaptureError
		if err != nil {
			message = err.Error()
			if cfg.redact != nil {
				message = cfg.redact(message)
			}
			if errors.As(err, &gap) {
				state = "degraded"
			}
		}
		var leaseErr error
		if run.lease != nil {
			_, leaseErr = run.lease.renewal()
		}
		if leaseErr == nil && (!cfg.resync || run.resyncStarted) && !errors.Is(err, ErrUnfinishedResync) {
			reportErr := s.report(branchID, state, s.currentActor(run), message)
			var lost *ConfigurationError
			if run.lease != nil && errors.As(reportErr, &lost) {
				err = lost
			}
		}
		s.mu.Lock()
		run.err = err
		delete(s.runs, branchID)
		close(run.done)
		s.mu.Unlock()
	}()
	stuck, stored := 0, run.progress.Load()
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		if run.lease != nil {
			err = nil
		} else {
			err = s.report(branchID, "starting", cfg.actor, "")
		}
		if err == nil {
			if cfg.stream != nil {
				err = cfg.stream(ctx, run)
			} else {
				err = s.runStream(ctx, branchID, cfg, run)
			}
		}
		if ctx.Err() != nil {
			if errors.Is(err, ctx.Err()) {
				return nil
			}
			return err
		}
		var gap *CaptureError
		var command mongo.CommandError
		var configuration *ConfigurationError
		if run.lease == nil {
			err = streamCaptureError(err, false)
		}
		if err == nil || !retry || errors.As(err, &gap) || errors.As(err, &configuration) {
			return err
		}
		// Following retries for as long as retrying can help. It cannot when
		// the server answers the same position with an error every time: an
		// event too large to return, a privilege the user lacks, a
		// transaction too large to store. Such a follower would otherwise
		// report "retrying" forever, and an import would never return.
		if cfg.stream != nil {
			if now := run.progress.Load(); now != stored {
				stuck, stored = 0, now
			}
			switch {
			case errors.As(err, &command) && (command.Code == 10334 || command.Code == 13):
				return &CaptureError{Message: fmt.Sprintf("the source cannot be followed past this position: %v", err)}
			case !mongo.IsNetworkError(err) && !mongo.IsTimeout(err) || errors.Is(err, errCommitTimeout):
				if stuck++; stuck >= maxStuckAttempts {
					return &CaptureError{Message: fmt.Sprintf("following made no progress in %d attempts and has stopped; the last error was: %v", stuck, err)}
				}
			}
		}
		retryMessage := err.Error()
		if cfg.redact != nil {
			retryMessage = cfg.redact(retryMessage)
		}
		reportErr := s.report(branchID, "retrying", s.currentActor(run), retryMessage)
		if errors.As(reportErr, &configuration) {
			return configuration
		}
		delay := 5 * time.Second
		if attempt < 6 {
			delay = time.Duration(1<<attempt) * 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Running reports whether this process is capturing or following the branch.
func (s *Service) Running(branchID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[branchID] != nil
}

func (s *Service) currentActor(run *running) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return run.actor
}

func (s *Service) runError(branchID string, run *running) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.err != nil {
		return run.err
	}
	return fmt.Errorf("capture stopped for branch %s", branchID)
}

func (s *Service) report(branchID, state, actor, message string) error {
	status := Status{BranchID: branchID, State: state, Actor: actor, Error: message, UpdatedAt: time.Now()}
	s.mu.Lock()
	previous := s.statuses[branchID]
	status.LastCapturedAt, status.LastEventAt, status.LastEventLagMS, status.HeadLSN = previous.LastCapturedAt, previous.LastEventAt, previous.LastEventLagMS, previous.HeadLSN
	if status.Actor == "" {
		status.Actor = s.statuses[branchID].Actor
	}
	var lease *sourceLease
	if run := s.runs[branchID]; run != nil {
		run.actor = status.Actor
		lease = run.lease
	}
	if lease == nil {
		s.statuses[branchID] = status
	}
	s.mu.Unlock()
	base := context.Background()
	if lease != nil {
		base = lease.ctx
	}
	ctx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	// Lifecycle reports must not replace checkpoint metrics: a fresh process
	// starts with empty memory, and another watcher may have committed newer
	// progress. Updating only these fields also closes that cross-worker race.
	fields := bson.M{
		"capture_status.branch_id":  branchID,
		"capture_status.state":      state,
		"capture_status.error":      message,
		"capture_status.updated_at": status.UpdatedAt,
	}
	if actor != "" {
		fields["capture_status.actor"] = actor
	}
	var stored struct {
		Status Status `bson:"capture_status"`
	}
	var err error
	if lease != nil {
		err = s.reportSource(ctx, lease, fields, state, &stored.Status)
	} else {
		err = s.state.FindOneAndUpdate(ctx, bson.M{"_id": branchID}, bson.M{"$set": fields},
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&stored)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	if run := s.runs[branchID]; run != nil {
		run.actor = stored.Status.Actor
	}
	s.statuses[branchID] = stored.Status
	s.mu.Unlock()
	return nil
}

// Statuses includes failed ingesters, retaining the reason and last update.
func (s *Service) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]Status, 0, len(s.statuses))
	for _, status := range s.statuses {
		items = append(items, status)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].BranchID < items[j].BranchID })
	return items
}

// Drain waits until all writes preceding a marker on this database are durable
// in the WAL. Writers must be quiesced before a release; later writes are outside
// the barrier and must not race a destructive lifecycle operation.
func (s *Service) Drain(ctx context.Context, branchID string) error {
	branch, err := s.branches.GetBranchByIDContext(ctx, branchID)
	if err != nil {
		return err
	}
	if !branch.IsLive() {
		return nil
	}
	// The marker below is a write; it goes to the branch's own database or
	// nowhere.
	if _, err := branch.OwnDatabase(); err != nil {
		return err
	}
	if err := s.start(ctx, branchID); err != nil {
		return err
	}
	marker := primitive.NewObjectID().Hex()
	done := make(chan struct{})
	s.mu.Lock()
	run := s.runs[branchID]
	if run == nil {
		s.mu.Unlock()
		return fmt.Errorf("capture stopped before drain")
	}
	run.barriers[marker] = done
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(run.barriers, marker); s.mu.Unlock() }()
	coll := s.client.Database(branch.PhysicalDB).Collection(barrierCollection)
	if _, err := coll.InsertOne(ctx, bson.M{"_id": marker}); err != nil {
		return fmt.Errorf("capture barrier: %w", err)
	}
	// A competing watcher may commit this marker and move the shared resume
	// token past it. Its durable acknowledgement lets this caller finish even
	// when our local stream retries without seeing the marker again.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	complete := func() error {
		_, _ = coll.DeleteOne(ctx, bson.M{"_id": marker})
		return nil
	}
	for {
		select {
		case <-done:
			return complete()
		case <-ticker.C:
			if err := coll.FindOne(ctx, bson.M{"_id": marker, "captured": true}).Err(); err == nil {
				return complete()
			}
		case <-run.done:
			return s.runError(branchID, run)
		case <-ctx.Done():
			return fmt.Errorf("capture drain: %w", ctx.Err())
		}
	}
}

func (s *Service) ackBarrier(run *running, marker string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if done := run.barriers[marker]; done != nil {
		close(done)
		delete(run.barriers, marker)
	}
}

// Stop drains acknowledged writes before canceling and waiting for termination.
func (s *Service) Stop(ctx context.Context, branchID string) error {
	if err := s.Drain(ctx, branchID); err != nil {
		return err
	}
	s.mu.Lock()
	run := s.runs[branchID]
	s.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	select {
	case <-run.done:
		return run.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel stops a watcher without adding a barrier and waits for its current
// checkpoint transaction. Complete received groups persist; an unproven trailing
// transaction stays behind the resume boundary. Use for command exit or explicit
// discard; use Stop before releasing a database whose history must be drained.
func (s *Service) Cancel(ctx context.Context, branchID string) error {
	s.mu.Lock()
	run := s.runs[branchID]
	s.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var first error
	for _, id := range ids {
		if err := s.Stop(ctx, id); err != nil {
			if first == nil {
				first = err
			}
			s.mu.Lock()
			if run := s.runs[id]; run != nil {
				run.cancel()
			}
			s.mu.Unlock()
		}
	}
	return first
}
