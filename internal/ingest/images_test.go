package ingest

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestPreviousTimestampIsStrictlyEarlier(t *testing.T) {
	for _, tc := range []struct {
		at, want primitive.Timestamp
		ok       bool
	}{
		{primitive.Timestamp{T: 10, I: 5}, primitive.Timestamp{T: 10, I: 4}, true},
		{primitive.Timestamp{T: 10, I: 1}, primitive.Timestamp{T: 10, I: 0}, true},
		{primitive.Timestamp{T: 10, I: 0}, primitive.Timestamp{T: 9, I: math.MaxUint32}, true},
		{primitive.Timestamp{}, primitive.Timestamp{}, false},
	} {
		got, ok := previousTimestamp(tc.at)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("previousTimestamp(%v) = %v, %v; want %v, %v", tc.at, got, ok, tc.want, tc.ok)
		}
		if ok && !got.Before(tc.at) {
			t.Fatalf("%v is not before %v", got, tc.at)
		}
	}
}

// A snapshot read describes one event only when that event is the document's
// sole change at its cluster time.
func TestTimeStepRefusesRecoveryForARepeatedDocument(t *testing.T) {
	first := primitive.Timestamp{T: 100, I: 1}
	second := primitive.Timestamp{T: 100, I: 2}

	var step timeStep
	if !step.admit(first, "orders/a", true) {
		t.Fatal("a document's only change at a time can be read back")
	}
	if !step.admit(first, "orders/b", true) {
		t.Fatal("another document at the same time is independent")
	}
	if step.admit(first, "orders/a", false) {
		t.Fatal("a later change at the same time means the earlier read-back was not that event's image")
	}

	step = timeStep{}
	for range 2 {
		if !step.admit(first, "orders/a", false) {
			t.Fatal("events that carry their own images are never refused")
		}
	}
	if step.admit(first, "orders/a", true) {
		t.Fatal("a read-back cannot isolate a change that follows another at the same time")
	}
	if !step.admit(second, "orders/a", true) {
		t.Fatal("a new cluster time starts a new step")
	}
}

func TestOnlyLastingSnapshotFailuresBecomeUnavailable(t *testing.T) {
	for _, code := range []int32{239, 246, 72} {
		if !snapshotGoneCodes[code] {
			t.Fatalf("code %d must be treated as an unavailable snapshot", code)
		}
	}
	// A step-down or a timeout must reach the retry loop unchanged.
	for _, transient := range []mongo.CommandError{
		{Code: 10107, Name: "NotWritablePrimary"},
		{Code: 11602, Name: "InterruptedDueToReplStateChange"},
		{Code: 50, Name: "MaxTimeMSExpired"},
	} {
		if snapshotGoneCodes[transient.Code] {
			t.Fatalf("%s is transient and must be retried", transient.Name)
		}
	}
	wrapped := fmt.Errorf("%w: too old", errSnapshotGone)
	if !errors.Is(wrapped, errSnapshotGone) {
		t.Fatal("the reason must not hide the sentinel")
	}
}
