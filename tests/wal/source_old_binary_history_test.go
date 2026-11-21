package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSource_OldBinaryIndexedHistory(t *testing.T) {
	empty := primitive.Binary{Subtype: 2, Data: []byte{}}
	zeros := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	for _, tc := range []struct {
		name         string
		empty, zeros any
	}{
		{"scalar", empty, zeros},
		{"compound", bson.D{{Key: "z", Value: bson.D{{Key: "part", Value: empty}}}, {Key: "a", Value: zeros}},
			bson.D{{Key: "z", Value: bson.D{{Key: "part", Value: zeros}}}, {Key: "a", Value: zeros}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db := setupTestDB(t)
			f := newMergeFixture(t, db, "old-binary-index")
			const collection = "catalog"
			emptyKey, zeroKey := wal.DocumentIDString(tc.empty), wal.DocumentIDString(tc.zeros)
			require.NotEqual(t, emptyKey, zeroKey)
			// Two older rows for the empty ID and one for the actual four-byte
			// ID share an indexed key. A later current-format put uses its own.
			var lsns []int64
			for i, id := range []any{tc.empty, tc.zeros, tc.empty, tc.empty} {
				lsn, err := f.mainWriter.Put(ctx, collection, bson.M{"_id": id, "version": int32(i)})
				require.NoError(t, err)
				if i == 0 || i == 2 {
					allowOldFormatRows(t, db)
					_, err = db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": f.main.ID, "lsn": lsn},
						bson.M{"$set": bson.M{"document_id": zeroKey}})
					require.NoError(t, err)
				}
				lsns = append(lsns, lsn)
			}
			f.refresh(t)
			history, err := f.wal.GetDocumentHistory(f.main.ID, collection, emptyKey, 0, f.main.HeadLSN)
			require.NoError(t, err)
			var seen []int64
			for _, entry := range history {
				seen = append(seen, entry.LSN)
			}
			require.Equal(t, []int64{lsns[0], lsns[2], lsns[3]}, seen)
			history, err = f.wal.GetDocumentHistory(f.main.ID, collection, zeroKey, 0, f.main.HeadLSN)
			require.NoError(t, err)
			require.Len(t, history, 1)
			require.Equal(t, lsns[1], history[0].LSN)
			keys := []string{emptyKey, zeroKey}
			point, err := f.matFull.MaterializeDocumentsAtLSN(f.main, collection, keys, lsns[2])
			require.NoError(t, err)
			full, err := f.matFull.MaterializeCollectionAtLSN(f.main, collection, lsns[2])
			require.NoError(t, err)
			require.Len(t, point, 2)
			require.Equal(t, full, point)
			latest, err := f.wal.LatestDocumentEntries(f.main.ID, collection, keys, 0, lsns[3], func(lsn int64) bool {
				return lsn != lsns[2] && lsn != lsns[3]
			})
			require.NoError(t, err)
			require.Len(t, latest, 2)
			require.Equal(t, lsns[0], latest[emptyKey].LSN)
			require.Equal(t, lsns[1], latest[zeroKey].LSN)
		})
	}
}

func TestResync_OldBinaryIndexedDeletion(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, reproSourceURI(t))
	catalog := f.source.Collection("catalog")
	empty := primitive.Binary{Subtype: 2, Data: []byte{}}
	zeros := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	_, err := catalog.InsertMany(ctx, []any{bson.M{"_id": empty}, bson.M{"_id": zeros}})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	log := f.services.Client.Database(f.metadataName).Collection("wal_log")
	res, err := log.UpdateMany(ctx, bson.M{"branch_id": main.ID, "document_id": wal.DocumentIDString(empty)},
		bson.M{"$set": bson.M{"document_id": wal.DocumentIDString(zeros)}})
	require.NoError(t, err)
	require.EqualValues(t, 1, res.ModifiedCount)
	_, err = catalog.DeleteOne(ctx, bson.M{"_id": empty})
	require.NoError(t, err)
	info := f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true})
	require.EqualValues(t, 1, info.Deletes)
	require.EqualValues(t, 1, info.Unchanged)
	state := f.mainState(t, info.ConsistentLSN)["catalog"]
	require.Len(t, state, 1)
	require.Contains(t, state, wal.DocumentIDString(zeros))
}
