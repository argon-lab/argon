package wal_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
)

// Insert a v2.7.0 manifest and its compressed {k,d} frame directly, including
// the empty chunk list that its writer emitted after a shared-key deletion.
func insertReleasedSnapshot(t *testing.T, db *mongo.Database, branch *wal.Branch, collection string, lsn int64, key string, doc bson.D) primitive.ObjectID {
	t.Helper()
	ctx := t.Context()
	var size, count int64
	chunks := []string{}
	if doc != nil {
		frame, err := bson.Marshal(bson.D{{Key: "k", Value: key}, {Key: "d", Value: doc}})
		require.NoError(t, err)
		z, err := wal.NewCompressor(nil)
		require.NoError(t, err)
		body, err := z.Compress(frame)
		require.NoError(t, err)
		require.NoError(t, z.Close())
		id, err := snapshot.NewMongoChunkStore(db).Put(ctx, body)
		require.NoError(t, err)
		chunks, size, count = []string{id}, int64(len(body)), 1
	}
	result, err := db.Collection("wal_snapshots").InsertOne(ctx, bson.M{
		"project_id": branch.ProjectID, "branch_id": branch.ID, "collection": collection, "lsn": lsn,
		"ranges_applied": 0, "visibility_rules": 2, "chunk_ids": chunks, "doc_count": count,
		"size_bytes": size, "created_at": time.Now(),
	})
	require.NoError(t, err)
	return result.InsertedID.(primitive.ObjectID)
}

