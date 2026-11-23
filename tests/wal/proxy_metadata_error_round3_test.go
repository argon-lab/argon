package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/wireproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Round 3 (6aa0f7a, wireproxy.resolve: "The metadata store did not answer.
// That is not a missing branch: fail this command"). The command is failed
// through buildErrorReply, which answers every proxy error with code 26,
// NamespaceNotFound. Drivers do not treat that code as a failure of the
// command: it means "the namespace is not there", and a drop of a namespace
// that is not there has succeeded. The Go driver's Collection.Drop returns
// nil for it (mongo/collection.go, "ignore namespace not found errors").
//
// So while the proxy cannot read its metadata, a client that drops a
// collection of a checked-out branch through the alias is told the drop
// succeeded, and the collection and its documents are still there.
//
// On master the alias is served from the cache for five seconds without a
// metadata read, so the same drop reaches the server and is carried out.
//
// The proxy's metadata connection here goes through a link that is cut; the
// proxy's upstream, and the client, are not affected.
func TestRound3_ADropTheProxyCouldNotCheckIsNotReportedAsDone(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("round3-proxy-drop-%d", time.Now().UnixNano())
	_, svc, mainID := newSandboxFixture(t, db, project)
	_, err := db.Collection("wal_projects").InsertOne(ctx, bson.M{"_id": project, "name": project, "use_wal": true})
	require.NoError(t, err)

	info, err := svc.Create(ctx, project, mainID, "run-1", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(info.PhysicalDB).Drop(context.Background()) })

	// The proxy reads its metadata over a connection that will fail.
	link := startRound2Link(t, "localhost:27017")
	metaURI := fmt.Sprintf("mongodb://%s/?directConnection=true&serverSelectionTimeoutMS=300&connectTimeoutMS=300", link.listener.Addr())
	metaClient, err := mongo.Connect(ctx, options.Client().ApplyURI(metaURI))
	require.NoError(t, err)
	t.Cleanup(func() { _ = metaClient.Disconnect(context.Background()) })
	metaDB := metaClient.Database(db.Name())
	metaWAL, err := wal.NewService(metaDB)
	require.NoError(t, err)
	proxyProcess := newReviewProcess(t, metaDB)
	projects, err := projectwal.NewProjectService(metaDB, metaWAL, proxyProcess.branches)
	require.NoError(t, err)
	addr := startProxy(t, wireproxy.New("localhost:27017", projects, proxyProcess.branches))

	alias := project + "~run-1"
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/%s?directConnection=true", addr, alias)))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()
	viaProxy := client.Database(alias)

	// The agent works in the sandbox through the alias.
	_, err = viaProxy.Collection("scratch").InsertOne(ctx, bson.M{"_id": "kept?"})
	require.NoError(t, err)
	physical := db.Client().Database(info.PhysicalDB).Collection("scratch")
	n, err := physical.CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the control: the alias reaches the sandbox")

	// The proxy loses its metadata store. The deployment the client's
	// commands are forwarded to is as it was.
	link.Cut()

	dropErr := viaProxy.Collection("scratch").Drop(ctx)
	t.Logf("Drop through the alias returned: %v", dropErr)

	left, err := physical.CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	if left == 0 {
		t.Skip("the drop was carried out; nothing to report")
	}
	assert.Error(t, dropErr,
		"the proxy did not forward the drop (scratch still holds %d document) and the client was told it succeeded: "+
			"the proxy's \"cannot check branch alias\" failure is answered as NamespaceNotFound, which a driver reads as \"already dropped\"", left)

	// What the same failure looks like to a command the driver does not
	// special-case: the text is right, the code says the namespace is gone.
	findErr := viaProxy.Collection("scratch").FindOne(ctx, bson.M{}).Err()
	t.Logf("FindOne through the alias returned: %v", findErr)
}
