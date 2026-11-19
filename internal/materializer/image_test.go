package materializer

import (
	"math"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestEditImageRequiresMatchingIdentityAndValue(t *testing.T) {
	id := bson.D{{Key: "z", Value: 1}, {Key: "a", Value: 2}}
	doc := bson.M{"_id": id, "!": 1, "x": 2}
	sorted := bson.D{{Key: "_id", Value: id}, {Key: "!", Value: 1}, {Key: "x", Value: 2}}
	want, err := wal.MarshalDocument(sorted)
	require.NoError(t, err)
	marshal := func(id any, x int) bson.Raw {
		raw, err := wal.MarshalDocument(bson.D{{Key: "_id", Value: id}, {Key: "x", Value: x}, {Key: "!", Value: 1}})
		require.NoError(t, err)
		return raw
	}
	for _, candidate := range []bson.Raw{
		marshal(bson.D{id[1], id[0]}, 2),
		marshal(id, 3),
		append(marshal(id, 2), 0),
	} {
		image, err := EditImage(doc, candidate, wal.SnapshotImage, nil)
		require.NoError(t, err)
		require.Equal(t, want, image)
	}
	raw := marshal(id, 2)
	got, err := EditImage(doc, raw, wal.SnapshotImage, nil)
	require.NoError(t, err)
	require.Equal(t, raw, got)
}

func TestEditImagePreservesNestedCodeScope(t *testing.T) {
	// Replay decodes the scope as an ordered document, but its embedded
	// documents as maps. Each must compare independently of map iteration.
	var inner bson.D
	for i, key := range []string{"zone", "row", "shelf", "quantity", "price", "source", "item", "bin", "name", "aisle"} {
		inner = append(inner, bson.E{Key: key, Value: int32(i)})
	}
	scope := bson.D{
		{Key: "product", Value: inner},
		{Key: "shelves", Value: bson.A{inner, inner, inner}},
	}
	marshal := func(scope bson.D, zero float64) bson.Raw {
		t.Helper()
		raw, err := wal.MarshalDocument(bson.D{
			{Key: "_id", Value: "q"}, {Key: "zero", Value: zero},
			{Key: "script", Value: primitive.CodeWithScope{Code: "product.price", Scope: scope}},
		})
		require.NoError(t, err)
		return raw
	}
	negativeZero := math.Copysign(0, -1)
	stored := marshal(scope, negativeZero)
	var doc bson.M
	require.NoError(t, wal.UnmarshalDocument(stored, &doc))
	got, err := EditImage(doc, stored, wal.SnapshotImage, nil)
	require.NoError(t, err)
	require.Equal(t, stored, got)

	sorted, err := wal.MarshalDocument(doc)
	require.NoError(t, err)
	for _, tc := range []struct {
		name      string
		candidate bson.Raw
	}{
		{"reordered scope", marshal(bson.D{scope[1], scope[0]}, negativeZero)},
		{"positive zero", marshal(scope, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EditImage(doc, tc.candidate, wal.SnapshotImage, nil)
			require.NoError(t, err)
			require.Equal(t, sorted, got)
		})
	}
}

func TestEditImageSourceEquality(t *testing.T) {
	fields := bson.D{{Key: "_id", Value: "q"}, {Key: "z", Value: 1}, {Key: "a", Value: 2}}
	raw, err := wal.MarshalDocument(fields)
	require.NoError(t, err)
	source, err := wal.DecodeImage(raw)
	require.NoError(t, err)
	image, decoded := source.TrustedImage(wal.SnapshotImage, nil)
	require.Same(t, source, decoded)
	require.Equal(t, raw, image)
	_, decoded = source.TrustedImage(wal.SnapshotImage, nil)
	require.Nil(t, decoded, "the stored bytes have been released")
	source, err = wal.DecodeImage(raw)
	require.NoError(t, err)
	got, err := editImage(source, source, wal.SnapshotImage, nil)
	require.NoError(t, err)
	require.Equal(t, raw, got)

	// The released map can change. Even an independently decoded candidate
	// with the original bytes now needs to agree with the new value.
	source, err = wal.DecodeImage(raw)
	require.NoError(t, err)
	source.TakeValue()["z"] = int32(3)
	_, decoded = source.TrustedImage(wal.SnapshotImage, nil)
	require.Nil(t, decoded)
	candidate, err := wal.DecodeImage(raw)
	require.NoError(t, err)
	want, err := wal.MarshalDocument(bson.D{fields[0], fields[2], {Key: "z", Value: int32(3)}})
	require.NoError(t, err)
	for _, old := range []*wal.DecodedImage{source, candidate} {
		got, err := editImage(source, old, wal.SnapshotImage, nil)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	repeated, err := wal.MarshalDocument(append(fields, bson.E{Key: "z", Value: int32(3)}))
	require.NoError(t, err)
	source, err = wal.DecodeImage(repeated)
	require.NoError(t, err)
	clean, rebuilt := source.TrustedImage(wal.SnapshotImage, nil)
	require.NotNil(t, rebuilt)
	require.NotSame(t, source, rebuilt)
	source, err = wal.DecodeImage(repeated)
	require.NoError(t, err)
	got, err = editImage(source, source, wal.SnapshotImage, nil)
	require.NoError(t, err)
	require.Equal(t, clean, got)

	absent := wal.DecodedValue(nil)
	got, err = editImage(absent, absent, wal.SnapshotImage, nil)
	require.NoError(t, err)
	require.Nil(t, got)
}
