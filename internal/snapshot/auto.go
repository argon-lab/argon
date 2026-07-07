package snapshot

import (
	"context"
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
	// Meant for tests; production keeps snapshotting off the write path.
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
	if cfg == nil {
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
	s.autoMu.Unlock()

	release := func() {
		s.autoMu.Lock()
		st.inFlight = false
		s.autoMu.Unlock()
	}

	run := func() {
		defer release()
		if err := s.snapshotIfStale(branch, cfg.Threshold); err != nil {
			log.Printf("auto-snapshot for branch %s failed: %v", branch.ID, err)
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
func (s *Service) snapshotIfStale(branch *wal.Branch, threshold int64) error {
	ctx := context.Background()

	// Re-read the branch: the caller's copy may lag, and snapshotting at a
	// stale head is wasted work.
	fresh, err := s.branches.GetBranchByIDAny(branch.ID)
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
