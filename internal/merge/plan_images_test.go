package merge

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSortedImageWithCodeScope(t *testing.T) {
	id := bson.D{{Key: "zone", Value: int32(1)}, {Key: "aisle", Value: int32(2)}}
	doc := bson.M{
		"_id": id,
		"scripts": bson.A{primitive.CodeWithScope{
			Code: "value", Scope: bson.D{
				{Key: "zone", Value: int32(3)}, {Key: "aisle", Value: int32(4)}, {Key: "zone", Value: int32(5)},
			},
		}},
	}
	var raw bson.Raw
	var sorted []string
	image := imageRequest{key: wal.DocumentIDString(id), name: "theirs", doc: doc, raw: &raw, sorted: &sorted}
	require.NoError(t, image.store(nil, false, false))
	expectedID, err := bson.Marshal(id)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expectedID), raw.Lookup("_id").Document())
	_, scope := raw.Lookup("scripts", "0").CodeWithScope()
	expected, err := bson.Marshal(doc["scripts"].(bson.A)[0].(primitive.CodeWithScope).Scope)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expected), scope)
	require.Equal(t, []string{"theirs"}, sorted)
}

func TestStoredImageWithNestedCodeScope(t *testing.T) {
	nested := bson.D{{Key: "z", Value: 1}, {Key: "a", Value: 2}, {Key: "m", Value: 3}}
	scope := bson.D{
		{Key: "row", Value: nested},
		{Key: "items", Value: bson.A{nested, nested, nested}},
		{Key: "row", Value: nested},
	}
	stored, err := wal.MarshalDocument(bson.D{
		{Key: "_id", Value: "one"},
		{Key: "scripts", Value: bson.A{primitive.CodeWithScope{Code: "row.z", Scope: scope}}},
	})
	require.NoError(t, err)
	var doc bson.M
	require.NoError(t, wal.UnmarshalDocument(stored, &doc))
	entry := &wal.Entry{Operation: wal.OpPut, PostImage: stored}
	for i := range 200 {
		var raw bson.Raw
		var sorted []string
		image := imageRequest{key: "one", name: "document", doc: doc, raw: &raw, sorted: &sorted}
		require.NoError(t, image.store(entry, true, true))
		require.Empty(t, sorted, "iteration %d", i)
		require.Equal(t, stored, raw, "iteration %d", i)
	}
}

func TestOrdinaryImageRetainsFieldPositions(t *testing.T) {
	original := bson.D{
		{Key: "_id", Value: "order"}, {Key: "z", Value: int32(1)},
		{Key: "items", Value: bson.A{bson.D{
			{Key: "z", Value: 0}, {Key: "v", Value: int32(10)}, {Key: "a", Value: 1}, {Key: "v", Value: int64(11)},
		}}}, {Key: "a", Value: int32(2)}, {Key: "z", Value: int64(3)}, {Key: "_id", Value: "other"},
	}
	input, err := bson.Marshal(original)
	require.NoError(t, err)
	actual, rebuilt, err := ordinaryImage(input)
	require.NoError(t, err)
	require.True(t, rebuilt)
	want, err := bson.Marshal(bson.D{
		{Key: "_id", Value: "order"}, {Key: "z", Value: int64(3)},
		{Key: "items", Value: bson.A{bson.D{
			{Key: "z", Value: 0}, {Key: "v", Value: int64(11)}, {Key: "a", Value: 1},
		}}}, {Key: "a", Value: int32(2)},
	})
	require.NoError(t, err)
	require.Equal(t, bson.Raw(want), actual)
}
