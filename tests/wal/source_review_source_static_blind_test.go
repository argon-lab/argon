package wal_test

import (
	"context"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Proposed (b): the audit stream is open while production writes are followed
// and a write-back is refused and applied. Everything in it must be a
// document write the test made or one of the plan's four documents.
func TestSource_OnlyPlannedDocumentsChangeWhileFollowingAndWritingBack(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	audit := f.audit(t)

	catalog := f.source.Collection("catalog")
	const own = 6
	for i := 0; i < own; i++ {
		_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-15"}, bson.M{"$inc": bson.M{"stock": 1}})
		require.NoError(t, err)
	}
	f.waitCaughtUp(t)

	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 4)

	second, secondDB := f.agentSandbox(t, "agent-two")
	_, err := secondDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-11"}, bson.M{"$set": bson.M{"stock": int32(500)}})
	require.NoError(t, err)
	refused := f.preview(t, second.BranchID)
	_, err = catalog.UpdateOne(ctx, bson.M{"_id": "item-11"}, bson.M{"$set": bson.M{"stock": int32(7)}})
	require.NoError(t, err)
	_, err = f.services.WriteBackMerge(ctx, refused.ID, "", f.sourceURI)
	require.Error(t, err)

	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Equal(t, 4, result.Applied)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after following and the write-back")

	seen := audit.operations(t)
	t.Logf("audited source events: %v", seen)
	total := 0
	for key, count := range seen {
		parts := strings.SplitN(key, " ", 2)
		require.Contains(t, []string{"insert", "update", "replace", "delete"}, parts[0], "Argon issued %q on the source", key)
		require.Equal(t, "catalog", parts[1], "Argon wrote to %q on the source", key)
		total += count
	}
	require.Equal(t, own+1+4, total, "only the test's own writes and the plan's four documents: %v", seen)
}
