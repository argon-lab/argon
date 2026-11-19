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
	expected, err := bson.Marshal(bson.D{{Key: "aisle", Value: int32(4)}, {Key: "zone", Value: int32(5)}})
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expected), scope)
	require.Equal(t, []string{"theirs"}, sorted)
}
