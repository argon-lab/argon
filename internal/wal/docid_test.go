package wal

import (
	"bytes"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestDocumentKeysPreserveBSONTypes(t *testing.T) {
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	ids := []interface{}{oid, oid.Hex(), int32(42), int64(42), "42", `{"$numberInt":"42"}`,
		"~argon:bson:reserved", "", primitive.Binary{Subtype: 4, Data: make([]byte, 16)},
		bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: "x"}}, nil}
	keys := map[string]bool{}
	for _, id := range ids {
		key := DocumentIDString(id)
		if keys[key] {
			t.Fatalf("colliding key for %T: %q", id, key)
		}
		keys[key] = true
		decoded, err := DocumentIDValue(key)
		if err != nil {
			t.Fatal(err)
		}
		before, err := bson.Marshal(bson.D{{Key: "id", Value: id}})
		if err != nil {
			t.Fatal(err)
		}
		after, err := bson.Marshal(bson.D{{Key: "id", Value: decoded}})
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("%T %v became %T %v (%v)", id, id, decoded, decoded, err)
		}
	}
}

func TestDocumentIDFromImageKeepsEmbeddedOrder(t *testing.T) {
	id := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}
	raw, _ := bson.Marshal(bson.D{{Key: "_id", Value: id}})
	recovered, err := DocumentIDFromImage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if DocumentIDString(recovered) != DocumentIDString(id) {
		t.Fatalf("embedded ID reordered: %#v", recovered)
	}
}
