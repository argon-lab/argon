package wal_test

import (
	"context"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/migrate"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// insertLegacyEntry writes a raw schema-v1 wal_log document the way the old
// code did: expression payloads in compressed_document, no "v" field. The
// LSN is reserved through the sequencer so v2 entries can interleave.
func insertLegacyEntry(t *testing.T, db *mongo.Database, walService *wal.Service, compressor *wal.Compressor,
	projectID, branchID string, op wal.OperationType, collection, documentID string, payload bson.M) int64 {
	t.Helper()

	raw, err := bson.Marshal(payload)
	require.NoError(t, err)
	compressed, err := compressor.Compress(raw)
	require.NoError(t, err)

	// Reserve an LSN by appending a throwaway v2 entry? No — reserve
	// directly through a counter bump: append via the wal_counters
	// document to stay in sequence with future writes.
	seq := wal.NewSequencer(db)
	lsn, err := seq.Reserve(projectID, 1)
	require.NoError(t, err)

	_, err = db.Collection("wal_log").InsertOne(context.Background(), bson.M{
		"lsn":                 lsn,
		"project_id":          projectID,
		"branch_id":           branchID,
		"operation":           op,
		"collection":          collection,
		"document_id":         documentID,
		"compressed_document": compressed,
	})
	require.NoError(t, err)
	return lsn
}

func TestMigrate_LegacyWAL(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	migrator, err := migrate.NewService(db, branchService, mat)
	require.NoError(t, err)
	compressor, err := wal.NewCompressor(nil)
	require.NoError(t, err)
	ctx := context.Background()

	branch, err := branchService.CreateBranch("legacy-project", "main", "")
	require.NoError(t, err)

	// Legacy history: two inserts, an update by filter, a delete by filter,
	// and an update that matches nothing (must be dropped).
	insertLegacyEntry(t, db, walService, compressor, "legacy-project", branch.ID,
		wal.LegacyOpInsert, "users", "u1", bson.M{"_id": "u1", "name": "Alice", "score": int32(10)})
	insertLegacyEntry(t, db, walService, compressor, "legacy-project", branch.ID,
		wal.LegacyOpInsert, "users", "u2", bson.M{"_id": "u2", "name": "Bob", "score": int32(20)})
	insertLegacyEntry(t, db, walService, compressor, "legacy-project", branch.ID,
		wal.LegacyOpUpdate, "users", "", bson.M{
			"filter": bson.M{"name": "Alice"},
			"update": bson.M{"$set": bson.M{"score": int32(15)}},
		})
	insertLegacyEntry(t, db, walService, compressor, "legacy-project", branch.ID,
		wal.LegacyOpDelete, "users", "", bson.M{"name": "Bob"})
	noopLSN := insertLegacyEntry(t, db, walService, compressor, "legacy-project", branch.ID,
		wal.LegacyOpUpdate, "users", "", bson.M{
			"filter": bson.M{"name": "Nobody"},
			"update": bson.M{"$set": bson.M{"x": 1}},
		})
	lastLSN := noopLSN
	require.NoError(t, branchService.UpdateBranchHead(branch.ID, lastLSN))
	branch, err = branchService.GetBranchByID(branch.ID)
	require.NoError(t, err)

	t.Run("Materializer refuses legacy entries before migration", func(t *testing.T) {
		_, err := mat.MaterializeCollection(branch, "users")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "migration")
	})

	t.Run("Migration rewrites history in place", func(t *testing.T) {
		result, err := migrator.MigrateProject(ctx, "legacy-project")
		require.NoError(t, err)
		assert.Equal(t, 4, result.EntriesRewritten, "two inserts, one update, one delete")
		assert.Equal(t, 1, result.EntriesRemoved, "the no-op update is dropped")

		state, err := mat.MaterializeCollection(branch, "users")
		require.NoError(t, err)
		require.Len(t, state, 1, "Bob was deleted")
		assert.Equal(t, "Alice", state["u1"]["name"])
		assert.Equal(t, int32(15), state["u1"]["score"], "legacy $set resolved into the post-image")

		// The rewritten entries are proper v2 physical records.
		entries, err := walService.GetBranchEntries(branch.ID, "users", 0, lastLSN)
		require.NoError(t, err)
		require.Len(t, entries, 4)
		for _, entry := range entries {
			assert.False(t, entry.IsLegacy(), "entry LSN %d still legacy", entry.LSN)
			assert.NotEmpty(t, entry.DocumentID)
			if entry.Operation == wal.OpPut {
				assert.NotEmpty(t, entry.PostImage)
			}
		}
		// The resolved update carries the pre-image of the replaced doc.
		update := entries[2]
		assert.Equal(t, wal.OpPut, update.Operation)
		var pre bson.M
		require.NoError(t, bson.Unmarshal(update.PreImage, &pre))
		assert.Equal(t, int32(10), pre["score"])
	})

	t.Run("Migration is idempotent", func(t *testing.T) {
		result, err := migrator.MigrateProject(ctx, "legacy-project")
		require.NoError(t, err)
		assert.Zero(t, result.EntriesRewritten)
		assert.Zero(t, result.EntriesRemoved)
	})
}

func TestMigrate_MixedHistoryAndBranches(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	migrator, err := migrate.NewService(db, branchService, mat)
	require.NoError(t, err)
	compressor, err := wal.NewCompressor(nil)
	require.NoError(t, err)
	ctx := context.Background()

	main, err := branchService.CreateBranch("mixed-project", "main", "")
	require.NoError(t, err)

	// Legacy prefix on main.
	insertLegacyEntry(t, db, walService, compressor, "mixed-project", main.ID,
		wal.LegacyOpInsert, "items", "i1", bson.M{"_id": "i1", "v": int32(1)})
	lsn := insertLegacyEntry(t, db, walService, compressor, "mixed-project", main.ID,
		wal.LegacyOpInsert, "items", "i2", bson.M{"_id": "i2", "v": int32(2)})
	require.NoError(t, branchService.UpdateBranchHead(main.ID, lsn))
	main, _ = branchService.GetBranchByID(main.ID)

	// Fork a child at the legacy point, then write v2 entries on both.
	child, err := branchService.CreateBranch("mixed-project", "child", main.ID)
	require.NoError(t, err)

	postImage, err := bson.Marshal(bson.M{"_id": "i3", "v": int32(3)})
	require.NoError(t, err)
	_, err = walService.Append(&wal.Entry{
		ProjectID:  "mixed-project",
		BranchID:   child.ID,
		Operation:  wal.OpPut,
		Collection: "items",
		DocumentID: "i3",
		PostImage:  postImage,
	})
	require.NoError(t, err)
	childHead := walService.GetCurrentLSN("mixed-project")
	require.NoError(t, branchService.UpdateBranchHead(child.ID, childHead))

	// Migrate: parent's legacy prefix must be rewritten and the child's
	// ancestry (which crosses the migrated segment) must materialize.
	_, err = migrator.MigrateProject(ctx, "mixed-project")
	require.NoError(t, err)

	child, _ = branchService.GetBranchByID(child.ID)
	state, err := mat.MaterializeCollection(child, "items")
	require.NoError(t, err)
	assert.Len(t, state, 3, "child sees migrated parent history plus its own v2 write")
	assert.EqualValues(t, 1, state["i1"]["v"])
	assert.EqualValues(t, 3, state["i3"]["v"])
}
