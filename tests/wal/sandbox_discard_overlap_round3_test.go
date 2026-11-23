package wal_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Round 3 (what 6aa0f7a left of the round 2 finding "a discard of a sandbox
// another discard removed says so"). The fix names the missing branch at one
// read: the first one of DeleteBranchByID, which the round 2 test reached by
// holding the first discard at its drop guard. Every other read by ID on the
// way still returns the driver's sentinel as it is. The widest of them is in
// checkout.discard, which reloads the branch after its beforeDiscard hook;
// in a running engine that hook is ingest.Cancel, which stops the branch's
// capture worker and takes as long as that takes. A second discard that
// finishes in that time (the TTL sweeper and the agent, a retried DELETE)
// leaves the first with
//
//	failed to release the sandbox: mongo: no documents in result
//
// which DELETE /sandboxes/<name> answers as 409 with that text,
// argon_sandbox_discard as "discard failed: failed to release the sandbox:
// mongo: no documents in result", and the sweeper logs as "sandbox sweep
// skipped". None says the sandbox is gone.
//
// The same on master (the reload has always been there): pre-existing.
func TestRound3_DiscardOverlappingAnotherBeforeItsReloadSaysTheSandboxIsGone(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("round3-discard-overlap-%d", time.Now().UnixNano())

	sweeper := newReviewProcess(t, db)
	agent := newReviewProcess(t, db)

	main, err := agent.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	writer := walwriter.New(walService, agent.branches, materializer.NewService(walService, agent.branches), main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "seed"})
	require.NoError(t, err)

	info, err := agent.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(info.PhysicalDB).Drop(context.Background()) })

	// While the sweeper's discard stops capture, the agent's discard runs.
	var once sync.Once
	var agentErr error
	sweeper.checkout.SetBeforeDiscard(func(context.Context, string) error {
		once.Do(func() { agentErr = agent.sandbox.Discard(ctx, info.BranchID) })
		return nil
	})
	sweeperErr := sweeper.sandbox.Discard(ctx, info.BranchID)

	require.NoError(t, agentErr, "the agent's discard removes the sandbox")
	_, err = agent.branches.GetBranchByID(info.BranchID)
	require.Error(t, err, "the sandbox is gone")
	t.Logf("the overlapping Discard returned: %v", sweeperErr)

	if sweeperErr != nil {
		assert.False(t, strings.Contains(sweeperErr.Error(), "mongo: no documents in result") && !strings.Contains(sweeperErr.Error(), "not found"),
			"the overlapping discard of a sandbox that is gone answers with the driver's bare sentinel: %q", sweeperErr.Error())
	}
}
