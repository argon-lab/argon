package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestWriteBack_KeepsArrayElementFieldOrder(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	first := bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}
	second := bson.D{{Key: "b", Value: int32(3)}, {Key: "a", Value: int32(4)}}
	secondReordered := bson.D{{Key: "a", Value: int32(4)}, {Key: "b", Value: int32(3)}}
	added := bson.D{{Key: "a", Value: int32(9)}, {Key: "b", Value: int32(9)}}
	identified := bson.D{{Key: "b", Value: int32(7)}, {Key: "_id", Value: "line"}, {Key: "a", Value: int32(8)}}
	edited := bson.D{{Key: "_id", Value: "line"}, {Key: "a", Value: int32(9)}, {Key: "b", Value: int32(7)}}
	editedInSourceOrder := bson.D{{Key: "b", Value: int32(7)}, {Key: "_id", Value: "line"}, {Key: "a", Value: int32(9)}}
	for name, tc := range map[string]struct {
		original, want, expected bson.A
	}{
		"change in place":          {bson.A{first, second}, bson.A{added, second}, bson.A{added, second}},
		"remove the first element": {bson.A{first, second}, bson.A{second}, bson.A{second}},
		"insert at the front":      {bson.A{second, first}, bson.A{added, second, first}, bson.A{added, second, first}},
		"move a reordered element": {bson.A{first, second}, bson.A{secondReordered, first}, bson.A{second, first}},
		"move and edit an element with an ID": {
			bson.A{identified, second}, bson.A{second, edited}, bson.A{second, editedInSourceOrder},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFollowFixture(t, metadataDeploymentURI)
			coll := f.source.Collection("carts")
			_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "1"}, {Key: "items", Value: tc.original}})
			require.NoError(t, err)
			f.importOnline(t, walcli.OnlineImport{})

			info, db := f.agentSandbox(t, "agent-run")
			_, err = db.Collection("carts").UpdateOne(ctx, bson.M{"_id": "1"}, bson.M{"$set": bson.M{"items": tc.want}})
			require.NoError(t, err)

			plan := f.preview(t, info.BranchID)
			useFormat2(t, f.services.Client.Database(f.metadataName), plan)
			require.Len(t, plan.Changes, 1)
			_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
			require.NoError(t, err)

			var got bson.D
			require.NoError(t, coll.FindOne(ctx, bson.M{"_id": "1"}).Decode(&got))
			require.Equal(t, bson.D{{Key: "_id", Value: "1"}, {Key: "items", Value: tc.expected}}, got)
			n, err := coll.CountDocuments(ctx, bson.M{"items": second})
			require.NoError(t, err)
			require.EqualValues(t, 1, n, "an exact embedded-document match still finds the untouched element")
		})
	}
}
