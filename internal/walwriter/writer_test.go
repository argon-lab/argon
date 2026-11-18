package walwriter

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

type orderedPayload struct{}

func (orderedPayload) MarshalBSON() ([]byte, error) {
	return bson.Marshal(bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}})
}

func TestNormalizeDocFieldOrder(t *testing.T) {
	ordered := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}
	sorted := bson.D{{Key: "a", Value: int32(2)}, {Key: "z", Value: int32(1)}}
	unordered := bson.M{"a": int32(2), "z": int32(1)}
	raw, err := bson.Marshal(ordered)
	require.NoError(t, err)
	type fields struct {
		Z     int32
		A     map[string]any
		Extra bson.M `bson:",inline"`
	}

	for _, tc := range []struct {
		name   string
		id     any
		wantID bson.D
	}{
		{"ordered compound key", ordered, ordered},
		{"unordered compound key", unordered, sorted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := bson.M{
				"_id": tc.id, "ordered": ordered, "raw": bson.Raw(raw),
				"custom": orderedPayload{},
				"array":  []bson.M{{"z": map[string]any(unordered), "a": bson.A{unordered}}},
				"struct": fields{Z: 1, A: unordered, Extra: bson.M{"d": int32(4), "c": int32(3)}},
			}
			want := bson.D{
				{Key: "_id", Value: tc.wantID},
				{Key: "array", Value: bson.A{bson.D{{Key: "a", Value: bson.A{sorted}}, {Key: "z", Value: sorted}}}},
				{Key: "custom", Value: ordered}, {Key: "ordered", Value: ordered}, {Key: "raw", Value: ordered},
				{Key: "struct", Value: bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: sorted}, {Key: "c", Value: int32(3)}, {Key: "d", Value: int32(4)}}},
			}
			expected, err := bson.Marshal(want)
			require.NoError(t, err)
			for range 10 {
				normalized, id, err := normalizeDoc(doc)
				require.NoError(t, err)
				got, err := bson.Marshal(normalized)
				require.NoError(t, err)
				require.Equal(t, expected, got)
				require.Equal(t, wal.DocumentIDString(tc.id), wal.DocumentIDString(id))
			}
		})
	}
}
