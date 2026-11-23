package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Review, completeness pass (the first reviewer's unverified "busy-loops with
// no backoff", measured): a create whose name is held by another create that
// has inserted its branch and not committed yet gets a write conflict, which
// the driver labels transient, and WithTransaction runs the callback again at
// once. Each run reserves an LSN outside the transaction and makes three round
// trips, so the wait is a spin: it lasts as long as the other transaction
// stays open (a stalled or killed process keeps one open until the server's
// transactionLifetimeLimitSeconds, 60 s by default), it takes no context, and
// it leaves one gap in the project's LSN sequence per turn.
//
// The other create is stood in for by a transaction this test holds open for
// two seconds over a live branch document of the same name.
func TestReview_CreateWaitingForAHeldNameDoesNotSpinThroughLSNs(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "review-held-name"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	reserved := func() int64 {
		var counter struct {
			Next int64 `bson:"next"`
		}
		require.NoError(t, db.Collection("wal_counters").FindOne(ctx, bson.M{"_id": project}).Decode(&counter))
		return counter.Next
	}

	session, err := db.Client().StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	require.NoError(t, session.StartTransaction())
	held := mongo.NewSessionContext(ctx, session)
	_, err = db.Collection("wal_branches").InsertOne(held, bson.M{
		"_id": primitive.NewObjectID().Hex(), "project_id": project, "name": "run-1", "is_deleted": false,
	})
	require.NoError(t, err)

	before := reserved()
	done := make(chan error, 1)
	go func() {
		_, err := f.branches.CreateBranch(project, "run-1", main.ID)
		done <- err
	}()
	const hold = 2 * time.Second
	time.Sleep(hold)
	spent := reserved() - before
	// The other create gives up; the name is free and the waiting create
	// goes through.
	require.NoError(t, session.AbortTransaction(ctx))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the waiting create did not finish after the name was released")
	}

	t.Logf("LSNs reserved by one create while it waited %s: %d", hold, spent)
	assert.Less(t, spent, int64(100),
		"a create waiting %s for a name reserved %d LSNs: one transaction attempt per LSN, with no pause between attempts", hold, spent)
}
