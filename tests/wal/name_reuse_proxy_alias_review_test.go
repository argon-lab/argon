package wal_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wireproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Review: a sandbox name can now be used again. The wire proxy resolves
// <project>~<branch> by NAME and remembers the answer for five seconds, so
// an agent that discards "run-1", starts over under the same name and keeps
// using mongodb://proxy/<project>~run-1 must reach the NEW sandbox's
// database, not the database of the sandbox it just discarded.
func TestReview_ProxyAliasFollowsAReusedSandboxName(t *testing.T) {
	db := setupTestDB(t)
	project := fmt.Sprintf("review-proxy-reuse-%d", time.Now().UnixNano())
	f, svc, mainID := newSandboxFixture(t, db, project)
	ctx := context.Background()

	projectService, err := projectwal.NewProjectService(db, f.wal, f.branches)
	require.NoError(t, err)
	_, err = db.Collection("wal_projects").InsertOne(ctx, bson.M{"_id": project, "name": project, "use_wal": true})
	require.NoError(t, err)
	addr := startProxy(t, wireproxy.New("localhost:27017", projectService, f.branches))

	first, err := svc.Create(ctx, project, mainID, "run-1", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(first.PhysicalDB).Drop(context.Background()) })

	alias := project + "~run-1"
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/%s?directConnection=true", addr, alias)))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()
	viaProxy := client.Database(alias)

	// The agent works in the first sandbox through the alias.
	_, err = viaProxy.Collection("docs").InsertOne(ctx, bson.M{"_id": "first-run"})
	require.NoError(t, err)
	n, err := db.Client().Database(first.PhysicalDB).Collection("docs").CountDocuments(ctx, bson.M{"_id": "first-run"})
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the alias reaches the first sandbox")

	// It discards the sandbox and starts over under the same name.
	require.NoError(t, svc.Discard(ctx, first.BranchID))
	second, err := svc.Create(ctx, project, mainID, "run-1", time.Hour)
	require.NoError(t, err, "the discarded sandbox's name can be used again")
	t.Cleanup(func() { _ = db.Client().Database(second.PhysicalDB).Drop(context.Background()) })
	require.NotEqual(t, first.PhysicalDB, second.PhysicalDB)
	// Control: with REVIEW_WAIT_FOR_ALIAS_CACHE=1 the test waits out the
	// proxy's five-second alias cache and passes, which shows the stale
	// cache entry is what misroutes the alias.
	if os.Getenv("REVIEW_WAIT_FOR_ALIAS_CACHE") != "" {
		time.Sleep(6 * time.Second)
	}

	// Same alias, new sandbox: reads see its fork state and writes land in it.
	seed, err := viaProxy.Collection("docs").CountDocuments(ctx, bson.M{"_id": "seed"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, seed, "a read through the alias sees the new sandbox's fork state")

	_, err = viaProxy.Collection("docs").InsertOne(ctx, bson.M{"_id": "second-run"})
	require.NoError(t, err)
	inNew, err := db.Client().Database(second.PhysicalDB).Collection("docs").CountDocuments(ctx, bson.M{"_id": "second-run"})
	require.NoError(t, err)
	inOld, err := db.Client().Database(first.PhysicalDB).Collection("docs").CountDocuments(ctx, bson.M{"_id": "second-run"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, inNew, "the write through the alias lands in the new sandbox (%s)", second.PhysicalDB)
	assert.EqualValues(t, 0, inOld, "nothing is written to the discarded sandbox's database (%s)", first.PhysicalDB)
}
