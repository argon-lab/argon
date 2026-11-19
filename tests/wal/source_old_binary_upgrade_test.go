package wal_test

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func oldSharedIndexKey(t *testing.T, four any) string {
	t.Helper()
	// The 61ea18c index envelope contained the four-byte ID for either image.
	raw, err := bson.Marshal(bson.D{{Key: "i", Value: four}})
	require.NoError(t, err)
	return "~argon:bson:" + base64.RawURLEncoding.EncodeToString(raw)
}

// Insert the v2 WAL BSON produced by the importer and follower at 61ea18c.
// Both puts and the delete carried the four-zero key at key_v 1. Small images
// were stored as binary subtype 0 with a compression tag and a uint32 length.
func insertOldBinaryEntry(t *testing.T, db *mongo.Database, branch *wal.Branch, collection, sharedKey string, op wal.OperationType, image bson.D) int64 {
	t.Helper()
	lsn, err := wal.NewSequencer(db).Reserve(branch.ProjectID, 1)
	require.NoError(t, err)
	now := time.Now()
	row := bson.D{
		{Key: "v", Value: int32(2)}, {Key: "lsn", Value: lsn}, {Key: "timestamp", Value: now},
		{Key: "project_id", Value: branch.ProjectID}, {Key: "branch_id", Value: branch.ID},
		{Key: "operation", Value: op}, {Key: "collection", Value: collection},
		{Key: "document_id", Value: sharedKey}, {Key: "key_v", Value: int32(1)},
	}
	field, actor := "post", "importer"
	if op == wal.OpDelete {
		field, actor = "pre", "source"
	}
	if image != nil {
		raw, err := bson.Marshal(image)
		require.NoError(t, err)
		wrapped := binary.LittleEndian.AppendUint32([]byte{0}, uint32(len(raw)))
		row = append(row, bson.E{Key: field, Value: append(wrapped, raw...)})
	}
	row = append(row, bson.E{Key: "actor", Value: actor})
	if op == wal.OpDelete {
		row = append(row, bson.E{Key: "metadata", Value: bson.D{
			{Key: "capture_operation", Value: "delete"}, {Key: "capture_wall_time", Value: now},
			{Key: "source_time", Value: primitive.Timestamp{T: uint32(now.Unix()), I: 1}},
		}})
	}
	_, err = db.Collection("wal_log").InsertOne(t.Context(), row)
	require.NoError(t, err)
	return lsn
}

func oldBinaryDocument(id any, value string) bson.D {
	return bson.D{{Key: "_id", Value: id}, {Key: "value", Value: value}}
}

func checkOldBinaryRows(t *testing.T, mat *materializer.Service, branch *wal.Branch, collection string, lsn int64, keys []string, want map[string]string) {
	t.Helper()
	full, err := mat.MaterializeCollectionAtLSN(branch, collection, lsn)
	require.NoError(t, err)
	require.Len(t, full, len(want))
	for k, value := range want {
		require.Equal(t, value, full[k]["value"])
		require.Equal(t, k, wal.DocumentIDString(full[k]["_id"]))
	}
	multi, err := mat.MaterializeDocumentsAtLSN(branch, collection, keys, lsn)
	require.NoError(t, err)
	require.Equal(t, full, multi)
	for _, k := range keys {
		one, err := mat.MaterializeDocumentAtLSN(branch, collection, k, lsn)
		require.NoError(t, err)
		require.Equal(t, full[k], one)
	}
}

