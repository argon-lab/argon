package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestWriteBack_ReservesExactArrayMatchesBeforeFallback(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	first := bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}
	survivor := bson.D{{Key: "b", Value: int32(2)}, {Key: "a", Value: int32(1)}}
	identified := bson.D{{Key: "_id", Value: "line"}, {Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}
	other := bson.D{{Key: "_id", Value: "line"}, {Key: "b", Value: int32(3)}, {Key: "a", Value: int32(4)}}
	edited := bson.D{{Key: "_id", Value: "line"}, {Key: "a", Value: int32(4)}, {Key: "b", Value: int32(5)}}
	editedInSourceOrder := bson.D{{Key: "_id", Value: "line"}, {Key: "b", Value: int32(5)}, {Key: "a", Value: int32(4)}}
	third := bson.D{{Key: "c", Value: int32(3)}, {Key: "b", Value: int32(2)}, {Key: "a", Value: int32(1)}}
	// The first cart keeps one of two BSON-distinct elements with equal
	// decoded values. The second has an early fallback try to use a later
	// exact match. The third has two plausible matches and keeps plan order.
	cases := []struct {
		id                      string
		original, want, applied bson.A
	}{
		{"survivor", bson.A{first, survivor}, bson.A{survivor}, bson.A{survivor}},
		{"reserve", bson.A{identified, other}, bson.A{edited, identified}, bson.A{editedInSourceOrder, identified}},
		{"ambiguous", bson.A{
			append(append(bson.D{}, first...), bson.E{Key: "c", Value: int32(3)}),
			append(append(bson.D{}, survivor...), bson.E{Key: "c", Value: int32(3)}),
		}, bson.A{third}, bson.A{third}},
	}
	carts := f.source.Collection("carts")
	for _, cart := range cases {
		_, err := carts.InsertOne(ctx, bson.D{{Key: "_id", Value: cart.id}, {Key: "items", Value: cart.original}})
		require.NoError(t, err)
	}
	f.importOnline(t, walcli.OnlineImport{})

	info, sandbox := f.agentSandbox(t, "cart-edits")
	for _, cart := range cases {
		_, err := sandbox.Collection("carts").UpdateByID(ctx, cart.id, bson.M{"$set": bson.M{"items": cart.want}})
		require.NoError(t, err)
	}
	match := bson.M{"_id": "survivor", "items": survivor}
	before, err := sandbox.Collection("carts").CountDocuments(ctx, match)
	require.NoError(t, err)
	require.EqualValues(t, 1, before)
	plan := f.preview(t, info.BranchID)
	useFormat2(t, f.services.Client.Database(f.metadataName), plan)
	require.Len(t, plan.Changes, len(cases))
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Equal(t, len(cases), result.Applied)

	for _, cart := range cases {
		var got bson.D
		require.NoError(t, carts.FindOne(ctx, bson.M{"_id": cart.id}).Decode(&got))
		require.Equal(t, bson.D{{Key: "_id", Value: cart.id}, {Key: "items", Value: cart.applied}}, got)
	}
	after, err := carts.CountDocuments(ctx, match)
	require.NoError(t, err)
	require.EqualValues(t, 1, after)
}
