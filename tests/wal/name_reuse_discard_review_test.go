package wal_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/sandbox"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// reviewProcess is one Argon process's services on a shared metadata
// database: its own branch, checkout and sandbox services.
type reviewProcess struct {
	branches *branchwal.BranchService
	checkout *checkout.Service
	sandbox  *sandbox.Service
}

func newReviewProcess(t *testing.T, db *mongo.Database) *reviewProcess {
	t.Helper()
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branches, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	co := checkout.NewService(db.Client(), db, branches, materializer.NewService(walService, branches))
	return &reviewProcess{branches: branches, checkout: co, sandbox: sandbox.NewService(branches, co)}
}

// Review: sandbox.Service.Discard takes a branch ID, but finishes with
// DeleteBranch(projectID, branch.Name). While a name belonged to one branch
// for good, the two were the same thing. Now that a name can be used again,
// a discard that is still running when the sandbox is discarded elsewhere
// and started over under the same name (the TTL sweeper and the agent, a
// retried DELETE, two agents) deletes the NEW sandbox's branch: Discard(id1)
// returns nil, the new sandbox's record is deleted while it is checked out,
// and its database is left behind with no branch.
//
// The interleaving is made deterministic at the drop guard, which runs
// inside the first discard after it has reloaded the branch and before it
// drops the database (in production it is a query of wal_sources followed
// by dropDatabase).
func TestReview_DiscardByIDDoesNotDeleteTheSandboxThatReusedItsName(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("review-discard-reuse-%d", time.Now().UnixNano())

	sweeper := newReviewProcess(t, db) // e.g. the server's TTL sweeper, or a retried request
	agent := newReviewProcess(t, db)   // the agent that discards and starts over

	main, err := agent.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	writer := walwriter.New(walService, agent.branches, materializer.NewService(walService, agent.branches), main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "seed"})
	require.NoError(t, err)

	first, err := agent.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(first.PhysicalDB).Drop(context.Background()) })

	var once sync.Once
	var second *sandbox.Info
	var agentDiscardErr, agentCreateErr error
	sweeper.checkout.SetDropGuard(func(context.Context, string) error {
		once.Do(func() {
			agentDiscardErr = agent.sandbox.Discard(ctx, first.BranchID)
			second, agentCreateErr = agent.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
		})
		return nil
	})

	discardErr := sweeper.sandbox.Discard(ctx, first.BranchID)

	require.NoError(t, agentDiscardErr, "the agent's own discard of the first sandbox")
	require.NoError(t, agentCreateErr, "the agent starts over under the same name")
	require.NotNil(t, second)
	t.Cleanup(func() { _ = db.Client().Database(second.PhysicalDB).Drop(context.Background()) })
	require.NotEqual(t, first.BranchID, second.BranchID)
	t.Logf("the overlapping Discard(%s) returned: %v", first.BranchID, discardErr)

	// The agent now holds a connection string for the second sandbox. A
	// discard of the FIRST sandbox's ID must leave it alone.
	current, err := agent.branches.GetBranchByID(second.BranchID)
	require.NoError(t, err, "Discard(%s) deleted branch %s, the new sandbox that reused the name %q", first.BranchID, second.BranchID, second.BranchName)
	assert.True(t, current.IsLive())
	byName, err := agent.branches.GetBranch(project, "run-1")
	require.NoError(t, err)
	assert.Equal(t, second.BranchID, byName.ID)
}
