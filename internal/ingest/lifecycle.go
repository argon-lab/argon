package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CaptureError marks a gap that retries cannot repair. Its event is never
// checkpointed, so an incomplete history cannot masquerade as healthy capture.
type CaptureError struct{ Message string }

func (e *CaptureError) Error() string { return e.Message }

type ConfigurationError struct{ Message string }

func (e *ConfigurationError) Error() string { return e.Message }

const barrierCollection = "__argon_capture_barriers"

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
}

type running struct {
	ready     chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
	readyOnce sync.Once
	err       error
	actor     string
	barriers  map[string]chan struct{}
}

func (s *Service) register(ctx context.Context, branchID string, cfg runConfig) (*running, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run := s.runs[branchID]; run != nil {
		return run, ctx, false
	}
	ctx, cancel := context.WithCancel(ctx)
	run := &running{ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel, actor: cfg.actor, barriers: make(map[string]chan struct{})}
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
	defer func() {
		state := "stopped"
		message := ""
		var gap *CaptureError
		if err != nil {
			message = err.Error()
			if errors.As(err, &gap) {
				state = "degraded"
			}
		}
		_ = s.report(branchID, state, s.currentActor(run), message)
		s.mu.Lock()
		run.err = err
		delete(s.runs, branchID)
		close(run.done)
		s.mu.Unlock()
		run.cancel()
	}()
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		if err = s.report(branchID, "starting", cfg.actor, ""); err == nil {
			err = s.runStream(ctx, branchID, cfg, run)
		}
		if ctx.Err() != nil {
			return err
		}
		var gap *CaptureError
		var command mongo.CommandError
		var configuration *ConfigurationError
		if errors.As(err, &command) && (command.Code == 260 || command.Code == 280 || command.Code == 286 || command.Code == 40573) {
			err = &CaptureError{fmt.Sprintf("capture cannot resume safely: %v", err)}
		}
		if err == nil || !retry || errors.As(err, &gap) || errors.As(err, &configuration) {
			return err
		}
		_ = s.report(branchID, "retrying", s.currentActor(run), err.Error())
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
	if run := s.runs[branchID]; run != nil {
		run.actor = status.Actor
	}
	s.statuses[branchID] = status
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if err := s.state.FindOneAndUpdate(ctx, bson.M{"_id": branchID}, bson.M{"$set": fields},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&stored); err != nil {
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
	branch, err := s.branches.GetBranchByID(branchID)
	if err != nil {
		return err
	}
	if !branch.IsLive() {
		return nil
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
