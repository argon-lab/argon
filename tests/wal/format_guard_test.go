package wal_test

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// allowOldFormatRows lets a test plant entries in the format of releases up
// to 2.0.1 by switching validation off while keeping the guard's validator,
// so a service created later in the test leaves the setting alone.
func allowOldFormatRows(t *testing.T, db *mongo.Database) {
	t.Helper()
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "collMod", Value: "wal_log"},
		{Key: "validator", Value: wal.FormatGuardValidator()},
		{Key: "validationLevel", Value: "off"},
	}).Err())
}

func TestFormatGuard_RefusesUntypedDataEntries(t *testing.T) {
	db := setupTestDB(t)
	svc, err := wal.NewService(db)
	require.NoError(t, err)
	ctx := context.Background()
	log := db.Collection("wal_log")

	// A current append is accepted.
	put := &wal.Entry{ProjectID: "p", BranchID: "b", Operation: wal.OpPut, Collection: "c",
		PostImage: mustRaw(t, bson.D{{Key: "_id", Value: "k"}, {Key: "v", Value: 1}})}
	_, err = svc.Append(put)
	require.NoError(t, err)

	// What 2.0.1 writes for a put and for a delete: no key_v.
	for _, op := range []string{"put", "delete", "insert", "update"} {
		_, err = log.InsertOne(ctx, bson.M{"project_id": "p", "branch_id": "b", "lsn": int64(100 + len(op)),
			"operation": op, "collection": "c", "document_id": "k", "v": 2})
		require.Error(t, err, "operation %s without key_v must be refused", op)
		var we mongo.WriteException
		require.ErrorAs(t, err, &we)
		require.Equal(t, 121, we.WriteErrors[0].Code, "DocumentValidationFailure")
	}

	// Control entries carry no key and are still accepted.
	_, err = log.InsertOne(ctx, bson.M{"project_id": "p", "branch_id": "b", "lsn": int64(200), "operation": "create_branch", "v": 2})
	require.NoError(t, err)

	// A row that already lacks key_v (history from before the upgrade) can
	// still be rewritten in place, as the schema-v1 migration does.
	allowOldFormatRows(t, db)
	_, err = log.InsertOne(ctx, bson.M{"_id": "old", "project_id": "p", "branch_id": "b", "lsn": int64(300),
		"operation": "insert", "collection": "c", "document_id": "k", "v": 1})
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "collMod", Value: "wal_log"}, {Key: "validationLevel", Value: "moderate"}}).Err())
	_, err = log.UpdateOne(ctx, bson.M{"_id": "old"}, bson.M{"$set": bson.M{"operation": "put", "v": 2}})
	require.NoError(t, err)

	// A second service on the same database keeps the guard as it is.
	_, err = wal.NewService(db)
	require.NoError(t, err)
	var spec []bson.M
	cur, err := db.ListCollections(ctx, bson.M{"name": "wal_log"})
	require.NoError(t, err)
	require.NoError(t, cur.All(ctx, &spec))
	require.Equal(t, "moderate", spec[0]["options"].(bson.M)["validationLevel"])
}

// Issue 183: an old engine appending after a current one left the indexed
// diff reading a stale parent value. The append is now refused.
func TestFormatGuard_OldEngineAfterTypedRowIsRefused(t *testing.T) {
	db := setupTestDB(t)
	svc, err := wal.NewService(db)
	require.NoError(t, err)
	_, err = svc.Append(&wal.Entry{ProjectID: "p", BranchID: "b", Operation: wal.OpPut, Collection: "c",
		PostImage: mustRaw(t, bson.D{{Key: "_id", Value: "k"}, {Key: "v", Value: 8}})})
	require.NoError(t, err)
	_, err = db.Collection("wal_log").InsertOne(context.Background(), bson.M{"project_id": "p", "branch_id": "b",
		"lsn": int64(99), "operation": "put", "collection": "c", "document_id": "k", "v": 2,
		"doc": mustRaw(t, bson.D{{Key: "_id", Value: "k"}, {Key: "v", Value: 14}})})
	require.Error(t, err)
	legacy, err := svc.HasLegacyKeyedEntries("b", "c", 0, 1<<62)
	require.NoError(t, err)
	require.False(t, legacy)
}

func mustRaw(t *testing.T, d bson.D) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(d)
	require.NoError(t, err)
	return raw
}
