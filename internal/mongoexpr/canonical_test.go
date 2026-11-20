package mongoexpr

import (
	"bytes"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestCanonicalBytesNestedCodeScope(t *testing.T) {
	fields := bson.M{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8, "i": 9, "j": 10}
	script := primitive.CodeWithScope{Code: "nested.a", Scope: bson.D{
		{Key: "nested", Value: fields},
		{Key: "copies", Value: bson.A{fields, fields}},
	}}
	image := bson.M{"scripts": bson.A{bson.M{"script": script}}}

	want, err := CanonicalBytes(image)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		got, err := CanonicalBytes(image)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("canonical bytes changed on iteration %d", i)
		}
	}
}
