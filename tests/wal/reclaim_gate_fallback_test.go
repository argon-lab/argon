package wal_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestSource_GCBranchSizeLimit(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "gc-size")
	collection := strings.Repeat("c", 210)
	at, err := h.writer.Put(ctx, collection, bson.M{"_id": "q"})
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	branches := h.db.Collection("wal_branches")
	raw, err := branches.FindOne(ctx, bson.M{"_id": h.main.ID}).Raw()
	require.NoError(t, err)
	_, err = branches.UpdateByID(ctx, h.main.ID, bson.M{"$set": bson.M{"padding": strings.Repeat("p", (16<<20)-len(raw)-200)}})
	require.NoError(t, err)
	report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	require.EqualValues(t, 1, report.EntriesRemoved)
	floor, err := h.wal.ReclaimFloor(h.main.ID, collection)
	require.NoError(t, err)
	require.Equal(t, at, floor)
	require.Zero(t, h.reload().ReclaimGate(collection))
}

// ARGON_TEST_GC_AUTH_URI names an authenticated test replica set on which the
// fixture can create a database, role and user.
func TestSource_GCWithoutBranchUpdate(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_GC_AUTH_URI")
	if uri == "" {
		t.Skip("requires an authenticated test replica set")
	}
	ctx := t.Context()
	db := electionClient(t, uri, nil).Database("argon_gc_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() { require.NoError(t, db.Drop(context.Background())) })
	f := newSnapshotFixture(t, db)
	main, err := f.branches.CreateBranch("restricted", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = writer.Put(ctx, "items", bson.M{"_id": "a"})
	require.NoError(t, err)
	at, err := writer.Put(ctx, "items", bson.M{"_id": "b"})
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, at)
	require.NoError(t, err)
	privilege := func(collection string, actions ...string) bson.M {
		return bson.M{"resource": bson.M{"db": db.Name(), "collection": collection}, "actions": actions}
	}
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "createRole", Value: "collector"}, {Key: "roles", Value: bson.A{}}, {Key: "privileges", Value: bson.A{
		privilege("", "find", "listIndexes", "createIndex"), privilege("wal_log", "remove"), privilege("wal_reclaim_floors", "insert", "update"),
	}}}).Err())
	t.Cleanup(func() {
		require.NoError(t, db.RunCommand(context.Background(), bson.D{{Key: "dropRole", Value: "collector"}}).Err())
	})
	password := primitive.NewObjectID().Hex()
	require.NoError(t, db.RunCommand(ctx, bson.D{{Key: "createUser", Value: "collector"}, {Key: "pwd", Value: password}, {Key: "roles", Value: bson.A{"collector"}}}).Err())
	t.Cleanup(func() {
		require.NoError(t, db.RunCommand(context.Background(), bson.D{{Key: "dropUser", Value: "collector"}}).Err())
	})
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetAuth(options.Credential{AuthSource: db.Name(), Username: "collector", Password: password}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	reader := newSnapshotFixture(t, client.Database(db.Name()))
	report, err := gc.NewService(reader.wal, reader.branches, reader.snapshots).RunProject(ctx, main.ProjectID, zeroRetention)
	require.NoError(t, err)
	require.EqualValues(t, 2, report.EntriesRemoved)
	floor, err := f.wal.ReclaimFloor(main.ID, "items")
	require.NoError(t, err)
	require.Equal(t, at, floor)
	fresh, err := f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	require.Zero(t, fresh.ReclaimGate("items"))
}

func TestSource_ReclaimForgetRecordInterleaving(t *testing.T) {
	ctx := t.Context()
	h := newHistoryFixture(t, "forget-record")
	_, err := h.db.Collection("wal_reclaim_floors").InsertOne(ctx, bson.M{"_id": bson.D{{Key: "branch_id", Value: h.main.ID}, {Key: "collection", Value: "items"}}, "lsn": int64(10)})
	require.NoError(t, err)
	var armed, recorded bool
	var recordErr error
	client := electionClient(t, metadataDeploymentURI, &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if armed && e.CommandName == "update" {
			armed = false
			recorded = true
			recordErr = h.wal.RecordReclaimFloor(ctx, h.main.ID, "items", 10)
		}
	}})
	w, err := wal.NewService(client.Database(h.db.Name()))
	require.NoError(t, err)
	armed = true
	err = w.ForgetReclaimFloors(ctx, h.main.ID)
	require.NoError(t, recordErr)
	require.NoError(t, err)
	require.True(t, recorded)
	floor, err := w.ReclaimFloor(h.main.ID, "items")
	require.NoError(t, err)
	require.Zero(t, floor)
	require.Zero(t, h.reload().ReclaimGate("items"))
}

