package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// The REST control plane cannot write to the database a project follows: the
// apply endpoint is refused for such a plan, whatever the body asks for, and
// both the source and the followed branch are left as they were.
func TestAPI_MergeApplyNeverReachesAFollowedSource(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	const deployment = "mongodb://localhost:27017"
	id := time.Now().UnixNano()
	dbName := fmt.Sprintf("argon_api_follow_test_%d", id)
	services, err := walcli.NewServicesAt(deployment, dbName)
	require.NoError(t, err)
	ctx := context.Background()
	source := services.Client.Database(fmt.Sprintf("api_follow_src_%d", id))
	t.Cleanup(func() {
		_ = source.Drop(context.Background()) // the test's own database
		_ = services.Client.Database(dbName).Drop(context.Background())
	})
	_, err = source.Collection("catalog").InsertMany(ctx, []interface{}{
		bson.M{"_id": "a", "stock": int32(1)}, bson.M{"_id": "b", "stock": int32(2)},
	})
	require.NoError(t, err)
	_, err = services.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: deployment, Database: source.Name(), Project: "followed"})
	require.NoError(t, err)
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")

	router := NewRouter(services)
	t.Cleanup(router.Shutdown)
	code, resp := do(t, router, "POST", "/api/v1/projects/followed/sandboxes", map[string]interface{}{"name": "agent-1", "ttl_minutes": 30})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	sandbox, err := services.Branches.GetBranch(mustProjectID(t, services, "followed"), "agent-1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = services.Client.Database(sandbox.PhysicalDB).Drop(context.Background()) })
	_, err = services.Client.Database(sandbox.PhysicalDB).Collection("catalog").UpdateOne(ctx, bson.M{"_id": "a"}, bson.M{"$set": bson.M{"stock": int32(100)}})
	require.NoError(t, err)

	code, resp = do(t, router, "POST", "/api/v1/projects/followed/branches/agent-1/merge-preview", nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	planID := resp["id"].(string)
	require.Len(t, resp["changes"], 1)

	main, err := services.Branches.GetBranch(mustProjectID(t, services, "followed"), "main")
	require.NoError(t, err)
	code, resp = do(t, router, "POST", "/api/v1/merge-plans/"+planID+"/apply",
		map[string]interface{}{"strategy": "", "write_back": true, "uri": deployment})
	require.Equal(t, http.StatusConflict, code, "%v", resp)
	require.Contains(t, resp["error"], "follows the external database")

	var doc bson.M
	require.NoError(t, source.Collection("catalog").FindOne(ctx, bson.M{"_id": "a"}).Decode(&doc))
	require.EqualValues(t, 1, doc["stock"], "the source is untouched")
	after, err := services.Branches.GetBranch(mustProjectID(t, services, "followed"), "main")
	require.NoError(t, err)
	require.Equal(t, main.HeadLSN, after.HeadLSN, "main is untouched")
	code, resp = do(t, router, "GET", "/api/v1/merge-plans/"+planID, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "pending", resp["status"])
}
