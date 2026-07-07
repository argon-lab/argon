package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/argon-lab/argon/internal/ingest"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// ingestFixture holds a checked-out branch with a running ingester.
type ingestFixture struct {
	*snapshotFixture
	checkout *checkout.Service
	ingest   *ingest.Service
	client   *mongo.Client
	branchID string
	physical *mongo.Database
}

func newIngestFixture(t *testing.T, project string) *ingestFixture {
	t.Helper()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	client := db.Client()
	co := checkout.NewService(client, db, f.branches, f.mat)
	ing := ingest.NewService(client, db, f.wal, f.branches)

	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = client.Database(checkout.PhysicalDBName(main.ID)).Drop(context.Background())
	})

	// Seed through the SDK so the checkout has content.
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = writer.Put(context.Background(), "users", bson.M{"_id": "seed", "n": int32(0)})
	require.NoError(t, err)

	info, err := co.Checkout(context.Background(), main.ID)
	require.NoError(t, err)

	return &ingestFixture{
		snapshotFixture: f,
		checkout:        co,
		ingest:          ing,
		client:          client,
		branchID:        main.ID,
		physical:        client.Database(info.PhysicalDB),
	}
}

// startIngester runs the ingester in the background and returns a stop
// function that drains it.
func (f *ingestFixture) startIngester(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- f.ingest.Run(ctx, f.branchID, ingest.WithReady(ready)) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("ingester exited before opening the stream: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("ingester never became ready")
	}
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("ingester did not stop")
		}
	}
}

// waitForHead polls until the branch head reaches at least target entries
// for the given collection, guarding against ingest lag in assertions.
func (f *ingestFixture) waitForEntries(t *testing.T, collection string, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		branch, err := f.branches.GetBranchByID(f.branchID)
		require.NoError(t, err)
		entries, err := f.wal.GetBranchEntries(f.branchID, collection, 0, branch.HeadLSN)
		require.NoError(t, err)
		if len(entries) >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s entries", want, collection)
}

// physicalState reads a collection from the physical database keyed the way
// the materializer keys state.
func (f *ingestFixture) physicalState(t *testing.T, collection string) map[string]bson.M {
	t.Helper()
	cursor, err := f.physical.Collection(collection).Find(context.Background(), bson.M{})
	require.NoError(t, err)
	state := make(map[string]bson.M)
	var docs []bson.M
	require.NoError(t, cursor.All(context.Background(), &docs))
	for _, doc := range docs {
		state[wal.DocumentIDString(doc["_id"])] = doc
	}
	return state
}

func TestIngest_DirectWritesReachTheWAL(t *testing.T) {
	f := newIngestFixture(t, "ingest-e2e")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	users := f.physical.Collection("users")

	// The full write mix, straight through the official driver.
	_, err := users.InsertOne(ctx, bson.M{"_id": "alice", "score": int32(10)})
	require.NoError(t, err)
	_, err = users.UpdateOne(ctx, bson.M{"_id": "alice"}, bson.M{"$inc": bson.M{"score": int32(5)}})
	require.NoError(t, err)
	_, err = users.ReplaceOne(ctx, bson.M{"_id": "seed"}, bson.M{"replaced": true})
	require.NoError(t, err)
	_, err = users.InsertOne(ctx, bson.M{"_id": "bob", "score": int32(1)})
	require.NoError(t, err)
	_, err = users.DeleteOne(ctx, bson.M{"_id": "bob"})
	require.NoError(t, err)

	// A brand-new collection created directly by the application.
	_, err = f.physical.Collection("events").InsertOne(ctx, bson.M{"_id": "e1", "kind": "direct"})
	require.NoError(t, err)

	f.waitForEntries(t, "users", 5)
	f.waitForEntries(t, "events", 1)

	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)

	t.Run("WAL state converges to the physical database", func(t *testing.T) {
		for _, coll := range []string{"users", "events"} {
			walState, err := f.matFull.MaterializeCollection(branch, coll)
			require.NoError(t, err)
			assert.Equal(t, f.physicalState(t, coll), walState,
				"collection %s: WAL materialization must equal the physical database", coll)
		}
	})

	t.Run("Entries carry images and the ingest actor", func(t *testing.T) {
		entries, err := f.wal.GetBranchEntries(f.branchID, "users", 0, branch.HeadLSN)
		require.NoError(t, err)

		var sawUpdateWithPre, sawDelete bool
		for _, e := range entries {
			if e.Actor != "ingest" {
				continue
			}
			switch {
			case e.Operation == wal.OpPut && e.DocumentID == "alice" && len(e.PreImage) > 0:
				sawUpdateWithPre = true
				var pre bson.M
				require.NoError(t, bson.Unmarshal(e.PreImage, &pre))
				assert.EqualValues(t, 10, pre["score"], "pre-image captures the replaced document")
			case e.Operation == wal.OpDelete && e.DocumentID == "bob":
				sawDelete = true
				var pre bson.M
				require.NoError(t, bson.Unmarshal(e.PreImage, &pre))
				assert.EqualValues(t, 1, pre["score"], "delete pre-image captured")
			}
		}
		assert.True(t, sawUpdateWithPre, "update must be ingested as a put with a pre-image")
		assert.True(t, sawDelete, "delete must be ingested with its pre-image")
	})

	t.Run("Branching from directly-written data works", func(t *testing.T) {
		fork, err := f.branches.CreateBranch("ingest-e2e", "fork", f.branchID)
		require.NoError(t, err)
		state, err := f.mat.MaterializeCollection(fork, "users")
		require.NoError(t, err)
		assert.EqualValues(t, 15, state["alice"]["score"], "fork sees the ingested direct writes")
	})
}

func TestIngest_ResumesAfterRestart(t *testing.T) {
	f := newIngestFixture(t, "ingest-resume")
	ctx := context.Background()

	// First run captures the first write.
	stop := f.startIngester(t)
	docs := f.physical.Collection("docs")
	_, err := docs.InsertOne(ctx, bson.M{"_id": "first"})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)
	stop()

	// Writes while no ingester is running.
	for i := 0; i < 5; i++ {
		_, err := docs.InsertOne(ctx, bson.M{"_id": fmt.Sprintf("offline-%d", i)})
		require.NoError(t, err)
	}

	// The restarted ingester resumes from the persisted token and catches up.
	stop2 := f.startIngester(t)
	defer stop2()
	f.waitForEntries(t, "docs", 6)

	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	walState, err := f.matFull.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	assert.Len(t, walState, 6, "no offline write may be lost")
	assert.Equal(t, f.physicalState(t, "docs"), walState)
}
