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

// merge_plan_id names a plan of another project, or a branch the plan was not
// applied to: the undo is refused and nothing changes.
func TestReview_UndoMergePlanOnTheWrongBranchOrProject(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_review_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = services.Client.Database(dbName).Drop(context.Background()) })
	router := NewRouter(services)
	t.Cleanup(router.Shutdown)
	ctx := context.Background()

	for _, p := range []string{"proj-a", "proj-b"} {
		code, resp := do(t, router, "POST", "/api/v1/projects", map[string]string{"name": p})
		require.Equal(t, http.StatusCreated, code, "%v", resp)
	}
	code, resp := do(t, router, "POST", "/api/v1/projects/proj-a/branches", map[string]string{"name": "feature"})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	writer, err := services.WriterFor("proj-a", "feature")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "notes", bson.M{"_id": "n1"})
	require.NoError(t, err)
	code, resp = do(t, router, "POST", "/api/v1/projects/proj-a/branches/feature/merge-preview", nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	planID := resp["id"].(string)
	code, resp = do(t, router, "POST", "/api/v1/merge-plans/"+planID+"/apply", map[string]string{})
	require.Equal(t, http.StatusOK, code, "%v", resp)

	headOf := func(project, branch string) int64 {
		b, err := services.Branches.GetBranch(mustProjectID(t, services, project), branch)
		require.NoError(t, err)
		return b.HeadLSN
	}
	mainA, mainB := headOf("proj-a", "main"), headOf("proj-b", "main")
	for _, path := range []string{
		"/api/v1/projects/proj-b/branches/main/undo",
		"/api/v1/projects/proj-a/branches/feature/undo",
	} {
		code, resp = do(t, router, "POST", path, map[string]interface{}{"merge_plan_id": planID})
		// Answered like a plan that does not exist, so another branch's or
		// project's plan is not revealed.
		require.Equal(t, http.StatusNotFound, code, "%s: %v", path, resp)
		require.Contains(t, resp["error"], "not found")
	}
	code, resp = do(t, router, "POST", "/api/v1/projects/proj-a/branches/main/undo", map[string]interface{}{"merge_plan_id": "nope"})
	require.Equal(t, http.StatusNotFound, code, "%v", resp)
	code, resp = do(t, router, "POST", "/api/v1/projects/proj-a/branches/main/undo", map[string]interface{}{"merge_plan_id": planID, "actor": "x"})
	require.Equal(t, http.StatusBadRequest, code, "%v", resp)
	require.Equal(t, mainA, headOf("proj-a", "main"))
	require.Equal(t, mainB, headOf("proj-b", "main"))

	code, resp = do(t, router, "POST", "/api/v1/projects/proj-a/branches/main/undo", map[string]interface{}{"merge_plan_id": planID})
	require.Equal(t, http.StatusOK, code, "%v", resp)
	require.EqualValues(t, 1, resp["deleted"])
}
