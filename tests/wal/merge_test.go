package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/internal/merge"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// mergeFixture: a main branch with seeded data and a forked feature branch,
// each with its own writer.
type mergeFixture struct {
	*snapshotFixture
	merge      *merge.Service
	main       *wal.Branch
	feature    *wal.Branch
	mainWriter *walwriter.Writer
	featWriter *walwriter.Writer
}

func newMergeFixture(t *testing.T, db *mongo.Database, project string) *mergeFixture {
	t.Helper()
	f := newSnapshotFixture(t, db)
	mergeService := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	ctx := context.Background()

	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	mainWriter := walwriter.New(f.wal, f.branches, f.mat, main)

	// Shared baseline.
	_, err = mainWriter.PutMany(ctx, "docs", []bson.M{
		{"_id": "stable", "v": "base"},
		{"_id": "contested", "v": "base"},
		{"_id": "doomed", "v": "base"},
	})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	feature, err := f.branches.CreateBranch(project, "feature", main.ID)
	require.NoError(t, err)
	featWriter := walwriter.New(f.wal, f.branches, f.mat, feature)

	return &mergeFixture{
		snapshotFixture: f,
		merge:           mergeService,
		main:            main,
		feature:         feature,
		mainWriter:      mainWriter,
		featWriter:      featWriter,
	}
}

func (f *mergeFixture) refresh(t *testing.T) {
	t.Helper()
	f.main, _ = f.branches.GetBranchByID(f.main.ID)
	f.feature, _ = f.branches.GetBranchByID(f.feature.ID)
}

func TestMerge_CleanMerge(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-clean")
	ctx := context.Background()

	// Feature: modify one doc, add one, delete one. Main: untouched.
	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "feature"})
	require.NoError(t, err)
	_, err = f.featWriter.Put(ctx, "docs", bson.M{"_id": "new", "v": "feature"})
	require.NoError(t, err)
	_, _, err = f.featWriter.Delete(ctx, "docs", "doomed")
	require.NoError(t, err)
	f.refresh(t)

	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	assert.Len(t, plan.Changes, 3)
	assert.Empty(t, plan.Conflicts)

	result, err := f.merge.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	assert.Equal(t, 3, result.Applied)

	f.refresh(t)
	state, err := f.matFull.MaterializeCollection(f.main, "docs")
	require.NoError(t, err)
	assert.Equal(t, "feature", state["contested"]["v"])
	assert.Equal(t, "feature", state["new"]["v"])
	assert.Equal(t, "base", state["stable"]["v"], "untouched documents stay")
	assert.NotContains(t, state, "doomed", "feature's delete merged")

	// The audit marker is on the target.
	entries, err := f.wal.GetBranchEntries(f.main.ID, "", 0, f.main.HeadLSN)
	require.NoError(t, err)
	var sawMerge bool
	for _, e := range entries {
		if e.Operation == wal.OpMerge {
			sawMerge = true
			assert.Equal(t, plan.ID.Hex(), e.Metadata["plan_id"])
		}
	}
	assert.True(t, sawMerge, "merge control entry recorded")

	// The plan is spent.
	applied, err := f.merge.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, merge.StatusApplied, applied.Status)
	_, err = f.merge.Apply(ctx, plan.ID, "")
	assert.Error(t, err, "a plan applies exactly once")
}

func TestMerge_BothSidesNonOverlapping(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-both")
	ctx := context.Background()

	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "from-feature", "v": 1})
	require.NoError(t, err)
	_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": "from-main", "v": 2})
	require.NoError(t, err)
	f.refresh(t)

	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	assert.Len(t, plan.Changes, 1, "only the feature's addition merges")
	assert.Empty(t, plan.Conflicts)

	_, err = f.merge.Apply(ctx, plan.ID, "")
	require.NoError(t, err)

	f.refresh(t)
	state, err := f.matFull.MaterializeCollection(f.main, "docs")
	require.NoError(t, err)
	assert.Contains(t, state, "from-feature")
	assert.Contains(t, state, "from-main", "the target's own progress is kept")
}

func TestMerge_IdenticalChangeIsNotAConflict(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-identical")
	ctx := context.Background()

	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "same"})
	require.NoError(t, err)
	_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "same"})
	require.NoError(t, err)
	f.refresh(t)

	plan, err := f.merge.Compute(f.feature.ID)
	require.NoError(t, err)
	assert.Empty(t, plan.Changes, "identical change needs no merge")
	assert.Empty(t, plan.Conflicts, "identical change is not a conflict")
}

