package undo

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestRestorationImageWithRepeatedFields(t *testing.T) {
	pre, err := bson.Marshal(bson.D{
		{Key: "_id", Value: "m"}, {Key: "z", Value: "new"}, {Key: "b", Value: 10}, {Key: "z", Value: "back"},
	})
	require.NoError(t, err)
	c := Compensation{DocumentID: "m", ID: "m"}
	require.NoError(t, wal.UnmarshalDocument(pre, &c.Restore))
	ordered := bson.D{{Key: "_id", Value: "m"}, {Key: "z", Value: "back"}, {Key: "b", Value: 10}}
	sorted := bson.D{{Key: "_id", Value: "m"}, {Key: "b", Value: 10}, {Key: "z", Value: "back"}}
	for _, tt := range []struct {
		name     string
		metadata map[string]any
		want     bson.D
	}{
		{"unmarked", nil, ordered},
		{"unverified", map[string]any{"capture_images": "attached_unverified"}, sorted},
		{"current", map[string]any{"capture_images": "current"}, sorted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := &wal.Entry{PreImage: pre, DocumentKeyVersion: 1, Metadata: tt.metadata}
			raw, err := (&Service{}).restorationImage(nil, entry, c)
			require.NoError(t, err)
			want, err := bson.Marshal(tt.want)
			require.NoError(t, err)
			require.Equal(t, bson.Raw(want), raw)
		})
	}
}
