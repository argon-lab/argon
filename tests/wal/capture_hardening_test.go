package wal_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/ingest"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func captureContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCapture_CanceledLargeTransactionResumesAsOneGroup(t *testing.T) {
	ctx := captureContext(t)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	var txnBatches atomic.Int32
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName != "getMore" {
			return
		}
		array, ok := e.Reply.Lookup("cursor", "nextBatch").ArrayOK()
		if !ok {
			return
		}
		values, err := array.Values()
		if err != nil || len(values) == 0 {
			return
		}
		first, ok := values[0].DocumentOK()
		if !ok || first.Lookup("txnNumber").Type == 0 {
			return
		}
		if txnBatches.Add(1) == 2 {
			cancelWatch()
		}
	}}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017").SetMonitor(monitor))
	require.NoError(t, err)
	db := client.Database(fmt.Sprintf("argon_capture_transaction_%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = db.Drop(context.Background()); _ = client.Disconnect(context.Background()) })
	f := newIngestFixtureAt(t, "capture-large-transaction", db)
	before, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- f.ingest.Run(watchCtx, f.branchID, ingest.WithReady(ready)) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("capture startup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	session, err := client.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	docs := make([]interface{}, 251)
	for i := range docs {
		docs[i] = bson.M{"_id": fmt.Sprintf("txn-%03d", i), "n": i}
	}
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		_, err := f.physical.Collection("users").InsertMany(sc, docs)
		return nil, err
	})
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.GreaterOrEqual(t, txnBatches.Load(), int32(2), "must cross the 200-event server batch boundary")
	stopped, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	require.Equal(t, before.HeadLSN, stopped.HeadLSN, "cancellation must not publish a transaction prefix")
	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	after, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(f.branchID, "users", before.HeadLSN+1, after.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 251)
	for _, entry := range entries {
		require.NotEmpty(t, entry.TxnID)
		require.Equal(t, entries[0].TxnID, entry.TxnID)
	}
}

func TestCapture_StatusProgressSurvivesProcessRestart(t *testing.T) {
	f := newIngestFixture(t, "capture-status-restart")
	ctx := captureContext(t)
	require.NoError(t, f.ingest.Start(ctx, f.branchID, ingest.WithActor("agent:status")))
	_, err := f.physical.Collection("users").UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"n": 1}})
	require.NoError(t, err)
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	readStatus := func() ingest.Status {
		var stored struct {
			Status ingest.Status `bson:"capture_status"`
		}
		require.NoError(t, f.metaDB.Collection("wal_ingest_state").FindOne(ctx, bson.M{"_id": f.branchID}).Decode(&stored))
		return stored.Status
	}
	before := readStatus()
	require.Equal(t, "stopped", before.State)
	require.False(t, before.LastEventAt.IsZero())
	require.False(t, before.LastCapturedAt.IsZero())
	require.Positive(t, before.HeadLSN)
	require.GreaterOrEqual(t, before.LastEventLagMS, int64(0))
	time.Sleep(40 * time.Millisecond)
	// A brand-new Service has no in-memory status to carry metrics forward.
	restarted := ingest.NewService(f.client, f.metaDB, f.wal, f.branches)
	require.NoError(t, restarted.Start(ctx, f.branchID))
	after := readStatus()
	require.Equal(t, "running", after.State)
	require.Equal(t, "agent:status", after.Actor)
	require.Equal(t, before.LastEventAt, after.LastEventAt)
	require.Equal(t, before.LastEventLagMS, after.LastEventLagMS, "idle time must not turn the previous event sample into queue lag")
	require.Equal(t, before.HeadLSN, after.HeadLSN)
	require.True(t, after.LastCapturedAt.After(before.LastCapturedAt), "restart drains a newer barrier without erasing the event sample")
	require.Len(t, restarted.Statuses(), 1)
	require.True(t, restarted.Statuses()[0].LastEventAt.Equal(before.LastEventAt), "in-memory API health must hydrate persisted progress")
	_, err = f.physical.Collection("users").UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"n": 2}})
	require.NoError(t, err)
	require.NoError(t, restarted.Stop(ctx, f.branchID))
	final := readStatus()
	require.Equal(t, "stopped", final.State)
	require.True(t, final.LastEventAt.After(before.LastEventAt))
	require.Greater(t, final.HeadLSN, before.HeadLSN)
}

func TestCapture_CompetingWatchersDoNotDuplicateHistory(t *testing.T) {
	f := newIngestFixture(t, "capture-competing")
	ctx := captureContext(t)
	other := ingest.NewService(f.client, f.metaDB, f.wal, f.branches)
	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	require.NoError(t, other.Start(ctx, f.branchID))
	for batch := 0; batch < 6; batch++ {
		for i := 0; i < 5; i++ {
			_, err := f.physical.Collection("users").UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$inc": bson.M{"n": 1}})
			require.NoError(t, err)
		}
		// Either watcher can win the checkpoint transaction; both callers
		// must observe completion without waiting for a skipped local event.
		drained := make(chan error, 2)
		go func() { drained <- f.ingest.Drain(ctx, f.branchID) }()
		go func() { drained <- other.Drain(ctx, f.branchID) }()
		require.NoError(t, <-drained)
		require.NoError(t, <-drained)
	}
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	require.NoError(t, other.Stop(ctx, f.branchID))
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(f.branchID, "users", 0, branch.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 31, "each native event must be logged only once across competing checkpoints")
	state, err := f.matFull.MaterializeCollection(branch, "users")
	require.NoError(t, err)
	require.EqualValues(t, 30, state["seed"]["n"])
}

