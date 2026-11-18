package merge

import (
	"reflect"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestDeleteKeysRoundTrip(t *testing.T) {
	for _, id := range []interface{}{int32(42), int64(42), "507f1f77bcf86cd799439011"} {
		got, err := documentIDValue(Change{DocumentID: wal.DocumentIDString(id), Delete: true})
		if err != nil || !reflect.DeepEqual(got, id) {
			t.Fatalf("%T(%v) became %T(%v), err=%v", id, id, got, got, err)
		}
	}
}

func TestSavedPlanUsesOrderedDocumentID(t *testing.T) {
	id := bson.D{{Key: "zone", Value: "east"}, {Key: "bay", Value: int32(12)}}
	doc := bson.M{"_id": bson.M{"zone": "east", "bay": int32(12)}}
	key := wal.DocumentIDString(id)
	plan := &Plan{
		FormatVersion: 2,
		Changes:       []Change{{DocumentID: key, ID: doc["_id"], Document: doc}},
		Conflicts:     []Conflict{{DocumentID: key, Theirs: doc}},
	}
	changes, _, err := resolveChanges(plan, StrategyTheirs)
	require.NoError(t, err)
	require.Len(t, changes, 2)
	for _, change := range changes {
		got, err := documentIDValue(change)
		require.NoError(t, err)
		require.Equal(t, id, got)
	}
}