func TestMerge_ConflictStrategies(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	setup := func(project string) *mergeFixture {
		f := newMergeFixture(t, db, project)
		_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "theirs"})
		require.NoError(t, err)
		_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "ours"})
		require.NoError(t, err)
		f.refresh(t)
		return f
	}

	t.Run("Without a strategy the apply refuses", func(t *testing.T) {
		f := setup("merge-conflict-a")
		plan, err := f.merge.Preview(ctx, f.feature.ID)
		require.NoError(t, err)
		require.Len(t, plan.Conflicts, 1)
		assert.Empty(t, plan.Changes)

		_, err = f.merge.Apply(ctx, plan.ID, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "conflict")
	})

	t.Run("Strategy theirs adopts the branch", func(t *testing.T) {
		f := setup("merge-conflict-b")
		plan, err := f.merge.Preview(ctx, f.feature.ID)
		require.NoError(t, err)

		result, err := f.merge.Apply(ctx, plan.ID, merge.StrategyTheirs)
		require.NoError(t, err)
		assert.Equal(t, 1, result.ConflictsResolved)

		f.refresh(t)
		state, err := f.matFull.MaterializeCollection(f.main, "docs")
		require.NoError(t, err)
		assert.Equal(t, "theirs", state["contested"]["v"])
	})

	t.Run("Strategy ours keeps the target", func(t *testing.T) {
		f := setup("merge-conflict-c")
		plan, err := f.merge.Preview(ctx, f.feature.ID)
		require.NoError(t, err)

		result, err := f.merge.Apply(ctx, plan.ID, merge.StrategyOurs)
		require.NoError(t, err)
		assert.Equal(t, 1, result.ConflictsResolved)
		assert.Equal(t, 0, result.Applied)

		f.refresh(t)
		state, err := f.matFull.MaterializeCollection(f.main, "docs")
		require.NoError(t, err)
		assert.Equal(t, "ours", state["contested"]["v"])
	})
}

func TestMerge_DeleteVersusModifyConflict(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-delmod")
	ctx := context.Background()

	_, _, err := f.featWriter.Delete(ctx, "docs", "contested")
	require.NoError(t, err)
	_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": "contested", "v": "ours-edit"})
	require.NoError(t, err)
	f.refresh(t)

	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Conflicts, 1, "delete versus modify is a conflict")
	assert.Nil(t, plan.Conflicts[0].Theirs, "theirs side is the deletion")

	_, err = f.merge.Apply(ctx, plan.ID, merge.StrategyTheirs)
	require.NoError(t, err)

	f.refresh(t)
	state, err := f.matFull.MaterializeCollection(f.main, "docs")
	require.NoError(t, err)
	assert.NotContains(t, state, "contested", "strategy theirs applies the deletion")
}

func TestMerge_StalePlanRefusesToApply(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-stale")
	ctx := context.Background()

	_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": "new", "v": 1})
	require.NoError(t, err)
	f.refresh(t)

	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)

	// The target moves on after the preview.
	_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": "late", "v": 2})
	require.NoError(t, err)

	_, err = f.merge.Apply(ctx, plan.ID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale")
}

func TestMerge_IntoLiveTarget(t *testing.T) {
	f := newIngestFixture(t, "merge-live")
	mergeService := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	// The live main gets its baseline through the ingester.
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "base", "v": int32(1)})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	// Fork a (metadata-only) feature branch and change data there.
	main, _ := f.branches.GetBranchByID(f.branchID)
	feature, err := f.branches.CreateBranch("merge-live", "feature", main.ID)
	require.NoError(t, err)
	featWriter := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = featWriter.Put(ctx, "docs", bson.M{"_id": "merged-in", "v": int32(42)})
	require.NoError(t, err)

	plan, err := mergeService.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)

	_, err = mergeService.Apply(ctx, plan.ID, "")
	require.NoError(t, err)

	// The change landed in the physical database...
	var doc bson.M
	require.NoError(t, f.physical.Collection("docs").FindOne(ctx, bson.M{"_id": "merged-in"}).Decode(&doc))
	assert.EqualValues(t, 42, doc["v"])

	// ...and flows back through the ingester into the WAL.
	f.waitForEntries(t, "docs", 2)
	branch, _ := f.branches.GetBranchByID(f.branchID)
	walState, err := f.matFull.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	assert.Equal(t, f.physicalState(t, "docs"), walState)
}
