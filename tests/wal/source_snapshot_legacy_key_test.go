package wal_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
)

func TestSource_SnapshotLegacyKeyEvidenceUsesImage(t *testing.T) {
	h := newHistoryFixture(t, "legacy-key-evidence")
	ctx := t.Context()
	id := bson.D{{Key: "part", Value: primitive.Binary{Subtype: 2, Data: []byte{1, 2, 3}}}}
	for _, tc := range []struct {
		collection string
		key        string
		id         any
		evidence   bool
	}{
		{"extended", wal.LegacyDocumentIDString(id), id, true},
		{"migrated", fmt.Sprint(id), id, true},
		{"literal", wal.LegacyDocumentIDString(id), wal.LegacyDocumentIDString(id), false},
	} {
		t.Run(tc.collection, func(t *testing.T) {
			at := insertOldBinaryEntry(t, h.db, h.main, tc.collection, tc.key, wal.OpPut, oldBinaryDocument(tc.id, "kept"))
			allowOldFormatRows(t, h.db)
			logs := h.db.Collection("wal_log")
			_, err := logs.UpdateOne(ctx, bson.M{"project_id": h.project, "lsn": at}, bson.M{"$unset": bson.M{"key_v": ""}})
			require.NoError(t, err)
			history, err := h.wal.ScanOldBinaryHistory(ctx, h.main.ID, tc.collection)
			require.NoError(t, err)
			require.Equal(t, tc.evidence, history.Evidence)
		})
	}
}

func TestSource_SnapshotScalarBinaryCandidates(t *testing.T) {
	h := newHistoryFixture(t, "scalar-key-scan")
	const collection = "items"
	for _, subtype := range []byte{0, 2, 3, 4, 6, 66, 130, 194} {
		id := primitive.Binary{Subtype: subtype, Data: make([]byte, 16)}
		insertOldBinaryEntry(t, h.db, h.main, collection, wal.DocumentIDString(id), wal.OpPut, oldBinaryDocument(id, "kept"))
	}
	var candidates []string
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName == "find" {
			docs, err := e.Reply.Lookup("cursor", "firstBatch").Array().Values()
			require.NoError(t, err)
			for _, doc := range docs {
				candidates = append(candidates, doc.Document().Lookup("document_id").StringValue())
			}
		}
	}}
	db := electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name())
	log, err := wal.NewService(db)
	require.NoError(t, err)
	history, err := log.ScanOldBinaryHistory(t.Context(), h.main.ID, collection)
	require.NoError(t, err)
	require.True(t, history.Evidence)
	require.Equal(t, []string{wal.DocumentIDString(primitive.Binary{Subtype: 2, Data: make([]byte, 16)})}, candidates)
}