func TestResync_ReclaimedReleasedSnapshot(t *testing.T) {
	for _, trace := range []string{"older-import-snapshot", "later-follower-entry", "all-traces-erased"} {
		t.Run(trace, func(t *testing.T) {
			ctx := t.Context()
			f := newFollowFixture(t, reproSourceURI(t))
			_, err := f.source.Collection("anchor").InsertOne(ctx, bson.M{"_id": "stable"})
			require.NoError(t, err)
			f.importOnline(t, walcli.OnlineImport{})
			svc, db, main := f.services, f.services.Client.Database(f.metadataName), f.main(t)
			const collection = "inventory"
			empty, four := primitive.Binary{Subtype: 2, Data: []byte{}}, primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
			shared := oldSharedIndexKey(t, four)
			gone, survivor := oldBinaryDocument(empty, "deleted"), oldBinaryDocument(four, "surviving")
			source := f.source.Collection(collection)
			_, err = source.InsertMany(ctx, []any{gone, survivor})
			require.NoError(t, err)
			insertOldBinaryEntry(t, db, main, collection, shared, wal.OpPut, gone)
			lsn := insertOldBinaryEntry(t, db, main, collection, shared, wal.OpPut, survivor)
			var earlier primitive.ObjectID
			if trace == "older-import-snapshot" {
				earlier = insertReleasedSnapshot(t, db, main, collection, lsn, shared, survivor)
			}
			_, err = source.DeleteOne(ctx, bson.M{"_id": empty})
			require.NoError(t, err)
			at := insertOldBinaryEntry(t, db, main, collection, shared, wal.OpDelete, nil)
			manifest := insertReleasedSnapshot(t, db, main, collection, at, "", nil)
			// The two operations used by zero-retention GC in v2.7.0.
			require.NoError(t, svc.WAL.RecordReclaimFloor(ctx, main.ID, collection, at))
			removed, err := svc.WAL.DeleteDataEntriesUpTo(main.ID, collection, at)
			require.NoError(t, err)
			require.EqualValues(t, 3, removed)
			// An earlier upgrade could already have saved a positive, less
			// complete check for this manifest.
			if trace == "older-import-snapshot" {
				_, err = db.Collection("wal_snapshots").UpdateByID(ctx, manifest, bson.M{"$set": bson.M{"old_keys_trusted": true}})
				require.NoError(t, err)
			}
			head := at
			if trace == "later-follower-entry" {
				head = insertOldBinaryEntry(t, db, main, collection, shared, wal.OpPut, survivor)
			}
			require.NoError(t, svc.Branches.UpdateBranchHead(main.ID, head))
			main = f.main(t)
			require.Empty(t, main.PhysicalDB)
			actual, err := source.FindOne(ctx, bson.D{}).Raw()
			require.NoError(t, err)
			want, err := bson.Marshal(survivor)
			require.NoError(t, err)
			require.Equal(t, bson.Raw(want), actual)

			upgraded := newSnapshotFixture(t, db)
			catalog := db.Collection("wal_snapshots")
			stored, err := catalog.FindOne(ctx, bson.M{"_id": manifest}).Raw()
			require.NoError(t, err)
			if !earlier.IsZero() {
				old, err := catalog.FindOne(ctx, bson.M{"_id": earlier}).Raw()
				require.NoError(t, err)
				require.Equal(t, "ambiguous", old.Lookup("old_key_check").StringValue())
				require.Equal(t, "reclaimed", stored.Lookup("old_key_check").StringValue())
			}
			state, readErr := upgraded.mat.MaterializeCollectionAtLSN(main, collection, at)
			require.Equal(t, trace == "all-traces-erased", stored.Lookup("old_keys_trusted").Boolean())
			if trace == "all-traces-erased" {
				require.NoError(t, readErr)
				require.Empty(t, state)
			} else {
				require.Nil(t, state)
				requireUnknownPrefix(t, readErr, main, collection, at)
				one, err := upgraded.mat.MaterializeDocumentAtLSN(main, collection, shared, at)
				require.Nil(t, one)
				requireUnknownPrefix(t, err, main, collection, at)
			}
			if trace == "older-import-snapshot" {
				cmd := exec.CommandContext(ctx, buildSourceCLI(t), "time-travel", "query", "-p", f.project, "--collection", collection, "--lsn", strconv.FormatInt(at, 10), "--output", "json")
				cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
				out, err := cmd.Output()
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				require.NotZero(t, exit.ExitCode())
				require.Empty(t, out)
				require.Contains(t, string(exit.Stderr), "argon source resync -p <project> --force")
			}
			after := f.resync(t, svc, walcli.ResyncSourceOptions{Force: true})
			main = f.main(t)
			checkOldBinaryRows(t, svc.Materializer, main, collection, after.ConsistentLSN,
				[]string{wal.DocumentIDString(empty), shared}, map[string]string{shared: "surviving"})
			if trace == "older-import-snapshot" {
				require.NotZero(t, main.CollectionStart(collection, after.ConsistentLSN))
				stop := f.follow(t, svc, walcli.FollowSourceOptions{})
				_, err = source.UpdateByID(ctx, four, bson.M{"$set": bson.M{"value": "followed"}})
				require.NoError(t, err)
				f.waitCaughtUp(t)
				require.NoError(t, stop())
				main = f.main(t)
				require.Greater(t, main.HeadLSN, after.ConsistentLSN)
				checkOldBinaryRows(t, svc.Materializer, main, collection, main.HeadLSN,
					[]string{wal.DocumentIDString(empty), shared}, map[string]string{shared: "followed"})
			}
			state, err = svc.Materializer.MaterializeCollectionAtLSN(main, collection, at)
			if trace == "all-traces-erased" {
				require.NoError(t, err)
				require.Empty(t, state)
			} else {
				requireUnknownPrefix(t, err, main, collection, at)
				require.Nil(t, state)
			}
		})
	}
}

func requireUnknownPrefix(t *testing.T, err error, branch *wal.Branch, collection string, end int64) {
	t.Helper()
	var unknown *wal.ReclaimedOldBinaryError
	require.ErrorAs(t, err, &unknown)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	require.ErrorIs(t, err, wal.ErrOldBinarySnapshotReclaimed)
	require.Equal(t, branch.ID, unknown.BranchID)
	require.Equal(t, collection, unknown.Collection)
	require.Equal(t, branch.BaseLSN+1, unknown.From)
	require.Equal(t, end, unknown.To)
}

