package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A merge of zero changes into a checked-out branch (here: every conflict
// kept as ours) is applied and recorded. Undoing it by plan should be an
// empty plan, as it is on a branch that is not checked out, not a refusal.
func TestReview_UndoZeroChangeMergeIntoLiveTarget(t *testing.T) {
	f := newIngestFixture(t, "review-live-zero")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "a", "v": int32(1)})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("review-live-zero", "feature", main.ID)
	require.NoError(t, err)
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Empty(t, plan.Changes)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	plan, err = merges.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, merge.StatusApplied, plan.Status)
	require.Greater(t, plan.MergeFromLSN, int64(0))

	time.Sleep(2 * time.Second) // let capture read the marker transaction
	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	undos := undo.NewService(f.wal, f.branches, f.client, f.mat)
	undoPlan, err := undos.BuildMergePlan(main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.NoError(t, err, "head=%d merge_from_lsn=%d", main.HeadLSN, plan.MergeFromLSN)
	assert.Empty(t, undoPlan.Compensations)
}

// A merge into a branch that is not checked out, then a restore reset of the
// branch to before the merge, then a new write: the merge is no longer part
// of the branch. The refusal should not tell the user to wait for capture on
// a branch that has no capture.
func TestReview_UndoMergeDiscardedByResetBlamesCapture(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "review-reset-discard")
	ctx := context.Background()
	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "feature"})
	require.NoError(t, err)
	f.refresh(t)
	before := f.main.HeadLSN
	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	_, err = f.merge.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	plan, err = f.merge.GetPlan(ctx, plan.ID)
	require.NoError(t, err)

	_, err = f.restore.ResetBranchToLSN(f.main.ID, before)
	require.NoError(t, err)
	f.refresh(t)
	writer := walwriter.New(f.wal, f.branches, f.mat, f.main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "stable", "v": "after-reset"})
	require.NoError(t, err)
	f.refresh(t)
	require.False(t, f.main.IsLive())

	undos := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	_, err = undos.BuildMergePlan(f.main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "capture", "branch %s is not checked out; the merge was discarded by a reset", f.main.Name)
}

// Undoing the same merge twice: the second plan must not compensate again.
func TestReview_UndoMergeTwiceOnWALBranch(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "review-undo-twice")
	ctx := context.Background()
	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "feature"})
	require.NoError(t, err)
	_, _, err = f.featWriter.Delete(ctx, "docs", "doomed")
	require.NoError(t, err)
	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	_, err = f.merge.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	plan, err = f.merge.GetPlan(ctx, plan.ID)
	require.NoError(t, err)

	undos := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	f.refresh(t)
	first, err := undos.BuildMergePlan(f.main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.NoError(t, err)
	_, _, err = undos.Apply(ctx, f.main, first)
	require.NoError(t, err)
	f.refresh(t)
	second, err := undos.BuildMergePlan(f.main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.NoError(t, err)
	assert.Empty(t, second.Compensations)
	assert.Len(t, second.Conflicts, 2)
}

// A live merge resolved with "theirs": every resolved conflict is a change,
// and capture records one entry for each, so the count check passes.
func TestReview_UndoTheirsMergeIntoLiveTarget(t *testing.T) {
	f := newIngestFixture(t, "review-live-theirs")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()
	_, err := f.physical.Collection("docs").InsertMany(ctx, []interface{}{
		bson.M{"_id": "c", "v": int32(1)}, bson.M{"_id": "d", "v": int32(1)},
	})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 2)
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("review-live-theirs", "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "c", "v": int32(2)})
	require.NoError(t, err)
	_, _, err = writer.Delete(ctx, "docs", "d")
	require.NoError(t, err)
	_, err = f.physical.Collection("docs").UpdateOne(ctx, bson.M{"_id": "c"}, bson.M{"$set": bson.M{"v": int32(3)}})
	require.NoError(t, err)
	_, err = f.physical.Collection("docs").UpdateOne(ctx, bson.M{"_id": "d"}, bson.M{"$set": bson.M{"v": int32(3)}})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 4)

	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Conflicts, 2)
	_, err = merges.Apply(ctx, plan.ID, merge.StrategyTheirs)
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 6)
	plan, err = merges.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 2, plan.MergeChanges)

	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	undos := undo.NewService(f.wal, f.branches, f.client, f.mat)
	undoPlan, err := undos.BuildMergePlan(main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.NoError(t, err)
	require.Len(t, undoPlan.Compensations, 2)
}
