package materializer

import (
	"testing"

	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestLegacyImagesRecoverDistinctDocumentIDs(t *testing.T) {
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	state := map[string]bson.M{}
	s := &Service{}
	for _, id := range []interface{}{oid, oid.Hex()} {
		raw, _ := bson.Marshal(bson.M{"_id": id})
		if err := s.ApplyEntry(state, &wal.Entry{SchemaVersion: 2, Operation: wal.OpPut, DocumentID: oid.Hex(), PostImage: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if len(state) != 2 {
		t.Fatalf("legacy images collapsed: %#v", state)
	}
	if err := s.ApplyEntry(state, &wal.Entry{SchemaVersion: 2, Operation: wal.OpDelete, DocumentID: oid.Hex()}); err == nil {
		t.Fatal("ambiguous legacy delete must fail closed")
	}
	pre, _ := bson.Marshal(bson.M{"_id": oid.Hex()})
	if err := s.ApplyEntry(state, &wal.Entry{SchemaVersion: 2, Operation: wal.OpDelete, DocumentID: oid.Hex(), PreImage: pre}); err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || state[wal.DocumentIDString(oid)] == nil {
		t.Fatalf("deleted wrong ID: %#v", state)
	}
}
