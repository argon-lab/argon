package ingest

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
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

// The tracker answers two questions about a document at one cluster time:
// did it change earlier, and was that change's post-image read back.
func TestTimeStepRemembersADocumentsEarlierChange(t *testing.T) {
	first := primitive.Timestamp{T: 100, I: 1}
	second := primitive.Timestamp{T: 100, I: 2}
	attached := bson.Raw{5, 0, 0, 0, 0}

	var step timeStep
	if _, changed := step.earlier(first, "orders/a"); changed {
		t.Fatal("nothing has changed yet")
	}
	step.record("orders/a", stepDoc{post: attached})
	step.record("orders/b", stepDoc{readBack: true})

	a, changed := step.earlier(first, "orders/a")
	if !changed || a.readBack || len(a.post) == 0 {
		t.Fatalf("the earlier change and its attached post-image must be remembered: %+v %v", a, changed)
	}
	b, changed := step.earlier(first, "orders/b")
	if !changed || !b.readBack {
		t.Fatal("a read-back post-image must be remembered as such")
	}
	if _, changed := step.earlier(first, "orders/c"); changed {
		t.Fatal("documents are tracked independently")
	}
	if _, changed := step.earlier(second, "orders/a"); changed {
		t.Fatal("a new cluster time starts a new step")
	}
	if _, changed := step.earlier(second, "orders/b"); changed {
		t.Fatal("the previous step is forgotten as a whole")
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

func TestHasDuplicateFieldsLooksAtEveryDepth(t *testing.T) {
	doc := func(elements ...bson.E) bson.Raw {
		raw, err := bson.Marshal(bson.D(elements))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	wide := bson.D{}
	for i := range 40 {
		wide = append(wide, bson.E{Key: fmt.Sprintf("f%d", i), Value: i})
	}
	wideRaw, _ := bson.Marshal(wide)
	wideDup, _ := bson.Marshal(append(wide, bson.E{Key: "f3", Value: "again"}))

	for name, tc := range map[string]struct {
		raw  bson.Raw
		want bool
	}{
		"plain":            {doc(bson.E{Key: "a", Value: 1}, bson.E{Key: "b", Value: 2}), false},
		"top level":        {doc(bson.E{Key: "z", Value: "old"}, bson.E{Key: "b", Value: 2}, bson.E{Key: "z", Value: "new"}), true},
		"nested":           {doc(bson.E{Key: "a", Value: bson.D{{Key: "x", Value: 1}, {Key: "x", Value: 2}}}), true},
		"inside an array":  {doc(bson.E{Key: "a", Value: bson.A{bson.A{bson.D{{Key: "x", Value: 1}, {Key: "x", Value: 2}}}}}), true},
		"same name deeper": {doc(bson.E{Key: "a", Value: bson.D{{Key: "a", Value: 1}}}, bson.E{Key: "b", Value: bson.A{bson.D{{Key: "a", Value: 1}}}}), false},
		"wide":             {wideRaw, false},
		"wide duplicate":   {wideDup, true},
		"empty":            {bson.Raw{5, 0, 0, 0, 0}, false},
	} {
		if got := hasDuplicateFields(tc.raw); got != tc.want {
			t.Fatalf("%s: hasDuplicateFields = %v, want %v", name, got, tc.want)
		}
	}
}
