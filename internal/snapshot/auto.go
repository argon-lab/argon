package snapshot

import (
	"context"
	"fmt"
	"log"

	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// AutoConfig tunes threshold-based automatic snapshotting.
type AutoConfig struct {
	// Threshold is how many LSNs a branch head may advance past its newest
	// snapshot before a new snapshot is taken.
	Threshold int64
	// CheckEvery throttles how often MaybeSnapshot actually consults the
	// database: only every Nth call per branch performs the check.
	CheckEvery int
	// Synchronous runs snapshot creation inline instead of in a goroutine.
	// Used by one-shot commands and tests; long-running servers normally
	// keep snapshot creation off the write path.
	Synchronous bool
}

// DefaultAutoConfig snapshots roughly every 1000 entries per branch,
// checking the database at most every 64 writes.
func DefaultAutoConfig() AutoConfig {
	return AutoConfig{Threshold: 1000, CheckEvery: 64}
}

// autoState is the per-branch throttle/in-flight bookkeeping.
type autoState struct {
	callsSinceCheck int
	inFlight        bool
}

// EnableAuto turns on threshold-based auto-snapshotting; MaybeSnapshot is a
// no-op until this is called.
func (s *Service) EnableAuto(cfg AutoConfig) {
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultAutoConfig().Threshold
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = DefaultAutoConfig().CheckEvery
	}
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	if s.autoStopping {
		return
	}
	if s.autoContext == nil {
		s.autoContext, s.autoCancel = context.WithCancel(context.Background())
	}
	s.autoCfg = &cfg
	if s.autoBranches == nil {
		s.autoBranches = make(map[string]*autoState)
	}
}

// MaybeSnapshot implements the driver's auto-snapshot hook: called after
// writes with the (freshly advanced) branch. It is cheap by design — most
// calls only bump an in-memory counter; every CheckEvery-th call per branch
// consults the newest snapshot LSN and, when the head has advanced past the
// threshold, kicks off snapshot creation (asynchronously unless configured
// otherwise). Failures are logged, never surfaced to the write path: a
// missed snapshot only means bounded-replay starts further back.
func (s *Service) MaybeSnapshot(branch *wal.Branch) {
	s.autoMu.Lock()
	cfg := s.autoCfg
	if cfg == nil || s.autoStopping {
		s.autoMu.Unlock()
		return
	}
	st := s.autoBranches[branch.ID]
	if st == nil {
		st = &autoState{}
		s.autoBranches[branch.ID] = st
	}
	st.callsSinceCheck++
	if st.callsSinceCheck < cfg.CheckEvery || st.inFlight {
		s.autoMu.Unlock()
		return
	}
	st.callsSinceCheck = 0
	st.inFlight = true
	if s.autoRunning == 0 {
		s.autoIdle = make(chan struct{})
	}
	s.autoRunning++
	ctx := s.autoContext
	branchID := branch.ID
	s.autoMu.Unlock()

	release := func() {
		s.autoMu.Lock()
		st.inFlight = false
		s.autoRunning--
		if s.autoRunning == 0 {
			close(s.autoIdle)
		}
		s.autoMu.Unlock()
	}

	run := func() {
		defer release()
		if err := s.snapshotIfStale(ctx, branchID, cfg.Threshold); err != nil {
			log.Printf("auto-snapshot for branch %s failed: %v", branchID, err)
		}
	}

	if cfg.Synchronous {
		run()
	} else {
		go run()
	}
}

// snapshotIfStale creates a snapshot when the branch head has advanced more
// than threshold LSNs past the newest existing snapshot (or past the fork
// point when the branch has none).
func (s *Service) snapshotIfStale(ctx context.Context, branchID string, threshold int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Re-read the branch: the caller's copy may lag, and snapshotting at a
	// stale head is wasted work.
	fresh, err := s.branches.GetBranchByIDAny(branchID)
	if err != nil {
		return err
	}

	baseline := fresh.BaseLSN
	var newest Snapshot
	err = s.manifests.FindOne(ctx,
		bson.M{"branch_id": fresh.ID},
		options.FindOne().SetSort(bson.M{"lsn": -1}),
	).Decode(&newest)
	switch err {
	case nil:
		if newest.LSN > baseline {
			baseline = newest.LSN
		}
	case mongo.ErrNoDocuments:
		// No snapshot yet; baseline stays at the fork point.
	default:
		return err
	}

	if fresh.HeadLSN-baseline < threshold {
		return nil
	}
	_, err = s.CreateSnapshot(ctx, fresh.ID, fresh.HeadLSN)
	return err
}

// WaitAuto permanently stops admission of automatic snapshots and waits for
// every already registered task, including synchronous work, to return. A nil
// result means all tasks, including their deferred lock cleanup, have returned.
// Storage/cleanup failures are logged by the worker. Call it
// after stopping request/capture producers and before disconnecting MongoDB.
//
// The context bounds the wait. On timeout cooperative storage I/O is canceled,
// but callers receive an error: uncooperative or unavailable storage may still
// require abandoned-lock recovery if the process exits before cleanup finishes.
// A later WaitAuto call can wait for that cleanup without admitting new work.
func (s *Service) WaitAuto(ctx context.Context) error {
	s.autoMu.Lock()
	s.autoStopping = true
	idle := s.autoIdle
	cancel := s.autoCancel
	if s.autoRunning == 0 {
		s.autoMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
	s.autoMu.Unlock()
	select {
	case <-idle:
		if cancel != nil {
			cancel()
		}
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return fmt.Errorf("automatic snapshot shutdown did not finish; storage cancellation was requested and publication lock cleanup may still be pending: %w", ctx.Err())
	}
}
