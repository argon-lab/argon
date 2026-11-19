package wal

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUnmarshalDocumentRejectsNegativeBinaryLength(t *testing.T) {
	// {a: <binary>} whose binary length word is negative.
	raw := bson.Raw{
		13, 0, 0, 0,
		0x05, 'a', 0,
		0xff, 0xff, 0xff, 0xf0, // length
		0x00, // subtype
		0,
	}
	var doc bson.D
	if err := UnmarshalDocument(raw, &doc); err == nil {
		t.Fatalf("decoded an invalid binary value: %v", doc)
	}
}

func TestUnmarshalDocumentOldBinaryPayload(t *testing.T) {
	for _, payload := range [][]byte{{}, {0, 0, 0, 0}, {1, 2, 3}} {
		raw, err := bson.Marshal(bson.D{{Key: "a", Value: primitive.Binary{Subtype: bson.TypeBinaryBinaryOld, Data: payload}}})
		if err != nil {
			t.Fatal(err)
		}
		var doc bson.M
		if err := UnmarshalDocument(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if got := doc["a"].(primitive.Binary).Data; len(got) != len(payload) {
			t.Fatalf("payload of %d bytes decoded as %d", len(payload), len(got))
		}
	}
}