func TestSource_OrdinaryReclaimedReleasedSnapshot(t *testing.T) {
	h := newHistoryFixture(t, "ordinary-upgrade")
	ctx := t.Context()
	const collection = "inventory"
	// Binary values outside _id carry no evidence about the document keys.
	row := append(oldBinaryDocument("paper", "kept"), bson.E{Key: "payload", Value: primitive.Binary{Subtype: 2, Data: []byte{7}}})
	at := insertOldBinaryEntry(t, h.db, h.main, collection, "paper", wal.OpPut, row)
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	manifest := insertReleasedSnapshot(t, h.db, h.main, collection, at, "paper", row)
	catalog := h.db.Collection("wal_snapshots")
	released, err := catalog.FindOne(ctx, bson.M{"_id": manifest}).Raw()
	require.NoError(t, err)
	collected, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, gc.Config{RetentionWindow: 0})
	require.NoError(t, err)
	require.EqualValues(t, 1, collected.EntriesRemoved)
	// GC above used the installed reader. Restore the release manifest after
	// GC to open exactly the catalog an upgraded installation would see.
	_, err = catalog.ReplaceOne(ctx, bson.M{"_id": manifest}, released)
	require.NoError(t, err)
	later := insertOldBinaryEntry(t, h.db, h.main, collection, "later", wal.OpPut, oldBinaryDocument("later", "after"))
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, later))
	h.reload()
	finds := map[string]int{}
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" {
			finds[e.Command.Lookup("find").StringValue()]++
		}
	}}
	upgraded := newSnapshotFixture(t, electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name()))
	require.Equal(t, map[string]int{
		"wal_snapshots": 2, "wal_branches": 1, "wal_log": 1,
		"wal_reclaim_floors": 1, "wal_snapshot_chunks": 1,
	}, finds)
	stored, err := h.db.Collection("wal_snapshots").FindOne(ctx, bson.M{"_id": manifest}).Raw()
	require.NoError(t, err)
	require.True(t, stored.Lookup("old_keys_trusted").Boolean())
	full, err := upgraded.mat.MaterializeCollectionAtLSN(h.main, collection, at)
	require.NoError(t, err)
	require.Equal(t, []string{"paper"}, idsOf(full))
	require.Equal(t, "kept", full["paper"]["value"])
	require.Equal(t, row[2].Value, full["paper"]["payload"])
}

func TestSource_ReclaimedSnapshotFrameID(t *testing.T) {
	h := newHistoryFixture(t, "frame-id-upgrade")
	const collection = "inventory"
	id := bson.D{{Key: "owner", Value: "shop"}, {Key: "bin", Value: primitive.Binary{Subtype: 2, Data: []byte{7}}}}
	row := oldBinaryDocument(id, "recorded")
	at := insertOldBinaryEntry(t, h.db, h.main, collection, wal.DocumentIDString(id), wal.OpPut, row)
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	// A legacy string key alone does not establish the BSON type. The
	// frame's document contains the only surviving typed _id.
	manifest := insertReleasedSnapshot(t, h.db, h.main, collection, at, wal.LegacyDocumentIDString(id), row)
	require.NoError(t, h.wal.RecordReclaimFloor(t.Context(), h.main.ID, collection, at))
	removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, collection, at)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	h.reload()
	upgraded := newSnapshotFixture(t, h.db)
	m, err := h.db.Collection("wal_snapshots").FindOne(t.Context(), bson.M{"_id": manifest}).Raw()
	require.NoError(t, err)
	require.Equal(t, "reclaimed", m.Lookup("old_key_check").StringValue())
	_, err = upgraded.mat.MaterializeCollection(h.main, collection)
	requireUnknownPrefix(t, err, h.main, collection, at)
}

func TestSource_ReclaimedSnapshotLiteralEnvelope(t *testing.T) {
	h := newHistoryFixture(t, "literal-envelope")
	key := wal.DocumentIDString(primitive.Binary{Subtype: 2, Data: []byte{7}})
	const collection = "inventory"
	// Before strings beginning with the envelope prefix were escaped, the
	// literal ID was the frame key. Its BSON image establishes its type.
	row := oldBinaryDocument(key, "ordinary string")
	at := insertOldBinaryEntry(t, h.db, h.main, collection, key, wal.OpPut, row)
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	insertReleasedSnapshot(t, h.db, h.main, collection, at, key, row)
	require.NoError(t, h.wal.RecordReclaimFloor(t.Context(), h.main.ID, collection, at))
	removed, err := h.wal.DeleteDataEntriesUpTo(h.main.ID, collection, at)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	upgraded := newSnapshotFixture(t, h.db)
	state, err := upgraded.mat.MaterializeCollectionAtLSN(h.reload(), collection, at)
	require.NoError(t, err)
	require.Equal(t, key, state[wal.DocumentIDString(key)]["_id"])
}