func TestSource_ReclaimCallerSession(t *testing.T) {
	h := newHistoryFixture(t, "caller-session")
	var expected bson.Raw
	var writes int
	client := electionClient(t, metadataDeploymentURI, &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if expected != nil && (e.CommandName == "update" || e.CommandName == "delete") {
			writes++
			require.True(t, bytes.Equal(expected, e.Command.Lookup("lsid").Document()), "session for %s", e.CommandName)
		}
	}})
	w, err := wal.NewService(client.Database(h.db.Name()))
	require.NoError(t, err)
	ctx := t.Context()
	sess, err := client.StartSession()
	require.NoError(t, err)
	defer sess.EndSession(ctx)
	sc := mongo.NewSessionContext(ctx, sess)
	expected = sess.ID()
	require.NoError(t, w.RecordReclaimFloor(sc, h.main.ID, "items", 3))
	require.NoError(t, sess.StartTransaction())
	require.NoError(t, w.RecordReclaimFloor(sc, h.main.ID, "items", 8))
	require.NoError(t, sess.AbortTransaction(sc))
	floor, err := w.ReclaimFloor(h.main.ID, "items")
	require.NoError(t, err)
	require.EqualValues(t, 3, floor)
	require.NoError(t, sess.StartTransaction())
	require.NoError(t, w.ForgetReclaimFloors(sc, h.main.ID))
	require.NoError(t, sess.AbortTransaction(sc))
	require.EqualValues(t, 3, h.reload().ReclaimGate("items"))
	floor, err = w.ReclaimFloor(h.main.ID, "items")
	require.NoError(t, err)
	require.EqualValues(t, 3, floor)
	require.NoError(t, w.ForgetReclaimFloors(sc, h.main.ID))
	require.Positive(t, writes)
}

func TestSource_MalformedReclaimGates(t *testing.T) {
	h := newHistoryFixture(t, "malformed-gates")
	for i, v := range []any{"bad", bson.A{}, int64(4), bson.M{"caXRlbXM": int64(5), "bad": bson.M{"nested": 2}}, bson.M{"bad": true}} {
		_, err := h.db.Collection("wal_branches").UpdateByID(t.Context(), h.main.ID, bson.M{"$set": bson.M{"reclaim_gates": v}})
		require.NoError(t, err)
		require.Empty(t, h.reload().ReclaimGates)
		byName, err := h.branches.GetBranch(h.project, "main")
		require.NoError(t, err)
		require.Empty(t, byName.ReclaimGates)
		branches, err := h.branches.ListBranchesAny(h.project)
		require.NoError(t, err)
		require.Len(t, branches, 1)
		require.Empty(t, branches[0].ReclaimGates)
		cutoff := int64(i + 2)
		require.NoError(t, h.wal.RecordReclaimFloor(t.Context(), h.main.ID, "items", cutoff))
		floor, err := h.wal.ReclaimFloor(h.main.ID, "items")
		require.NoError(t, err)
		require.Equal(t, cutoff, floor)
		require.Empty(t, h.reload().ReclaimGates)
	}
}

func TestSource_ReclaimOversizedGateKey(t *testing.T) {
	h := newHistoryFixture(t, "gc-key-size")
	ctx := t.Context()
	// The oplog for inserting a literal 13 MiB _id exceeds MongoDB's
	// document limit. Seed a collation-equivalent short _id so the update
	// still sends the full logical name and matches an existing floor.
	require.NoError(t, h.db.CreateCollection(ctx, "wal_reclaim_floors", options.CreateCollection().SetCollation(&options.Collation{Locale: "en", Alternate: "shifted", MaxVariable: "space"})))
	name := "c" + strings.Repeat(" ", 13<<20)
	id := bson.D{{Key: "branch_id", Value: h.main.ID}, {Key: "collection", Value: "c"}}
	_, err := h.db.Collection("wal_reclaim_floors").InsertOne(ctx, bson.M{"_id": id, "lsn": int64(1)})
	require.NoError(t, err)
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, name, 8))
	floor, err := h.wal.ReclaimFloor(h.main.ID, name)
	require.NoError(t, err)
	require.EqualValues(t, 8, floor)
	require.Empty(t, h.reload().ReclaimGates)
}

func TestSource_GCUnexpectedGateError(t *testing.T) {
	h := newHistoryFixture(t, "gc-error")
	ctx := t.Context()
	at := h.put("q", 1)
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	// A validator that rejects the gate field is not one of the refusals
	// named in the code. Garbage collection still proceeds without a gate.
	err = h.db.RunCommand(ctx, bson.D{{Key: "collMod", Value: "wal_branches"}, {Key: "validator", Value: bson.M{"reclaim_gates": bson.M{"$exists": false}}}}).Err()
	require.NoError(t, err)
	_, err = gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	floor, err := h.wal.ReclaimFloor(h.main.ID, "docs")
	require.NoError(t, err)
	require.EqualValues(t, at, floor)
	require.Empty(t, h.reload().ReclaimGates)
	_, err = h.wal.GetEntry(h.project, at)
	require.Error(t, err)
}