func TestSource_OldBinaryImageHistory(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "old-binary-images")
	empty := primitive.Binary{Subtype: 2, Data: []byte{}}
	four := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	keys := []string{wal.DocumentIDString(empty), wal.DocumentIDString(four)}
	shared := oldSharedIndexKey(t, four)
	require.Equal(t, "~argon:bson:FQAAAAVpAAgAAAACBAAAAAAAAAAA", shared)
	for i, first := range []any{empty, four} {
		collection := []string{"empty-first", "four-first"}[i]
		other := []any{four, empty}[i]
		firstImage := oldBinaryDocument(first, "first")
		firstKey, otherKey := keys[i], keys[1-i]
		only := collection + "-only"
		onlyLSN := insertOldBinaryEntry(t, db, f.main, only, shared, wal.OpPut, firstImage)
		firstLSN := insertOldBinaryEntry(t, db, f.main, collection, shared, wal.OpPut, firstImage)
		secondLSN := insertOldBinaryEntry(t, db, f.main, collection, shared, wal.OpPut, oldBinaryDocument(other, "second"))
		deleteLSN := insertOldBinaryEntry(t, db, f.main, collection, shared, wal.OpDelete, firstImage)
		require.NoError(t, f.branches.UpdateBranchHead(f.main.ID, deleteLSN))
		f.refresh(t)
		checkOldBinaryRows(t, f.matFull, f.main, only, onlyLSN, keys, map[string]string{firstKey: "first"})
		checkOldBinaryRows(t, f.matFull, f.main, collection, firstLSN, keys, map[string]string{firstKey: "first"})
		checkOldBinaryRows(t, f.matFull, f.main, collection, secondLSN, keys, map[string]string{firstKey: "first", otherKey: "second"})
		checkOldBinaryRows(t, f.matFull, f.main, collection, deleteLSN, keys, map[string]string{otherKey: "second"})
		// New deletes with no image have an exact key, even in the shared form.
		lsn, err := f.wal.Append(&wal.Entry{
			ProjectID: f.main.ProjectID, BranchID: f.main.ID, Collection: only,
			DocumentID: firstKey, Operation: wal.OpDelete,
		})
		require.NoError(t, err)
		require.NoError(t, f.branches.UpdateBranchHead(f.main.ID, lsn))
		f.refresh(t)
		stored, err := f.wal.GetEntry(f.main.ProjectID, lsn)
		require.NoError(t, err)
		require.Equal(t, 2, stored.DocumentKeyVersion)
		checkOldBinaryRows(t, f.matFull, f.main, only, lsn, keys, nil)
	}
}

func TestSource_PartiallyReclaimedOldBinaryPointRead(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "partial-old-binary-gc")
	empty := primitive.Binary{Subtype: 2, Data: []byte{}}
	four := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	shared := oldSharedIndexKey(t, four)
	const collection = "items"
	for _, id := range []any{empty, four} {
		insertOldBinaryEntry(t, db, f.main, collection, shared, wal.OpPut, oldBinaryDocument(id, "older"))
	}
	tomb := insertOldBinaryEntry(t, db, f.main, collection, shared, wal.OpDelete, nil)
	require.NoError(t, f.branches.UpdateBranchHead(f.main.ID, tomb))
	f.refresh(t)

	// GC publishes its floor before deleting the covered rows. Simulate a
	// read while the old images still exist but the ambiguous delete is gone.
	require.NoError(t, f.wal.RecordReclaimFloor(t.Context(), f.main.ID, collection, tomb))
	result, err := db.Collection("wal_log").DeleteOne(t.Context(), bson.M{"branch_id": f.main.ID, "collection": collection, "lsn": tomb})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.DeletedCount)
	for _, id := range []any{empty, four} {
		got, err := f.mat.MaterializeDocumentAtLSN(f.main, collection, wal.DocumentIDString(id), tomb)
		require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
		require.Nil(t, got)
	}

	// A subsequent image above the floor proves the requested document's state.
	lsn := insertOldBinaryEntry(t, db, f.main, collection, shared, wal.OpPut, oldBinaryDocument(empty, "newer"))
	require.NoError(t, f.branches.UpdateBranchHead(f.main.ID, lsn))
	f.refresh(t)
	got, err := f.mat.MaterializeDocumentAtLSN(f.main, collection, wal.DocumentIDString(empty), lsn)
	require.NoError(t, err)
	require.Equal(t, "newer", got["value"])
}

