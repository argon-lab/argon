package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/internal/undo"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestUndo_MissingCapturedUpdatePreImageNeverDeletesDocument(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	branch, err := f.branches.CreateBranch("undo-gap", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, branch)
	_, err = writer.Put(context.Background(), "docs", bson.M{"_id": "existing", "v": 1})
	require.NoError(t, err)
	post, err := bson.Marshal(bson.M{"_id": "existing", "v": 2})
	require.NoError(t, err)
	lsn, err := f.wal.Append(&wal.Entry{ProjectID: branch.ProjectID, BranchID: branch.ID, Collection: "docs", DocumentID: "existing", Operation: wal.OpPut, PostImage: post, Actor: "agent:run", Metadata: map[string]interface{}{"capture_operation": "update"}})
	require.NoError(t, err)
	require.NoError(t, f.branches.UpdateBranchHead(branch.ID, lsn))
	branch, err = f.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	svc := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	plan, err := svc.BuildPlan(branch, lsn, lsn, "agent:run")
	require.NoError(t, err)
	require.Empty(t, plan.Compensations)
	require.Equal(t, []string{"docs/existing"}, plan.Unrecoverable)
	_, _, err = svc.Apply(context.Background(), branch, plan)
	require.NoError(t, err)
	state, err := f.matFull.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	require.Contains(t, state, "existing")
}

func TestUndo_CompensationsCanBeUndoneAndStalePlanRefused(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	branch, err := f.branches.CreateBranch("undo-twice", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, branch)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "a", "v": 1})
	require.NoError(t, err)
	damage, err := writer.Put(ctx, "docs", bson.M{"_id": "a", "v": 2})
	require.NoError(t, err)
	branch, err = f.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	svc := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	plan, err := svc.BuildPlan(branch, damage, damage, "")
	require.NoError(t, err)
	_, _, err = svc.Apply(ctx, branch, plan)
	require.NoError(t, err)
	branch, err = f.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	undoPlan, err := svc.BuildPlan(branch, damage+1, branch.HeadLSN, "undo")
	require.NoError(t, err)
	_, _, err = svc.Apply(ctx, branch, undoPlan)
	require.NoError(t, err)
	branch, err = f.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	state, err := f.matFull.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	require.EqualValues(t, 2, state["a"]["v"])
	_, _, err = svc.Apply(ctx, branch, undoPlan)
	require.ErrorContains(t, err, "stale")
}

func TestUndo_HistoricalRangePreservesLaterWrites(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	branch, err := f.branches.CreateBranch("undo-historical", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, branch)
	writer.SetActor("agent:run")
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "a", "v": 1})
	require.NoError(t, err)
	damage, err := writer.Put(ctx, "docs", bson.M{"_id": "a", "v": 2})
	require.NoError(t, err)
	later, err := writer.Put(ctx, "docs", bson.M{"_id": "a", "v": 3})
	require.NoError(t, err)
	branch, err = f.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	svc := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	for _, actor := range []string{"", "agent:run"} {
		plan, err := svc.BuildPlan(branch, damage, damage, actor)
		require.NoError(t, err)
		require.Empty(t, plan.Compensations)
		require.Len(t, plan.Conflicts, 1)
		require.Equal(t, later, plan.Conflicts[0].AtLSN)
		_, _, err = svc.Apply(ctx, branch, plan)
		require.NoError(t, err)
	}
	state, err := f.mat.MaterializeDocument(branch, "docs", "a")
	require.NoError(t, err)
	require.EqualValues(t, 3, state["v"])
}

func TestUndo_LiveStaleDocumentRollsBackWholePlan(t *testing.T) {
	f := newIngestFixture(t, "undo-live-stale")
	ctx := captureContext(t)
	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	users := f.physical.Collection("users")
	_, err := users.InsertOne(ctx, bson.M{"_id": "z", "n": 1})
	require.NoError(t, err)
	_, err = users.UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"n": 2}})
	require.NoError(t, err)
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	svc := undo.NewService(f.wal, f.branches, f.client, f.mat)
	entries, err := f.wal.GetBranchEntries(branch.ID, "users", 0, branch.HeadLSN)
	require.NoError(t, err)
	plan, err := svc.BuildPlan(branch, entries[1].LSN, branch.HeadLSN, "")
	require.NoError(t, err)
	// Change z while capture is stopped. The WAL head is unchanged, but the
	// physical compare inside the transaction must catch the stale plan.
	_, err = users.UpdateOne(ctx, bson.M{"_id": "z"}, bson.M{"$set": bson.M{"n": 999}})
	require.NoError(t, err)
	_, _, err = svc.Apply(ctx, branch, plan)
	require.ErrorContains(t, err, "stale")
	var seed bson.M
	require.NoError(t, users.FindOne(ctx, bson.M{"_id": "seed"}).Decode(&seed))
	require.EqualValues(t, 2, seed["n"], "earlier compensation must roll back with the later stale document")
}
