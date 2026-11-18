package merge

import (
	"encoding/json"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestPlanJSONCompoundKeysAndConflictImages(t *testing.T) {
	id := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int64(2)}}
	doc := bson.M{"_id": bson.M{"a": int64(2), "z": int32(1)}, "count": int64(8)}
	key := wal.DocumentIDString(id)
	plan := &Plan{
		FormatVersion: 2,
		Changes:       []Change{{DocumentID: key, Before: doc, ID: doc["_id"], Delete: true}},
		Conflicts:     []Conflict{{DocumentID: key, Base: doc, Ours: doc, Theirs: doc}},
	}
	output, err := plan.CLIJSON()
	require.NoError(t, err)
	type image struct {
		ID    json.RawMessage `json:"_id"`
		Count json.RawMessage
	}
	var decoded struct {
		Changes []struct {
			ID     json.RawMessage `json:"document_key"`
			Before image
		}
		Conflicts []struct {
			ID                 json.RawMessage `json:"document_id"`
			Base, Ours, Theirs image
		}
	}
	require.NoError(t, json.Unmarshal(output, &decoded))
	want := `{"z":{"$numberInt":"1"},"a":{"$numberLong":"2"}}`
	require.Equal(t, want, string(decoded.Changes[0].ID))
	conflict := decoded.Conflicts[0]
	require.Equal(t, want, string(conflict.ID))
	for _, side := range []image{decoded.Changes[0].Before, conflict.Base, conflict.Ours, conflict.Theirs} {
		require.Equal(t, want, string(side.ID))
		require.JSONEq(t, `{"$numberLong":"8"}`, string(side.Count))
	}
}
