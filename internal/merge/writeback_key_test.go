package merge

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWriteBack_ChecksEverySelectedKey(t *testing.T) {
	image := func(key any) bson.Raw {
		raw, err := wal.MarshalDocument(bson.D{{Key: "_id", Value: key}})
		require.NoError(t, err)
		return raw
	}
	old := primitive.Binary{Subtype: 2, Data: []byte{}}
	before := image(old)
	after, err := wal.MarshalDocument(bson.D{{Key: "_id", Value: old}, {Key: "stock", Value: 1}})
	require.NoError(t, err)
	other := image(primitive.Binary{Subtype: 2, Data: []byte{0, 0, 0, 0}})
	good := Change{Collection: "inventory", DocumentID: wal.DocumentIDString(old),
		storedKey: before.Lookup("_id"), BeforeRaw: before, DocumentRaw: after}
	p := &Plan{FormatVersion: orderedPlanVersion, ProvenanceVersion: provenanceVersion}
	changes := []Change{good}
	require.NoError(t, prepareWriteBack(p, changes))
	assert.Equal(t, before.Lookup("_id"), changes[0].ID)

	for _, name := range []string{"before", "document", "document_key", "delete"} {
		t.Run(name, func(t *testing.T) {
			c := good
			switch name {
			case "before":
				c.BeforeRaw = other
			case "document":
				c.DocumentRaw = other
			case "document_key":
				c.storedKey = other.Lookup("_id")
			case "delete":
				c.Delete, c.BeforeRaw, c.DocumentRaw = true, other, nil
			}
			err := prepareWriteBack(p, []Change{c})
			require.ErrorContains(t, err, "refused before claim")
			require.ErrorContains(t, err, "inventory/"+c.DocumentID)
		})
	}
}
