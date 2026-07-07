package wal_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/argon-lab/argon/internal/undo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestUndo_RestoresPreRangeState(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	undoService := undo.NewService(f.wal, f.branches, db.Client())
	ctx := context.Background()

	main, err := f.branches.CreateBranch("undo-test", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	// Segment A: the baseline the undo must restore.
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "keep", "v": int32(1)})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "victim", "v": int32(10)})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "doomed", "v": int32(5)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	baselineLSN := main.HeadLSN
	baseline, err := f.matFull.MaterializeBranch(main)
	require.NoError(t, err)

	// Segment B: the damage — an update, a delete, an insert, and a
	// second update of the same victim (only the oldest pre-image counts).
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "victim", "v": int32(99)})
	require.NoError(t, err)
	_, _, err = writer.Delete(ctx, "docs", "doomed")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "intruder", "v": int32(666)})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "victim", "v": int32(100)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	plan, err := undoService.BuildPlan(main, baselineLSN+1, main.HeadLSN, "")
	require.NoError(t, err)
	assert.Len(t, plan.Compensations, 3, "victim restored, doomed restored, intruder deleted")
	assert.Empty(t, plan.Conflicts)
	assert.Empty(t, plan.Unrecoverable)

	restored, deleted, err := undoService.Apply(ctx, main, plan)
	require.NoError(t, err)
	assert.Equal(t, 2, restored)
	assert.Equal(t, 1, deleted)

	// The branch state equals the baseline again — via new history, not
	// history rewriting.
	main, _ = f.branches.GetBranchByID(main.ID)
	after, err := f.matFull.MaterializeBranch(main)
	require.NoError(t, err)
	requireSameState(t, baseline, after, "state after undo")
	assert.Greater(t, main.HeadLSN, baselineLSN, "undo appends; it never rewrites")

	// Undo entries are attributed.
	entries, err := f.wal.GetBranchEntries(main.ID, "docs", 0, main.HeadLSN)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	assert.Equal(t, "undo", last.Actor)
}

func TestUndo_ActorFilterAndConflicts(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	undoService := undo.NewService(f.wal, f.branches, db.Client())
	ctx := context.Background()

	main, err := f.branches.CreateBranch("undo-actor", "main", "")
	require.NoError(t, err)

	human := walwriter.New(f.wal, f.branches, f.mat, main)
	human.SetActor("user:jake")
	agent := walwriter.New(f.wal, f.branches, f.mat, main)
	agent.SetActor("agent:rogue")

	// Baseline by the human.
	_, err = human.Put(ctx, "cfg", bson.M{"_id": "a", "v": "human"})
	require.NoError(t, err)
	_, err = human.Put(ctx, "cfg", bson.M{"_id": "b", "v": "human"})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	fromLSN := main.HeadLSN + 1

	// The agent damages a and creates c; the human then edits a again
	// (conflict) but leaves the agent's other damage alone.
	_, err = agent.Put(ctx, "cfg", bson.M{"_id": "a", "v": "agent"})
	require.NoError(t, err)
	_, err = agent.Put(ctx, "cfg", bson.M{"_id": "c", "v": "agent"})
	require.NoError(t, err)
	_, err = agent.Put(ctx, "cfg", bson.M{"_id": "b", "v": "agent"})
	require.NoError(t, err)
	_, err = human.Put(ctx, "cfg", bson.M{"_id": "a", "v": "human-again"})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)

	plan, err := undoService.BuildPlan(main, fromLSN, main.HeadLSN, "agent:rogue")
	require.NoError(t, err)

	require.Len(t, plan.Conflicts, 1, "the human's later edit of a is a conflict")
	assert.Equal(t, "a", plan.Conflicts[0].DocumentID)
	assert.Equal(t, "user:jake", plan.Conflicts[0].OtherActor)
	assert.Len(t, plan.Compensations, 2, "b restored, c deleted; a skipped")

	_, _, err = undoService.Apply(ctx, main, plan)
	require.NoError(t, err)

	main, _ = f.branches.GetBranchByID(main.ID)
	state, err := f.matFull.MaterializeCollection(main, "cfg")
	require.NoError(t, err)
	assert.Equal(t, "human-again", state["a"]["v"], "conflicted document keeps the human's edit")
	assert.Equal(t, "human", state["b"]["v"], "agent damage reverted")
	assert.NotContains(t, state, "c", "agent-created document removed")
}

func TestUndo_OnLiveBranchThroughPhysicalDB(t *testing.T) {
	f := newIngestFixture(t, "undo-live")
	undoService := undo.NewService(f.wal, f.branches, f.client)
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	docs := f.physical.Collection("docs")

	// Baseline written directly, captured by the ingester.
	_, err := docs.InsertOne(ctx, bson.M{"_id": "base", "v": int32(1)})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)
	branch, _ := f.branches.GetBranchByID(f.branchID)
	baselineLSN := branch.HeadLSN

	// Damage, also direct.
	_, err = docs.UpdateOne(ctx, bson.M{"_id": "base"}, bson.M{"$set": bson.M{"v": int32(999)}})
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = docs.InsertOne(ctx, bson.M{"_id": fmt.Sprintf("junk-%d", i)})
		require.NoError(t, err)
	}
	f.waitForEntries(t, "docs", 5)

	branch, _ = f.branches.GetBranchByID(f.branchID)
	plan, err := undoService.BuildPlan(branch, baselineLSN+1, branch.HeadLSN, "")
	require.NoError(t, err)
	assert.Len(t, plan.Compensations, 4)

	restored, deleted, err := undoService.Apply(ctx, branch, plan)
	require.NoError(t, err)
	assert.Equal(t, 1, restored)
	assert.Equal(t, 3, deleted)

	// The physical database is back to the baseline...
	var base bson.M
	require.NoError(t, docs.FindOne(ctx, bson.M{"_id": "base"}).Decode(&base))
	assert.EqualValues(t, 1, base["v"])
	count, err := docs.CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)

	// ...and the compensations flow through the ingester as new history.
	f.waitForEntries(t, "docs", 9)
	branch, _ = f.branches.GetBranchByID(f.branchID)
	walState, err := f.matFull.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	assert.Equal(t, f.physicalState(t, "docs"), walState, "WAL converges to the undone physical state")
}
