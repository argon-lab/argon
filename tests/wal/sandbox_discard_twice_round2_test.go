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

// Round 2: two discards of one sandbox overlap (the TTL sweeper and the
// agent, a retried DELETE). The one that finishes second now ends in
// DeleteBranchByID, whose GetBranchByID returns the driver's sentinel as it
// is, so the caller is told "mongo: no documents in result": the DELETE
// endpoint answers 409 with that as the error, argon_sandbox_discard answers
// "discard failed: mongo: no documents in result", and the sweeper logs
// "sandbox sweep skipped: run-1 (mongo: no documents in result)". None of
// them says that the sandbox is gone, which is what the caller asked for.
// Before 88d08c9 the same overlap answered "branch not found".
//
// The order is fixed at the drop guard, which runs inside the first discard
// before it drops the database.
func TestRound2_DiscardOfASandboxAnotherDiscardRemovedSaysSo(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("round2-discard-twice-%d", time.Now().UnixNano())

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

	var once sync.Once
	var agentErr error
	sweeper.checkout.SetDropGuard(func(context.Context, string) error {
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
