package wal_test

import (
	"context"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// liveMergeFixture is a checked-out main with capture running, a feature
// branch holding two changes for it, and an applied merge of them.
type liveMergeFixture struct {
	*ingestFixture
	merges    *merge.Service
	undos     *undo.Service
	plan      *merge.Plan
	feature   string
	featureID string
}

func newLiveMergeFixture(t *testing.T, project string) *liveMergeFixture {
	t.Helper()
	f := newIngestFixture(t, project)
	ctx := context.Background()
	stop := f.startIngester(t)
	t.Cleanup(stop)

	// main's own history: three documents written by the application.
	_, err := f.physical.Collection("docs").InsertMany(ctx, []interface{}{
		bson.M{"_id": "changed", "v": int32(1)},
		bson.M{"_id": "untouched", "v": int32(1)},
		bson.M{"_id": "later", "v": int32(1)},
	})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 3)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch(project, "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "changed", "v": int32(2)})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "inserted", "v": int32(1)})
	require.NoError(t, err)

	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 2)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 5)

	plan, err = merges.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	return &liveMergeFixture{
		ingestFixture: f, merges: merges, plan: plan, feature: feature.Name, featureID: feature.ID,
		undos: undo.NewService(f.wal, f.branches, f.client, f.mat),
	}
}

func (f *liveMergeFixture) doc(t *testing.T, id string) bson.M {
	t.Helper()
	var doc bson.M
	err := f.physical.Collection("docs").FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil
	}
	require.NoError(t, err)
	return doc
}

// A merge into a checked-out branch reaches the WAL through capture. Its
// entries carry the merge's actor and plan, as a merge into a branch that is
// not checked out does, so the merge can be told apart and undone exactly.
func TestMerge_IntoLiveTargetIsAttributedToTheMerge(t *testing.T) {
	f := newLiveMergeFixture(t, "merge-live-tag")
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(main.ID, "docs", main.BaseLSN+1, main.HeadLSN)
	require.NoError(t, err)

	var merged []string
	for _, entry := range entries {
		if !entry.IsData() {
			continue
		}
		if entry.Actor == "merge:"+f.feature {
			merged = append(merged, entry.DocumentID)
			assert.Equal(t, f.plan.ID.Hex(), entry.Metadata["merge_plan"], "entry %s/%s", entry.Collection, entry.DocumentID)
			continue
		}
		assert.Equal(t, "ingest", entry.Actor, "an application write keeps the capture actor: %s", entry.DocumentID)
	}
	assert.ElementsMatch(t, []string{"changed", "inserted"}, merged)
	assert.Greater(t, f.plan.MergeLSN, int64(0), "the applied plan records the merge's LSN")
	assert.Equal(t, 2, f.plan.MergeChanges)

	// The marker that carried the attribution leaves nothing behind.
	names, err := f.physical.ListCollectionNames(context.Background(), bson.M{})
	require.NoError(t, err)
	for _, name := range names {
		if name == "__argon_merge_markers" {
			count, err := f.physical.Collection(name).CountDocuments(context.Background(), bson.M{})
			require.NoError(t, err)
			assert.Zero(t, count)
		}
	}
}

// Undoing by the merge's actor reverts the merge and nothing else on the
// checked-out branch.
func TestMerge_UndoMergeIntoLiveTargetByActor(t *testing.T) {
	f := newLiveMergeFixture(t, "merge-live-undo-actor")
	ctx := context.Background()
	// An application write after the merge, to a document the merge did not touch.
	_, err := f.physical.Collection("docs").UpdateOne(ctx, bson.M{"_id": "later"}, bson.M{"$set": bson.M{"v": int32(9)}})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 6)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	plan, err := f.undos.BuildPlan(main, f.plan.MergeLSN, 0, "merge:"+f.feature)
	require.NoError(t, err)
	require.Len(t, plan.Compensations, 2, "%+v", plan)
	require.Empty(t, plan.Conflicts)
	_, _, err = f.undos.Apply(ctx, main, plan)
	require.NoError(t, err)

	assert.EqualValues(t, 1, f.doc(t, "changed")["v"])
	assert.Nil(t, f.doc(t, "inserted"))
	assert.EqualValues(t, 9, f.doc(t, "later")["v"], "the later application write survives")
	assert.EqualValues(t, 1, f.doc(t, "untouched")["v"])
}

