package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
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

func TestUnmarshalOldBinaryLengths(t *testing.T) {
	for _, count := range []uint32{0, 1, 3, 0xffffffff} {
		raw, err := bson.Marshal(bson.M{"bin": primitive.Binary{Subtype: 2, Data: []byte{'a', 'b'}}})
		if err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint32(bson.Raw(raw).Lookup("bin").Value[5:], count)
		var doc bson.M
		if err := UnmarshalDocument(raw, &doc); !errors.Is(err, ErrOldBinaryLength) {
			t.Fatalf("inner count %d for two bytes: got %v", count, err)
		}
	}
	// In old WALs short payloads can have no inner count at all.
	for _, payload := range [][]byte{{}, {1}, {1, 2}, {1, 2, 3}, {1, 2, 3, 4}} {
		value := bson.RawValue{Type: bson.TypeBinary, Value: append([]byte{byte(len(payload)), 0, 0, 0, 2}, payload...)}
		var decoded primitive.Binary
		if err := UnmarshalValue(value, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Subtype != 2 || !bytes.Equal(payload, decoded.Data) {
			t.Fatalf("lengthless payload %x decoded as %+v", payload, decoded)
		}
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
