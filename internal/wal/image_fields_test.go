package wal

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestTrustedImageRequiresCanonicalBSON(t *testing.T) {
	marshal := func(fields bson.D) bson.Raw {
		t.Helper()
		raw, err := bson.Marshal(fields)
		require.NoError(t, err)
		return raw
	}
	document := func(fields ...bson.E) bson.Raw {
		return marshal(append(bson.D{{Key: "_id", Value: "q"}}, fields...))
	}
	withValue := func(value any) bson.Raw { return document(bson.E{Key: "x", Value: value}) }
	scope := func(fields bson.D) primitive.CodeWithScope {
		return primitive.CodeWithScope{Code: "item", Scope: fields}
	}

	duplicate := bson.D{{Key: "k", Value: "old"}, {Key: "k", Value: "current"}}
	shortBinary := bson.RawValue{Type: bson.TypeBinary, Value: []byte{1, 0, 0, 0, 2, 'A'}}
	misplaced := withValue(bson.A{int32(7)})
	// The array's first element has its type at byte 4 and key at byte 5.
	misplaced.Lookup("x").Array()[5] = '1'
	regex := bson.RawValue{Type: bson.TypeRegex, Value: []byte{'a', 0, 'm', 'i', 0}}
	badCode := withValue(scope(bson.D{}))
	// The code reader consumes the last string byte as a terminator.
	badCode.Lookup("x").Value[8+len("item")] = '!'

	for _, test := range []struct {
		name string
		raw  bson.Raw
	}{
		{"repeated field", document(duplicate...)},
		{"repeated _id", document(bson.E{Key: "_id", Value: "later"})},
		{"repeated compound ID field", marshal(bson.D{{Key: "_id", Value: duplicate}})},
		{"repeated embedded field", withValue(duplicate)},
		{"repeated field in array element", withValue(bson.A{duplicate})},
		{"repeated scope field", withValue(scope(duplicate))},
		{"repeated field in scope document", withValue(scope(bson.D{{Key: "nested", Value: duplicate}}))},
		{"short subtype 2", withValue(shortBinary)},
		{"array labels", misplaced},
		{"regex options", withValue(regex)},
		{"code terminator", badCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := DecodeImage(test.raw)
			require.NoError(t, err)
			raw, trusted := decoded.TrustedImage(SnapshotImage, nil)
			require.Nil(t, raw)
			require.Nil(t, trusted)
		})
	}

	t.Run("canonical", func(t *testing.T) {
		inner := bson.D{
			{Key: "z", Value: math.Float64frombits(0x7ff8000000000042)},
			{Key: "a", Value: primitive.Binary{Subtype: 2, Data: []byte{'A'}}},
		}
		fields := bson.D{{Key: "z", Value: inner}, {Key: "a", Value: bson.A{inner}}}
		stored := document(bson.E{Key: "z", Value: fields}, bson.E{Key: "a", Value: scope(fields)})
		decoded, err := DecodeImage(stored)
		require.NoError(t, err)
		raw, trusted := decoded.TrustedImage(SnapshotImage, nil)
		require.Same(t, decoded, trusted)
		require.Equal(t, stored, raw)
		require.Same(t, &stored[0], &raw[0])
	})
}

func TestDecodeImageRejectsInconsistentBinaryLength(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		fields := bson.D{{Key: "_id", Value: "d"}, {Key: "data", Value: primitive.Binary{Subtype: 2, Data: []byte{'a', 'b'}}}}
		if repeated {
			fields = append(fields, bson.E{Key: "data", Value: true})
		}
		raw, err := bson.Marshal(fields)
		require.NoError(t, err)
		bson.Raw(raw).Lookup("data").Value[5] = 1
		_, err = DecodeImage(raw)
		require.ErrorIs(t, err, ErrOldBinaryLength)
	}
}