func TestCapture_PreImagesAreEnabledIndependentlyPerPhysicalDatabase(t *testing.T) {
	f := newIngestFixture(t, "capture-collections")
	ctx := captureContext(t)
	child, err := f.branches.CreateBranch("capture-collections", "second", f.branchID)
	require.NoError(t, err)
	info, err := f.checkout.Checkout(ctx, child.ID)
	require.NoError(t, err)
	second := f.client.Database(info.PhysicalDB)
	t.Cleanup(func() { _ = second.Drop(context.Background()) })
	for _, id := range []string{f.branchID, child.ID} {
		branch, err := f.branches.GetBranchByID(id)
		require.NoError(t, err)
		coll := f.client.Database(branch.PhysicalDB).Collection("new_collection")
		_, err = coll.InsertOne(ctx, bson.M{"_id": "a", "n": 1})
		require.NoError(t, err)
		require.NoError(t, f.ingest.Start(ctx, id))
		_, err = coll.UpdateOne(ctx, bson.M{"_id": "a"}, bson.M{"$set": bson.M{"n": 2}})
		require.NoError(t, err)
		require.NoError(t, f.ingest.Drain(ctx, id))
		branch, err = f.branches.GetBranchByID(id)
		require.NoError(t, err)
		entries, err := f.wal.GetBranchEntries(id, "new_collection", 0, branch.HeadLSN)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		require.NotEmpty(t, entries[1].PreImage, "same collection name in another physical database must also enable images")
	}
	require.NoError(t, f.ingest.Shutdown(ctx))
}

func TestCapture_CheckoutPreservesUncapturedWritesAndFirstWatchRecovers(t *testing.T) {
	f := newIngestFixture(t, "capture-first")
	ctx := captureContext(t)
	_, err := f.physical.Collection("users").UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"n": int32(42)}})
	require.NoError(t, err)
	// A repeated connection request must not drop this un-ingested write.
	_, err = f.checkout.Checkout(ctx, f.branchID)
	require.NoError(t, err)
	var physical bson.M
	require.NoError(t, f.physical.Collection("users").FindOne(ctx, bson.M{"_id": "seed"}).Decode(&physical))
	require.EqualValues(t, 42, physical["n"])
	require.NoError(t, f.ingest.Start(ctx, f.branchID, ingest.WithActor("agent:run-42")))
	t.Cleanup(func() { require.NoError(t, f.ingest.Shutdown(context.Background())) })
	require.NoError(t, f.ingest.Drain(ctx, f.branchID))
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	state, err := f.matFull.MaterializeCollection(branch, "users")
	require.NoError(t, err)
	require.EqualValues(t, 42, state["seed"]["n"])
	entries, err := f.wal.GetBranchEntries(f.branchID, "users", 0, branch.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 2, "checkout seed must not be ingested a second time")
	require.Equal(t, "agent:run-42", entries[1].Actor)
	require.Equal(t, "update", entries[1].Metadata["capture_operation"])
}

func TestCapture_ExactIntermediateImagesDrainAndAutoSnapshots(t *testing.T) {
	f := newIngestFixture(t, "capture-images")
	ctx := captureContext(t)
	f.snapshots.EnableAuto(snapshot.AutoConfig{Threshold: 5, CheckEvery: 1, Synchronous: true})
	f.ingest.SetAutoSnapshotter(f.snapshots)
	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	for i := 1; i <= 25; i++ {
		_, err := f.physical.Collection("users").UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"n": int32(i)}})
		require.NoError(t, err)
	}
	_, err := f.physical.Collection("users").DeleteOne(ctx, bson.M{"_id": "seed"})
	require.NoError(t, err)
	// Stop must include the final acknowledged delete, even without polling.
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(f.branchID, "users", 0, branch.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 27)
	for i := 1; i <= 25; i++ {
		var post, pre bson.M
		require.NoError(t, bson.Unmarshal(entries[i].PostImage, &post))
		require.NoError(t, bson.Unmarshal(entries[i].PreImage, &pre))
		require.EqualValues(t, i, post["n"], "event post-image must be exact, not a later updateLookup")
		require.EqualValues(t, i-1, pre["n"])
	}
	count, err := f.metaDB.Collection("wal_snapshots").CountDocuments(ctx, bson.M{"branch_id": f.branchID})
	require.NoError(t, err)
	require.Positive(t, count, "native writes must trigger automatic snapshots")
}

func TestCapture_DDLMarksPersistedDegradedHealth(t *testing.T) {
	f := newIngestFixture(t, "capture-ddl")
	ctx := captureContext(t)
	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	require.NoError(t, f.physical.Collection("users").Drop(ctx))
	require.Eventually(t, func() bool {
		for _, status := range f.ingest.Statuses() {
			if status.BranchID == f.branchID && status.State == "degraded" {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	var stored struct {
		Capture ingest.Status `bson:"capture_status"`
	}
	require.NoError(t, f.metaDB.Collection("wal_ingest_state").FindOne(ctx, bson.M{"_id": f.branchID}).Decode(&stored))
	require.Equal(t, "degraded", stored.Capture.State)
	require.Contains(t, stored.Capture.Error, "drop")
	// Neither drain nor release may pretend the missing DDL is durable.
	require.Error(t, f.ingest.Drain(ctx, f.branchID))
}