func TestResync_AmbiguousOldBinaryUpgrade(t *testing.T) {
	empty := primitive.Binary{Subtype: 2, Data: []byte{}}
	four := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	for _, tc := range []struct {
		name        string
		empty, four any
	}{
		{"scalar", empty, four},
		{"compound", bson.D{{Key: "zone", Value: 7}, {Key: "part", Value: empty}}, bson.D{{Key: "zone", Value: 7}, {Key: "part", Value: four}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := newFollowFixture(t, reproSourceURI(t))
			_, err := f.source.Collection("other").InsertOne(ctx, bson.M{"_id": "unaffected"})
			require.NoError(t, err)
			f.importOnline(t, walcli.OnlineImport{})
			main := f.main(t)
			svc, db := f.services, f.services.Client.Database(f.metadataName)
			keys := []string{wal.DocumentIDString(tc.empty), wal.DocumentIDString(tc.four)}
			shared := oldSharedIndexKey(t, tc.four)
			const collection = "items"
			gone, remaining := oldBinaryDocument(tc.empty, "deleted"), oldBinaryDocument(tc.four, "surviving")
			source := f.source.Collection(collection)
			_, err = source.InsertMany(ctx, []any{gone, remaining})
			require.NoError(t, err)
			insertOldBinaryEntry(t, db, main, collection, shared, wal.OpPut, gone)
			before := insertOldBinaryEntry(t, db, main, collection, shared, wal.OpPut, remaining)
			require.NoError(t, svc.Branches.UpdateBranchHead(main.ID, before))
			edits, err := svc.Branches.CreateBranch(main.ProjectID, "edits", main.ID)
			require.NoError(t, err)
			writer, err := svc.WriterFor(f.project, edits.Name)
			require.NoError(t, err)
			_, err = writer.Put(ctx, collection, bson.M{"_id": tc.four, "value": "edited"})
			require.NoError(t, err)
			res, err := source.DeleteOne(ctx, bson.M{"_id": tc.empty})
			require.NoError(t, err)
			require.EqualValues(t, 1, res.DeletedCount)
			tomb := insertOldBinaryEntry(t, db, main, collection, shared, wal.OpDelete, nil)
			require.NoError(t, svc.Branches.UpdateBranchHead(main.ID, tomb))
			main = f.main(t)
			checkOldBinaryRows(t, svc.Materializer, main, collection, before, keys, map[string]string{keys[0]: "deleted", keys[1]: "surviving"})
			inherited, err := svc.Branches.CreateBranch(main.ProjectID, "inherited", main.ID)
			require.NoError(t, err)
			bad := func(err error) {
				t.Helper()
				var ambiguous *wal.AmbiguousOldBinaryError
				require.ErrorAs(t, err, &ambiguous)
				require.Equal(t, main.ID, ambiguous.BranchID)
				require.Equal(t, collection, ambiguous.Collection)
				require.Equal(t, shared, ambiguous.Key)
				require.Equal(t, tomb, ambiguous.LSN)
				for _, phrase := range []string{"empty old-binary", "four-zero-byte", "argon source resync -p <project> --force", "argon import database"} {
					require.Contains(t, err.Error(), phrase)
				}
			}

			for _, key := range keys {
				one, err := svc.Materializer.MaterializeDocumentAtLSN(main, collection, key, tomb)
				bad(err)
				require.Nil(t, one)
				multi, err := svc.Materializer.MaterializeDocumentsAtLSN(main, collection, []string{key}, tomb)
				bad(err)
				require.Nil(t, multi)
			}
			multi, err := svc.Materializer.MaterializeDocumentsAtLSN(main, collection, keys, tomb)
			bad(err)
			require.Nil(t, multi)
			raw, err := svc.Materializer.DocumentImageEntries(main, collection, keys, 0, tomb)
			bad(err)
			require.Nil(t, raw)
			full, err := svc.Materializer.MaterializeCollection(main, collection)
			bad(err)
			require.Nil(t, full)
			_, err = svc.TimeTravel.MaterializeAtLSN(main, collection, tomb)
			bad(err)
			_, err = svc.TimeTravel.GetBranchStateAtLSN(main, tomb)
			bad(err)
			_, err = svc.Snapshots.CreateSnapshot(ctx, main.ID, tomb)
			bad(err)
			_, err = svc.Merge.Compute(edits.ID)
			bad(err)
			_, err = svc.Merge.Preview(ctx, edits.ID)
			bad(err)
			_, err = svc.Checkout.Checkout(ctx, inherited.ID)
			bad(err)
			if tc.name == "scalar" {
				cmd := exec.CommandContext(ctx, buildSourceCLI(t), "time-travel", "query", "-p", f.project, "--lsn", strconv.FormatInt(tomb, 10), "--output", "json")
				cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
				output, err := cmd.Output()
				require.Empty(t, output)
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				require.Contains(t, string(exit.Stderr), "argon source resync -p <project> --force")
			}

			other, err := svc.Materializer.MaterializeCollection(main, "other")
			require.NoError(t, err)
			require.Contains(t, other, "unaffected")
			unrelated, err := svc.Materializer.MaterializeDocumentAtLSN(main, collection, "unaffected", tomb)
			require.NoError(t, err)
			require.Nil(t, unrelated)

			var checkedPartial bool
			result := f.resync(t, svc, walcli.ResyncSourceOptions{Force: true, BatchSize: 1, Progress: func(p walcli.ResyncProgress) {
				if p.Collection == collection && p.Phase == "compare" && p.Puts > 0 {
					now := f.main(t)
					_, err := svc.Materializer.MaterializeCollection(now, collection)
					bad(err)
					checkedPartial = true
				}
			}})
			require.True(t, checkedPartial)
			main = f.main(t)
			checkOldBinaryRows(t, svc.Materializer, main, collection, result.ConsistentLSN, keys, map[string]string{keys[1]: "surviving"})
			_, err = svc.TimeTravel.MaterializeAtLSN(main, collection, result.ConsistentLSN-1)
			bad(err)
			_, err = svc.TimeTravel.MaterializeAtLSN(main, collection, tomb)
			bad(err)
			_, err = svc.Materializer.MaterializeCollection(inherited, collection)
			bad(err)
			stored, err := source.FindOne(ctx, bson.D{}).Raw()
			require.NoError(t, err)
			want, err := bson.Marshal(remaining)
			require.NoError(t, err)
			require.Equal(t, bson.Raw(want), stored)
			n, err := source.CountDocuments(ctx, bson.D{})
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			entries, err := svc.Materializer.DocumentImageEntries(main, collection, keys, 0, result.ConsistentLSN)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, 2, entries[keys[1]].DocumentKeyVersion)
			require.Equal(t, stored, entries[keys[1]].PostImage)
		})
	}
}