// Undoing by plan reverts exactly that merge, even when the same source was
// merged again afterwards.
func TestMerge_UndoMergeIntoLiveTargetByPlan(t *testing.T) {
	f := newLiveMergeFixture(t, "merge-live-undo-plan")
	ctx := context.Background()

	// A second merge from the same branch, to another document.
	feature, err := f.branches.GetBranchByID(f.featureID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "second", "v": int32(1)})
	require.NoError(t, err)
	second, err := f.merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, second.Changes, 1)
	_, err = f.merges.Apply(ctx, second.ID, "")
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 6)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	plan, err := f.undos.BuildMergePlan(main, f.plan.ID.Hex(), f.plan.MergeLSN, f.plan.MergeChanges)
	require.NoError(t, err)
	require.Len(t, plan.Compensations, 2, "%+v", plan)
	_, _, err = f.undos.Apply(ctx, main, plan)
	require.NoError(t, err)

	assert.EqualValues(t, 1, f.doc(t, "changed")["v"])
	assert.Nil(t, f.doc(t, "inserted"))
	assert.EqualValues(t, 1, f.doc(t, "second")["v"], "the second merge stays")
}

// A plan-based undo refuses while capture has not recorded the whole merge,
// rather than reverting part of it.
func TestMerge_UndoByPlanRefusesAnIncompleteCapture(t *testing.T) {
	f := newLiveMergeFixture(t, "merge-live-undo-partial")
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	_, err = f.undos.BuildMergePlan(main, f.plan.ID.Hex(), f.plan.MergeLSN, f.plan.MergeChanges+1)
	require.ErrorContains(t, err, "capture has recorded 2 of the merge's 3 changes")
}

// On a branch that is not checked out the merge appends its own entries;
// an undo by plan reverts them too, and only them.
func TestMerge_UndoByPlanOnABranchNotCheckedOut(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-undo-plan-wal")
	ctx := context.Background()
	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "feature"})
	require.NoError(t, err)
	_, _, err = f.featWriter.Delete(ctx, "docs", "doomed")
	require.NoError(t, err)
	f.refresh(t)
	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	_, err = f.merge.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	// main's own write after the merge.
	_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": "stable", "v": "main-later"})
	require.NoError(t, err)
	plan, err = f.merge.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	require.Equal(t, 2, plan.MergeChanges)
	require.Less(t, plan.MergeFromLSN, plan.MergeLSN, "the merge's entries precede its record")

	f.refresh(t)
	undos := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	undoPlan, err := undos.BuildMergePlan(f.main, plan.ID.Hex(), plan.MergeFromLSN, plan.MergeChanges)
	require.NoError(t, err)
	require.Len(t, undoPlan.Compensations, 2)
	_, _, err = undos.Apply(ctx, f.main, undoPlan)
	require.NoError(t, err)

	f.refresh(t)
	state, err := f.matFull.MaterializeCollection(f.main, "docs")
	require.NoError(t, err)
	assert.Equal(t, "base", state["contested"]["v"])
	assert.Equal(t, "base", state["doomed"]["v"], "the merged delete is undone")
	assert.Equal(t, "main-later", state["stable"]["v"], "main's own later write stays")
}

// A merge larger than a change-stream cursor batch is attributed whole: the
// marker opens the transaction, and the changes after it arrive in later
// batches.
func TestMerge_LargeMergeIntoLiveTargetIsAttributedAcrossBatches(t *testing.T) {
	f := newIngestFixture(t, "merge-live-large")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "base"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("merge-live-large", "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	const n = 600
	docs := make([]bson.M, n)
	for i := range docs {
		docs[i] = bson.M{"_id": i, "pad": strings.Repeat("x", 512)}
	}
	_, err = writer.PutMany(ctx, "docs", docs)
	require.NoError(t, err)
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, n)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	f.waitForEntries(t, "docs", n+1)

	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(main.ID, "docs", main.BaseLSN+1, main.HeadLSN)
	require.NoError(t, err)
	attributed := 0
	for _, entry := range entries {
		if entry.IsData() && entry.Actor == "merge:feature" && entry.Metadata["merge_plan"] == plan.ID.Hex() {
			attributed++
		}
	}
	assert.Equal(t, n, attributed)
}
