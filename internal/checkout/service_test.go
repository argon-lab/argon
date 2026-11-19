package checkout

import (
	"errors"
	"math"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func rawReply(t *testing.T, doc bson.D) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	return raw
}

func TestCaptureBoundary(t *testing.T) {
	tests := []struct {
		name   string
		reply  bson.D
		want   primitive.Timestamp
		wantOK bool
	}{
		{
			name: "replica set reply starts one increment later",
			reply: bson.D{
				{Key: "ok", Value: 1.0},
				{Key: "operationTime", Value: primitive.Timestamp{T: 1700000000, I: 7}},
			},
			want:   primitive.Timestamp{T: 1700000000, I: 8},
			wantOK: true,
		},
		{
			name: "increment overflow moves to the next second",
			reply: bson.D{
				{Key: "ok", Value: 1.0},
				{Key: "operationTime", Value: primitive.Timestamp{T: 1700000000, I: math.MaxUint32}},
			},
			want:   primitive.Timestamp{T: 1700000001, I: 0},
			wantOK: true,
		},
		{
			// What an Atlas free cluster answers to ping, and what a
			// standalone server answers to hello.
			name:   "reply without operationTime has no boundary",
			reply:  bson.D{{Key: "ok", Value: 1.0}},
			wantOK: false,
		},
		{
			name: "operationTime of another type has no boundary",
			reply: bson.D{
				{Key: "ok", Value: 1.0},
				{Key: "operationTime", Value: "1700000000"},
			},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := captureBoundary(rawReply(t, tt.reply))
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("captureBoundary() = %v, %v; want %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestBoundaryFromReply(t *testing.T) {
	t.Run("reply without operationTime names it and wraps the sentinel", func(t *testing.T) {
		_, err := boundaryFromReply(rawReply(t, bson.D{{Key: "ok", Value: 1.0}}))
		if !errors.Is(err, ErrNoCaptureBoundary) {
			t.Fatalf("error = %v; want ErrNoCaptureBoundary", err)
		}
		for _, want := range []string{"operationTime", "replica set"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
	t.Run("reply with operationTime returns the boundary", func(t *testing.T) {
		got, err := boundaryFromReply(rawReply(t, bson.D{
			{Key: "ok", Value: 1.0},
			{Key: "operationTime", Value: primitive.Timestamp{T: 1700000000, I: 7}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if want := (primitive.Timestamp{T: 1700000000, I: 8}); got != want {
			t.Fatalf("boundary = %v; want %v", got, want)
		}
	})
}