// An old snapshot holds one four-byte ID. Once GC has removed its prefix,
// neither the surviving row nor an empty collection can be inferred from it.
func TestResync_ReclaimedOldBinarySnapshot(t *testing.T) {
	for _, name := range []string{"recorded-floor", "unrecorded-floor"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newFollowFixture(t, reproSourceURI(t))
			const collection = "items"
			four := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
			key := wal.DocumentIDString(four)
			_, err := f.source.Collection(collection).InsertOne(ctx, oldBinaryDocument(four, "surviving"))
			require.NoError(t, err)
			f.importOnline(t, walcli.OnlineImport{})
			main := f.main(t)
			svc := f.services
			_, err = svc.Snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
			require.NoError(t, err)
			manifests := svc.Client.Database(f.metadataName).Collection("wal_snapshots")
			unset := bson.M{"exact_old_binary_keys": ""}
			if name == "unrecorded-floor" {
				unset["visibility_rules"] = ""
			} else {
				require.NoError(t, svc.WAL.RecordReclaimFloor(ctx, main.ID, collection, main.HeadLSN))
			}
			changed, err := manifests.UpdateMany(ctx, bson.M{"branch_id": main.ID, "collection": collection}, bson.M{"$unset": unset})
			require.NoError(t, err)
			require.Positive(t, changed.ModifiedCount)
			deleted, err := svc.WAL.DeleteDataEntriesUpTo(main.ID, collection, main.HeadLSN)
			require.NoError(t, err)
			require.EqualValues(t, 1, deleted)
			before := main.HeadLSN
			state, err := svc.Materializer.MaterializeCollection(main, collection)
			require.ErrorIs(t, err, wal.ErrOldBinarySnapshotReclaimed)
			require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
			require.Nil(t, state)
			one, err := svc.Materializer.MaterializeDocument(main, collection, key)
			require.ErrorIs(t, err, wal.ErrOldBinarySnapshotReclaimed)
			require.Nil(t, one)
			multi, err := svc.Materializer.MaterializeDocumentsAtLSN(main, collection, []string{key}, before)
			require.ErrorIs(t, err, wal.ErrOldBinarySnapshotReclaimed)
			require.Nil(t, multi)

			after := f.resync(t, svc, walcli.ResyncSourceOptions{Force: true})
			main = f.main(t)
			checkOldBinaryRows(t, svc.Materializer, main, collection, after.ConsistentLSN, []string{key}, map[string]string{key: "surviving"})
			require.NoError(t, svc.Materializer.ValidateReadable(main, after.ConsistentLSN))
			_, err = svc.TimeTravel.MaterializeAtLSN(main, collection, before)
			require.ErrorIs(t, err, wal.ErrOldBinarySnapshotReclaimed)
		})
	}
}
