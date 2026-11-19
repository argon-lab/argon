package wal

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestNormalizeEditImagePreservesDocumentIdentityAndCodeScope(t *testing.T) {
	id := bson.D{{Key: "part", Value: 1}, {Key: "part", Value: 2}}
	code := primitive.CodeWithScope{Code: "count", Scope: bson.D{{Key: "count", Value: 1}, {Key: "count", Value: 2}}}
	history := bson.D{
		{Key: "_id", Value: id}, {Key: "scope", Value: code},
		{Key: "items", Value: bson.A{bson.D{{Key: "z", Value: 1}, {Key: "a", Value: 2}, {Key: "z", Value: 3}}}},
		{Key: "_id", Value: "obsolete"},
	}
	raw, err := bson.Marshal(history)
	require.NoError(t, err)
	got, rebuilt, err := NormalizeEditImage(raw)
	require.NoError(t, err)
	require.True(t, rebuilt)
	want, err := bson.Marshal(bson.D{
		{Key: "_id", Value: id}, {Key: "scope", Value: code},
		{Key: "items", Value: bson.A{bson.D{{Key: "z", Value: 3}, {Key: "a", Value: 2}}}},
	})
	require.NoError(t, err)
	require.Equal(t, bson.Raw(want), got)
}

func TestNormalizeEditImageReportsDecodeError(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		fields := bson.D{{Key: "_id", Value: "d"}, {Key: "data", Value: primitive.Binary{Subtype: 2, Data: []byte{'a', 'b'}}}}
		if repeated {
			fields = append(fields, bson.E{Key: "data", Value: true})
		}
		raw, err := bson.Marshal(fields)
		require.NoError(t, err)
		bson.Raw(raw).Lookup("data").Value[5] = 1
		var doc bson.M
		require.ErrorIs(t, UnmarshalDocument(raw, &doc), ErrOldBinaryLength)
		_, _, err = NormalizeEditImage(raw)
		require.ErrorIs(t, err, ErrOldBinaryLength)
	}
}
