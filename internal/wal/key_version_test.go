package wal

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestDocumentKeyVersion(t *testing.T) {
	old := primitive.Binary{Subtype: 2, Data: []byte{1, 2, 3}}
	ordinary := primitive.Binary{Subtype: 4, Data: make([]byte, 16)}
	for _, tc := range []struct {
		name    string
		id      any
		version int
	}{
		{"string", "catalog", 1},
		{"numeric", int64(13), 1},
		{"decimal", primitive.NewDecimal128(0, 42), 1},
		{"escaped", "~argon:bson:reserved", 1},
		{"uuid", ordinary, 1},
		{"ordinary compound", bson.D{{Key: "uuid", Value: ordinary}, {Key: "n", Value: 1}}, 1},
		{"binary", old, 2},
		{"empty binary", primitive.Binary{Subtype: 2, Data: []byte{}}, 2},
		{"nested", bson.D{{Key: "values", Value: bson.A{"x", old}}}, 2},
		{"scope", primitive.CodeWithScope{Code: "x", Scope: bson.D{{Key: "x", Value: old}}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := bson.Marshal(bson.D{{Key: "_id", Value: tc.id}, {Key: "other", Value: old}})
			require.NoError(t, err)
			key := DocumentIDString(tc.id)
			for _, entry := range []*Entry{
				{Operation: OpPut, PostImage: raw},
				{Operation: OpDelete, PreImage: raw},
				{Operation: OpDelete, DocumentID: key},
			} {
				require.NoError(t, entry.NormalizeDocumentKey())
				require.Equal(t, key, entry.DocumentID)
				require.Equal(t, tc.version, entry.DocumentKeyVersion)
			}
		})
	}
}

func TestOrdinaryScalarKeyVersionDoesNotAllocate(t *testing.T) {
	for _, id := range []any{"item", primitive.NewObjectID(), int64(19), "~argon:bson:escaped",
		primitive.Binary{Subtype: 0, Data: make([]byte, 8192)},
	} {
		e := Entry{Operation: OpDelete, DocumentID: DocumentIDString(id)}
		allocs := testing.AllocsPerRun(100, func() { _ = e.NormalizeDocumentKey() })
		require.Zero(t, allocs, "key type %T", id)
	}
}

func TestOrdinaryCompoundKeyUsesOriginalDecode(t *testing.T) {
	id := bson.D{{Key: "parts", Value: bson.A{int32(42), primitive.Binary{Subtype: 4, Data: make([]byte, 16)}}}}
	image, err := bson.Marshal(bson.M{"_id": id})
	require.NoError(t, err)
	entry := Entry{Operation: OpPut, PostImage: image}
	decodeAndEncode := testing.AllocsPerRun(100, func() {
		value, _ := DocumentIDFromImage(image)
		entry.DocumentID = DocumentIDString(value)
	})
	normalized := testing.AllocsPerRun(100, func() { _ = entry.NormalizeDocumentKey() })
	require.Equal(t, decodeAndEncode, normalized)
}
