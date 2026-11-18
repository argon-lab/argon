package wal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// The plan deletes catalog/item-03. Production changes that document after
// the review. Write-back must refuse, leave production's document in place,
// and return the plan to pending.
func TestWriteBack_DeleteOverAChangedDocumentIsAConflict(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	cases := []struct {
		name  string
		cause func(t *testing.T, f *followFixture)
		want  bson.M
	}{
		{"production updated a document the plan deletes", func(t *testing.T, f *followFixture) {
			_, err := f.source.Collection("catalog").UpdateOne(context.Background(), bson.M{"_id": "item-03"}, bson.M{"$set": bson.M{"stock": int32(99)}})
			require.NoError(t, err)
		}, bson.M{"_id": "item-03", "name": "Item 3", "stock": int32(99)}},
		{"production deleted and re-inserted it with different content", func(t *testing.T, f *followFixture) {
			ctx := context.Background()
			_, err := f.source.Collection("catalog").DeleteOne(ctx, bson.M{"_id": "item-03"})
			require.NoError(t, err)
			_, err = f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "item-03", "made_by": "production"})
			require.NoError(t, err)
		}, bson.M{"_id": "item-03", "made_by": "production"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFollowFixture(t, metadataDeploymentURI)
			f.seedSmall(t)
			f.importOnline(t, walcli.OnlineImport{})
			info, sandboxDB := f.agentSandbox(t, "agent-run")
			agentEdits(t, sandboxDB)
			plan := f.preview(t, info.BranchID)
			require.Len(t, plan.Changes, 4)

			tc.cause(t, f)
			before := f.sourceState(t, nil)

			_, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
			var got bson.M
			findErr := f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "item-03"}).Decode(&got)
			t.Logf("write-back error: %v; item-03 in source afterwards: %v (find error: %v)", err, got, findErr)

			var stale *merge.ConflictError
			require.True(t, errors.As(err, &stale), "want a conflict, got %v", err)
			require.NoError(t, findErr, "production's item-03 must still exist")
			require.Equal(t, tc.want, got)
			require.Equal(t, before, f.sourceState(t, nil), "the source must be untouched")
			require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
		})
	}
}
