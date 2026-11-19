package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestWriteBack_TargetHistoryReclaimedAfterPreview(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	sandbox, sandboxDB := f.agentSandbox(t, "stock-edits")
	_, err := sandboxDB.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(99)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	require.Len(t, plan.Changes, 1)
	// Remove the target WAL entry after its verified image was saved in the plan.
	require.NoError(t, f.services.WAL.RecordReclaimFloor(ctx, main.ID, "catalog", plan.TargetHead))
	history, err := f.services.WAL.GetDocumentHistory(main.ID, "catalog", "item-00", main.BaseLSN+1, plan.TargetHead)
	require.NoError(t, err)
	require.NotEmpty(t, history)

	report, err := f.services.RunGC(ctx, main.ProjectID, 0, false)
	require.NoError(t, err)
	require.Positive(t, report.EntriesRemoved)
	history, err = f.services.WAL.GetDocumentHistory(main.ID, "catalog", "item-00", main.BaseLSN+1, plan.TargetHead)
	require.NoError(t, err)
	require.Empty(t, history)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	after, err := f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "item-00"}).Raw()
	require.NoError(t, err)
	require.Equal(t, plan.Changes[0].DocumentRaw, after)
}
