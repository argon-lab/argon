package materializer

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// EntryStateKey must name the key ApplyEntry writes or removes: callers use
// it to group a branch's entries by document without folding them.
func TestEntryStateKeyMatchesApplyEntry(t *testing.T) {
	oid, _ := primitive.ObjectIDFromHex("507f1f77bcf86cd799439011")
	ids := []interface{}{
		"plain",
		oid,
		oid.Hex(), // a string that reads as an ObjectID
		int32(7),
		int64(7),
		"{\"$numberInt\":\"7\"}",
		bson.D{{Key: "b", Value: 1}, {Key: "a", Value: 2}},
		bson.D{{Key: "a", Value: 2}, {Key: "b", Value: 1}},
	}
	s := &Service{}

	seen := map[string]bool{}
	for _, id := range ids {
		image, err := bson.Marshal(bson.D{{Key: "_id", Value: id}, {Key: "v", Value: 1}})
		if err != nil {
			t.Fatal(err)
		}

		put := &wal.Entry{SchemaVersion: 2, Operation: wal.OpPut, DocumentID: wal.DocumentIDString(id), DocumentKeyVersion: 1, PostImage: image}
		key, ok := EntryStateKey(put)
		if !ok {
			t.Fatalf("put of %#v: no key", id)
		}
		// The image decides, whatever key the envelope carries.
		legacyPut := &wal.Entry{SchemaVersion: 2, Operation: wal.OpPut, DocumentID: wal.LegacyDocumentIDString(id), PostImage: image}
		if legacyKey, ok := EntryStateKey(legacyPut); !ok || legacyKey != key {
			t.Fatalf("put of %#v under its legacy key: key %q ok=%v, want %q", id, legacyKey, ok, key)
		}
		state := map[string]bson.M{}
		if err := s.ApplyEntry(state, put); err != nil {
			t.Fatal(err)
		}
		if _, stored := state[key]; !stored || len(state) != 1 {
			t.Fatalf("put of %#v: key %q, state %#v", id, key, state)
		}
		if seen[key] {
			t.Fatalf("key %q names two different IDs", key)
		}
		seen[key] = true

		// A delete with a pre-image, and one written with a current key and
		// no image, both remove that same key.
		for _, del := range []*wal.Entry{
			{SchemaVersion: 2, Operation: wal.OpDelete, DocumentID: "stale-envelope-key", DocumentKeyVersion: 1, PreImage: image},
			{SchemaVersion: 2, Operation: wal.OpDelete, DocumentID: wal.DocumentIDString(id), DocumentKeyVersion: 1},
		} {
			delKey, ok := EntryStateKey(del)
			if !ok || delKey != key {
				t.Fatalf("delete of %#v: key %q ok=%v, want %q", id, delKey, ok, key)
			}
			after := map[string]bson.M{key: state[key]}
			if err := s.ApplyEntry(after, del); err != nil {
				t.Fatal(err)
			}
			if len(after) != 0 {
				t.Fatalf("delete of %#v left %#v", id, after)
			}
		}
	}

	// A put whose image carries no _id is stored under the envelope key.
	noID, _ := bson.Marshal(bson.M{"v": 1})
	early := &wal.Entry{SchemaVersion: 2, Operation: wal.OpPut, DocumentID: "envelope", PostImage: noID}
	if key, ok := EntryStateKey(early); !ok || key != "envelope" {
		t.Fatalf("image without _id: key %q ok=%v", key, ok)
	}
	state := map[string]bson.M{}
	if err := s.ApplyEntry(state, early); err != nil {
		t.Fatal(err)
	}
	if _, stored := state["envelope"]; !stored {
		t.Fatalf("image without _id: state %#v", state)
	}

	// Entries the fold cannot place from the entry alone report no key.
	for name, entry := range map[string]*wal.Entry{
		"legacy delete without a pre-image": {SchemaVersion: 2, Operation: wal.OpDelete, DocumentID: "x"},
		"schema-v1 update":                  {SchemaVersion: 1, Operation: wal.LegacyOpUpdate, DocumentID: "x"},
		"schema-v1 delete":                  {SchemaVersion: 1, Operation: wal.OpDelete, DocumentID: "x", DocumentKeyVersion: 1},
		"put without a document ID":         {SchemaVersion: 2, Operation: wal.OpPut, PostImage: noID},
		"control entry":                     {SchemaVersion: 2, Operation: wal.OpMerge},
	} {
		if key, ok := EntryStateKey(entry); ok {
			t.Fatalf("%s: got key %q, want none", name, key)
		}
	}
}
