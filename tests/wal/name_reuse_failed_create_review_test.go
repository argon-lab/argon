package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/sandbox"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Review: a client whose sandbox create is slow gives up on it, discards the
// half-made sandbox it sees in the branch list and creates it again under
// the same name, which is what this change makes possible ("a client that
// retried", "an agent that starts over under the same name"). The first
// create is still checking its branch out. When that checkout then fails,
// because its branch was deleted underneath it, sandbox.Service.removeFailed
// cleans up with DeleteBranch(projectID, branch.Name), and the name now
// belongs to the retry's sandbox: the first create deletes the second
// create's branch.
//
// No hook or sleep decides the order here. The first create runs on its own
// goroutine, as another request would; the retry starts once the first
// create's branch record exists, while its checkout (sixty collections) is
// still loading.
func TestReview_FailedCreateDoesNotDeleteTheSandboxThatReusedItsName(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("review-failed-create-%d", time.Now().UnixNano())

	slow := newReviewProcess(t, db)  // serves the first create
	retry := newReviewProcess(t, db) // serves the discard and the second create

	main, err := slow.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	writer := walwriter.New(walService, slow.branches, materializer.NewService(walService, slow.branches), main)
	for i := 0; i < 60; i++ {
		_, err = writer.Put(ctx, fmt.Sprintf("coll%02d", i), bson.M{"_id": "seed"})
		require.NoError(t, err)
	}
	main, err = slow.branches.GetBranchByID(main.ID)
	require.NoError(t, err)

	type created struct {
		info *sandbox.Info
		err  error
	}
	firstDone := make(chan created, 1)
	go func() {
		info, err := slow.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
		firstDone <- created{info, err}
	}()

	// The client sees the half-made sandbox in the branch list.
	var firstBranch *wal.Branch
	require.Eventually(t, func() bool {
		firstBranch, err = retry.branches.GetBranch(project, "run-1")
		return err == nil
	}, 10*time.Second, time.Millisecond)
	require.False(t, firstBranch.IsLive(), "the first create is still checking out")
	t.Cleanup(func() { _ = db.Client().Database(wal.PhysicalDBName(firstBranch.ID)).Drop(context.Background()) })

	// It discards it and starts over under the same name.
	require.NoError(t, retry.sandbox.Discard(ctx, firstBranch.ID))
	second, secondErr := retry.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)

	first := <-firstDone
	t.Logf("first create (its branch was discarded while it checked out): info=%v err=%v", first.info != nil, first.err)
	if b, err := retry.branches.GetBranch(project, "run-1"); err == nil {
		t.Cleanup(func() { _ = db.Client().Database(wal.PhysicalDBName(b.ID)).Drop(context.Background()) })
	}

	require.NoError(t, secondErr, "the retry under the same name creates its sandbox")
	t.Cleanup(func() { _ = db.Client().Database(second.PhysicalDB).Drop(context.Background()) })
	require.NotEqual(t, firstBranch.ID, second.BranchID)
	current, err := retry.branches.GetBranchByID(second.BranchID)
	require.NoError(t, err, "the first create's cleanup deleted branch %s, the retry's sandbox, by its name %q", second.BranchID, second.BranchName)
	assert.True(t, current.IsLive(), "the retry's sandbox is checked out")
}
