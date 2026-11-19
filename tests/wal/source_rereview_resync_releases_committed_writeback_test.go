package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A write-back commits on the source while the follower is stopped behind a
// collection drop it has not reached yet. Recording the commit fails, so the
// plan stays writing_back. Following then ends degraded at the drop and a
// resync skips the stream range that holds the write-back's transaction.
//
// The plan stays claimed even after resync closes the gap.
func TestResync_DoesNotReleaseCommittedWriteBack(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("archive").InsertOne(ctx, bson.M{"_id": "a", "note": "old"})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	// The follower is not running. The source drops a collection.
	require.NoError(t, f.source.Collection("archive").Drop(ctx))

	// Write-back is allowed while the follower is stopped.
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	_, err = sandboxDB.Collection("catalog").InsertOne(ctx, bson.D{{Key: "_id", Value: "item-new"}, {Key: "name", Value: "from the plan"}})
	require.NoError(t, err)
	plan := f.preview(t, info.BranchID)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.NoError(t, err)
	var onSource bson.M
	require.NoError(t, f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "item-new"}).Decode(&onSource), "the write-back committed on the source")

	// What a crash between the source commit and the plan update leaves
	// behind (as in TestWriteBack_CommitWithoutRecordIsSettledByFollowing).
	// The claim is backdated as if the resync ran half an hour later.
	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	_, err = plans.UpdateOne(ctx, bson.M{"_id": plan.ID}, bson.M{
		"$set":   bson.M{"status": merge.StatusWritingBack, "write_back.started_at": time.Now().Add(-30 * time.Minute)},
		"$unset": bson.M{"applied_at": "", "strategy": "", "write_back.committed_at": ""},
	})
	require.NoError(t, err)
	claimed := f.plan(t, plan)
	require.Equal(t, merge.StatusWritingBack, claimed.Status)
	require.NotNil(t, claimed.WriteBack)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.ErrorContains(t, err, "will not be written again")
	require.ErrorContains(t, err, "Check the source database")
	require.ErrorContains(t, err, "argon merge preview")

	// Following reaches the drop and ends degraded, before the write-back's
	// transaction in the stream.
	before := f.degrade(t, "drop of archive")
	require.Equal(t, merge.StatusWritingBack, f.plan(t, plan).Status, "still claimed after the degraded follower")

	result := f.resync(t, f.services, walcli.ResyncSourceOptions{})
	f.requireResynced(t, before, result)
	require.Contains(t, f.mainState(t, result.ConsistentLSN)["catalog"], "item-new", "the resync absorbed the write-back's document")

	after := f.plan(t, plan)
	require.Equal(t, merge.StatusWritingBack, after.Status, "a committed write-back in the resync gap must stay writing_back")
	require.Equal(t, claimed.WriteBack, after.WriteBack)
	require.Empty(t, after.WriteBackLastError)

	// Production later removes the document the plan inserted.
	_, err = f.source.Collection("catalog").DeleteOne(ctx, bson.M{"_id": "item-new"})
	require.NoError(t, err)
	_, againErr := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	count, err := f.source.Collection("catalog").CountDocuments(ctx, bson.M{"_id": "item-new"})
	require.NoError(t, err)

	require.ErrorContains(t, againErr, "will not be written again")
	require.Zero(t, count, "the document production deleted must not come back")
}
