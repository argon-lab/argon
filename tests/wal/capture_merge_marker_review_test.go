package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// reviewMergeThenRestart merges two documents of collection into a
// checked-out main while capture runs, stops capture shortly after the
// merge commits (after capture has read the transaction but before the
// barrier that proves it complete, which waits for the stream's 500ms idle
// await), restarts capture, and returns the merged collection's data entries.
func reviewMergeThenRestart(t *testing.T, project, collection string, wait time.Duration) (plan *merge.Plan, actors map[string]string, plans map[string]interface{}) {
	t.Helper()
	f := newIngestFixture(t, project)
	ctx := context.Background()
	stop := f.startIngester(t)

	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app", "v": int32(1)})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch(project, "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, collection, bson.M{"_id": "a", "v": int32(1)})
	require.NoError(t, err)
	_, err = writer.Put(ctx, collection, bson.M{"_id": "b", "v": int32(1)})
	require.NoError(t, err)

	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err = merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 2)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)

	time.Sleep(wait)
	stop()

	restart := f.startIngester(t)
	defer restart()
	f.waitForEntries(t, collection, 2)

	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(main.ID, collection, main.BaseLSN+1, main.HeadLSN)
	require.NoError(t, err)
	actors = map[string]string{}
	plans = map[string]interface{}{}
	for _, entry := range entries {
		if entry.IsData() {
			actors[entry.DocumentID] = entry.Actor
			plans[entry.DocumentID] = entry.Metadata["merge_plan"]
		}
	}
	return plan, actors, plans
}

// Control: the merged collection already exists on main. A restart inside
// the barrier window replays from before the marker, so the merge keeps its
// actor.
func TestReview_RestartAfterMergeIntoExistingCollectionKeepsAttribution(t *testing.T) {
	project := fmt.Sprintf("review-restart-existing-%d", time.Now().UnixNano())
	plan, actors, plans := reviewMergeThenRestart(t, project, "docs", 200*time.Millisecond)
	for _, id := range []string{"a", "b"} {
		assert.Equal(t, "merge:feature", actors[id], "docs/%s", id)
		assert.Equal(t, plan.ID.Hex(), plans[id], "docs/%s", id)
	}
}

// The merge brings a collection main does not have yet. Its transaction then
// holds a "create" event after the marker; capture converts that event to no
// entry and advances lastSafeToken past the marker, so the transaction's
// replay point (transactionStartToken) lies after the marker. A restart inside
// the barrier window resumes there, never reads the marker, and records the
// merge's writes under the checkout's actor without merge_plan.
func TestReview_RestartAfterMergeIntoNewCollectionKeepsAttribution(t *testing.T) {
	project := fmt.Sprintf("review-restart-new-%d", time.Now().UnixNano())
	plan, actors, plans := reviewMergeThenRestart(t, project, "fresh", 200*time.Millisecond)
	for _, id := range []string{"a", "b"} {
		assert.Equal(t, "merge:feature", actors[id], "fresh/%s", id)
		assert.Equal(t, plan.ID.Hex(), plans[id], "fresh/%s", id)
	}
}

// Capture stopped before the merge and started after it reads the whole
// transaction, marker included.
func TestReview_CaptureStartedAfterMergeAttributesIt(t *testing.T) {
	project := fmt.Sprintf("review-start-after-%d", time.Now().UnixNano())
	f := newIngestFixture(t, project)
	ctx := context.Background()
	stop := f.startIngester(t)
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)
	stop()

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch(project, "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, "fresh", bson.M{"_id": "a"})
	require.NoError(t, err)
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)

	restart := f.startIngester(t)
	defer restart()
	f.waitForEntries(t, "fresh", 1)
	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(main.ID, "fresh", main.BaseLSN+1, main.HeadLSN)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsData() {
			assert.Equal(t, "merge:feature", entry.Actor)
			assert.Equal(t, plan.ID.Hex(), entry.Metadata["merge_plan"])
		}
	}
	// The marker collection is never recorded as data.
	markerEntries, err := f.wal.GetBranchEntries(main.ID, "__argon_merge_markers", 0, main.HeadLSN)
	require.NoError(t, err)
	assert.Empty(t, markerEntries)
}
