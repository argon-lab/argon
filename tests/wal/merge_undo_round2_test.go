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

// A merge into a branch that is not checked out, then a restore reset of the
// branch to before the merge, and no write since: the head is below the
// merge, so the guard `fromLSN <= branch.HeadLSN` skips the discarded check
// and the refusal is buildPlan's "invalid undo range", not that the merge
// was discarded by a reset.
func TestRound2_UndoMergeDiscardedByResetWithNoLaterWrite(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "round2-reset-nowrite")
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
	require.Less(t, f.main.HeadLSN, plan.MergeFromLSN)

	undos := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	_, err = undos.BuildMergePlan(f.main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discarded by a restore reset")
}

// A merge of zero changes into a checked-out branch: the empty plan names a
// range whose start is past its end (the merge record LSN is above the
// head capture has reached), which the REST undo reports as from_lsn > to_lsn.
func TestRound2_ZeroChangeLiveMergePlanRangeIsOrdered(t *testing.T) {
	f := newIngestFixture(t, "round2-live-zero")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "a", "v": int32(1)})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("round2-live-zero", "feature", main.ID)
	require.NoError(t, err)
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Empty(t, plan.Changes)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	plan, err = merges.GetPlan(ctx, plan.ID)
	require.NoError(t, err)

	time.Sleep(2 * time.Second)
	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	undos := undo.NewService(f.wal, f.branches, f.client, f.mat)
	undoPlan, err := undos.BuildMergePlan(main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.NoError(t, err)
	assert.LessOrEqual(t, undoPlan.FromLSN, undoPlan.ToLSN, "head=%d merge_from_lsn=%d", main.HeadLSN, plan.MergeFromLSN)
}

// A merge with changes into a checked-out branch whose capture is not
// running: nothing is recorded yet, the head is below the merge record, and
// the refusal is "invalid undo range" instead of the live message telling
// the user to let capture catch up (or start it).
func TestRound2_UndoLiveMergeBeforeCaptureSaysCapture(t *testing.T) {
	f := newIngestFixture(t, "round2-live-nocapture")
	ctx := context.Background()
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	require.True(t, main.IsLive())
	feature, err := f.branches.CreateBranch("round2-live-nocapture", "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "x", "v": int32(1)})
	require.NoError(t, err)
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	plan, err = merges.GetPlan(ctx, plan.ID)
	require.NoError(t, err)

	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	undos := undo.NewService(f.wal, f.branches, f.client, f.mat)
	_, err = undos.BuildMergePlan(main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capture", "head=%d merge_from_lsn=%d", main.HeadLSN, plan.MergeFromLSN)
}
