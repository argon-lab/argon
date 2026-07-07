package wal_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// newCheckoutFixture extends the snapshot fixture with a checkout service
// and a client handle for physical databases.
func newCheckoutFixture(t *testing.T) (*snapshotFixture, *checkout.Service, *mongo.Client) {
	t.Helper()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	client := db.Client()
	svc := checkout.NewService(client, db, f.branches, f.mat)
	return f, svc, client
}

func dropPhysical(t *testing.T, client *mongo.Client, branchID string) {
	t.Helper()
	t.Cleanup(func() {
		_ = client.Database(checkout.PhysicalDBName(branchID)).Drop(context.Background())
	})
}

func TestCheckout_MaterializesIntoRealMongo(t *testing.T) {
	f, svc, client := newCheckoutFixture(t)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("co-test", "main", "")
	require.NoError(t, err)
	dropPhysical(t, client, main.ID)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	for i := 0; i < 50; i++ {
		_, err := writer.Put(ctx, "users", bson.M{
			"_id":   fmt.Sprintf("u%02d", i),
			"score": int32(i),
			"team":  []string{"red", "blue"}[i%2],
		})
		require.NoError(t, err)
	}
	_, err = writer.Put(ctx, "orders", bson.M{"_id": "o1", "total": int32(99)})
	require.NoError(t, err)

	info, err := svc.Checkout(ctx, main.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, info.Collections)
	assert.EqualValues(t, 51, info.Documents)

	physical := client.Database(info.PhysicalDB)

	t.Run("State matches the WAL materialization", func(t *testing.T) {
		count, err := physical.Collection("users").CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		assert.EqualValues(t, 50, count)

		var doc bson.M
		require.NoError(t, physical.Collection("users").FindOne(ctx, bson.M{"_id": "u07"}).Decode(&doc))
		assert.EqualValues(t, 7, doc["score"])
	})

	t.Run("Real mongod capabilities work", func(t *testing.T) {
		users := physical.Collection("users")

		// Secondary index.
		_, err := users.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "score", Value: -1}}})
		require.NoError(t, err)

		// Sort + limit + skip — Find options the SDK path never supported.
		cursor, err := users.Find(ctx, bson.M{},
			options.Find().SetSort(bson.D{{Key: "score", Value: -1}}).SetLimit(3).SetSkip(1))
		require.NoError(t, err)
		var top []bson.M
		require.NoError(t, cursor.All(ctx, &top))
		require.Len(t, top, 3)
		assert.EqualValues(t, 48, top[0]["score"], "sorted descending, skipping the max")

		// Aggregation pipeline — flat-out unsupported before.
		aggCursor, err := users.Aggregate(ctx, mongo.Pipeline{
			{{Key: "$group", Value: bson.M{"_id": "$team", "total": bson.M{"$sum": "$score"}}}},
			{{Key: "$sort", Value: bson.M{"_id": 1}}},
		})
		require.NoError(t, err)
		var groups []bson.M
		require.NoError(t, aggCursor.All(ctx, &groups))
		require.Len(t, groups, 2)
		assert.Equal(t, "blue", groups[0]["_id"])
	})
}

func TestCheckout_RefreshAndRelease(t *testing.T) {
	f, svc, client := newCheckoutFixture(t)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("co-refresh", "main", "")
	require.NoError(t, err)
	dropPhysical(t, client, main.ID)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)

	_, err = writer.Put(ctx, "docs", bson.M{"_id": "a", "v": int32(1)})
	require.NoError(t, err)

	info, err := svc.Checkout(ctx, main.ID)
	require.NoError(t, err)
	physical := client.Database(info.PhysicalDB)

	// Release, write more through the SDK, check out again: the refresh
	// reflects the newer WAL state.
	require.NoError(t, svc.Release(ctx, main.ID))
	branch, _ := f.branches.GetBranchByID(main.ID)
	assert.False(t, branch.IsLive())

	writer2 := walwriter.New(f.wal, f.branches, f.mat, branch)
	_, err = writer2.Put(ctx, "docs", bson.M{"_id": "b", "v": int32(2)})
	require.NoError(t, err)

	_, err = svc.Checkout(ctx, main.ID)
	require.NoError(t, err)
	count, err := physical.Collection("docs").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.EqualValues(t, 2, count, "refresh materializes the post-release writes")
}

func TestCheckout_LiveBranchRejectsSDKWrites(t *testing.T) {
	f, svc, client := newCheckoutFixture(t)
	ctx := context.Background()

	main, err := f.branches.CreateBranch("co-guard", "main", "")
	require.NoError(t, err)
	dropPhysical(t, client, main.ID)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "a"})
	require.NoError(t, err)

	_, err = svc.Checkout(ctx, main.ID)
	require.NoError(t, err)

	// A fresh handle sees the live state and refuses every write form.
	live, err := f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	require.True(t, live.IsLive())
	liveWriter := walwriter.New(f.wal, f.branches, f.mat, live)

	_, err = liveWriter.Put(ctx, "docs", bson.M{"_id": "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checked out")

	_, err = liveWriter.PutMany(ctx, "docs", []bson.M{{"_id": "b2"}})
	require.Error(t, err)

	_, _, err = liveWriter.Delete(ctx, "docs", "a")
	require.Error(t, err)

	// Reads still work (the WAL state remains queryable).
	state, err := f.mat.MaterializeCollection(live, "docs")
	require.NoError(t, err)
	assert.Len(t, state, 1)

	// After release, SDK writes flow again.
	require.NoError(t, svc.Release(ctx, main.ID))
	released, _ := f.branches.GetBranchByID(main.ID)
	releasedWriter := walwriter.New(f.wal, f.branches, f.mat, released)
	_, err = releasedWriter.Put(ctx, "docs", bson.M{"_id": "c"})
	require.NoError(t, err)
}

func TestCheckout_ConnectionString(t *testing.T) {
	cases := []struct{ base, db, want string }{
		{"mongodb://localhost:27017", "argon_br_x", "mongodb://localhost:27017/argon_br_x"},
		{"mongodb://localhost:27017/", "argon_br_x", "mongodb://localhost:27017/argon_br_x"},
		{"mongodb://user:pw@host:27017/admin?authSource=admin", "argon_br_x", "mongodb://user:pw@host:27017/argon_br_x?authSource=admin"},
		{"mongodb+srv://cluster.example.net/?retryWrites=true", "argon_br_x", "mongodb+srv://cluster.example.net/argon_br_x?retryWrites=true"},
		{"", "argon_br_x", "mongodb://localhost:27017/argon_br_x"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, checkout.ConnectionString(c.base, c.db), "base %q", c.base)
	}
}
