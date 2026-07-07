package wal_test

import (
	"context"
	"fmt"
	"net"
	"testing"

	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/wireproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// startProxy runs a wire proxy on an ephemeral port over the fixture's
// services and returns its address.
func startProxy(t *testing.T, f *ingestFixture) string {
	t.Helper()
	projectService, err := projectwal.NewProjectService(f.metaDB, f.wal, f.branches)
	require.NoError(t, err)

	proxy := wireproxy.New("localhost:27017", projectService, f.branches)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = proxy.Serve(ctx, listener) }()
	t.Cleanup(cancel)
	return listener.Addr().String()
}

func TestWireProxy_AliasRouting(t *testing.T) {
	f := newIngestFixture(t, "proxy-e2e")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	addr := startProxy(t, f)

	// The fixture creates branches directly under the project ID
	// "proxy-e2e" with no project document; alias resolution looks projects
	// up by name, so register the mapping.
	_, err := f.metaDB.Collection("wal_projects").InsertOne(ctx, bson.M{
		"_id": "proxy-e2e", "name": "proxy-e2e", "use_wal": true,
	})
	require.NoError(t, err)

	uri := fmt.Sprintf("mongodb://%s/proxy-e2e~main?directConnection=true", addr)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()

	db := client.Database("proxy-e2e~main")

	t.Run("CRUD through the alias", func(t *testing.T) {
		_, err := db.Collection("docs").InsertOne(ctx, bson.M{"_id": "via-proxy", "v": int32(1)})
		require.NoError(t, err)

		var doc bson.M
		require.NoError(t, db.Collection("docs").FindOne(ctx, bson.M{"_id": "via-proxy"}).Decode(&doc))
		assert.EqualValues(t, 1, doc["v"])

		// The write landed in the branch's physical database.
		var physDoc bson.M
		require.NoError(t, f.physical.Collection("docs").FindOne(ctx, bson.M{"_id": "via-proxy"}).Decode(&physDoc))
		assert.EqualValues(t, 1, physDoc["v"])
	})

	t.Run("Cursor batching and aggregation through the alias", func(t *testing.T) {
		docs := make([]interface{}, 300)
		for i := range docs {
			docs[i] = bson.M{"_id": fmt.Sprintf("batch-%03d", i), "n": int32(i), "group": i % 3}
		}
		_, err := db.Collection("bulk").InsertMany(ctx, docs)
		require.NoError(t, err)

		// Small batch size forces getMore round-trips through the proxy.
		cursor, err := db.Collection("bulk").Find(ctx, bson.M{}, options.Find().SetBatchSize(20))
		require.NoError(t, err)
		count := 0
		for cursor.Next(ctx) {
			count++
		}
		require.NoError(t, cursor.Err())
		assert.Equal(t, 300, count)

		_, err = db.Collection("bulk").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "n", Value: -1}}})
		require.NoError(t, err)

		agg, err := db.Collection("bulk").Aggregate(ctx, mongo.Pipeline{
			{{Key: "$group", Value: bson.M{"_id": "$group", "total": bson.M{"$sum": "$n"}}}},
		})
		require.NoError(t, err)
		var groups []bson.M
		require.NoError(t, agg.All(ctx, &groups))
		assert.Len(t, groups, 3)
	})

	t.Run("Writes through the proxy become versioned history", func(t *testing.T) {
		f.waitForEntries(t, "docs", 1)
		branch, err := f.branches.GetBranchByID(f.branchID)
		require.NoError(t, err)
		walState, err := f.matFull.MaterializeCollection(branch, "docs")
		require.NoError(t, err)
		assert.Contains(t, walState, "via-proxy")
	})

	t.Run("Non-alias traffic passes through untouched", func(t *testing.T) {
		other := client.Database("proxy_plain_db")
		t.Cleanup(func() { _ = other.Drop(context.Background()) })
		_, err := other.Collection("x").InsertOne(ctx, bson.M{"_id": 1})
		require.NoError(t, err)
		count, err := other.Collection("x").CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, count)
	})

	t.Run("Unknown aliases fail with a clean command error", func(t *testing.T) {
		bad := client.Database("proxy-e2e~no-such-branch")
		_, err := bad.Collection("x").InsertOne(ctx, bson.M{"_id": 1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "argon proxy")
	})
}
