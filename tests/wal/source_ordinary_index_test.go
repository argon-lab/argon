package wal_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
)

func TestSource_OrdinaryKeysUseCoveredScans(t *testing.T) {
	ctx := t.Context()
	f := newHistoryFixture(t, "ordinary-index")
	const collection = "catalog"
	shared := wal.DocumentIDString(primitive.Binary{Subtype: 2, Data: make([]byte, 4)})
	ids := []any{"book", int64(47), primitive.Binary{Subtype: 4, Data: make([]byte, 16)},
		bson.D{{Key: "region", Value: bson.D{{Key: "number", Value: int32(2)}}}}, shared}
	var keys []string
	var docs []bson.M
	for _, id := range ids {
		keys = append(keys, wal.DocumentIDString(id))
		docs = append(docs, bson.M{"_id": id, "version": 1})
	}
	_, err := f.writer.PutMany(ctx, collection, docs)
	require.NoError(t, err)
	for _, d := range docs {
		d["version"] = 2
	}
	newest, err := f.writer.PutMany(ctx, collection, docs)
	require.NoError(t, err)
	main := f.reload()

	var finds []bson.Raw
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" && e.Command.Lookup("find").StringValue() == "wal_log" {
			finds = append(finds, bytes.Clone(e.Command))
		}
	}}
	db := electionClient(t, metadataDeploymentURI, monitor).Database(f.db.Name())
	log, err := wal.NewService(db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, log.Close()) })
	rows, err := log.LatestDocumentEntries(main.ID, collection, keys, 0, main.HeadLSN, nil)
	require.NoError(t, err)
	require.Len(t, rows, len(keys))
	for i, key := range keys {
		require.Equal(t, 1, rows[key].DocumentKeyVersion)
		require.Equal(t, newest[i], rows[key].LSN)
	}
	require.Len(t, finds, 3) // oldest key version, indexed positions, selected images
	require.EqualValues(t, 1, finds[0].Lookup("projection", "key_v").AsInt64())
	fetched, err := finds[2].Lookup("filter", "lsn", "$in").Array().Values()
	require.NoError(t, err)
	var lsns []int64
	for _, value := range fetched {
		lsns = append(lsns, value.AsInt64())
	}
	require.ElementsMatch(t, newest, lsns)

	var scanned []string
	err = log.ScanDocumentKeys(ctx, main.ID, collection, 0, main.HeadLSN, func(k string) error {
		scanned = append(scanned, k)
		return nil
	})
	require.NoError(t, err)
	require.ElementsMatch(t, keys, scanned)
	require.Len(t, finds, 4)
	for _, command := range []bson.Raw{finds[1], finds[3]} {
		// Explain the actual query, including its projection and sort.
		find := bson.D{{Key: "find", Value: "wal_log"}, {Key: "filter", Value: command.Lookup("filter")},
			{Key: "projection", Value: command.Lookup("projection")}}
		if sort := command.Lookup("sort"); !sort.IsZero() {
			find = append(find, bson.E{Key: "sort", Value: sort})
		}
		reply, err := db.RunCommand(ctx, bson.D{{Key: "explain", Value: find}, {Key: "verbosity", Value: "executionStats"}}).Raw()
		require.NoError(t, err)
		require.EqualValues(t, 2*len(keys), reply.Lookup("executionStats", "nReturned").AsInt64())
		require.Zero(t, reply.Lookup("executionStats", "totalDocsExamined").AsInt64())
	}

	// The literal string ID also matches the old delete's index envelope
	// as a legacy lookup candidate. Only the old binary IDs are ambiguous.
	tomb := insertOldBinaryEntry(t, f.db, main, collection, shared, wal.OpDelete, nil)
	require.NoError(t, f.branches.UpdateBranchHead(main.ID, tomb))
	main = f.reload()
	literal := wal.DocumentIDString(shared)
	got, err := f.matFull.MaterializeDocumentAtLSN(main, collection, literal, tomb)
	require.NoError(t, err)
	require.Equal(t, shared, got["_id"])
	_, err = f.matFull.MaterializeDocumentAtLSN(main, collection, shared, tomb)
	var ambiguous *wal.AmbiguousOldBinaryError
	require.ErrorAs(t, err, &ambiguous)
}
