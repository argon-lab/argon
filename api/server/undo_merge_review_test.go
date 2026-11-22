package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// In the demo every merge plan reached by ID must be the visitor's own: a
// foreign plan answers exactly like a plan that does not exist. The undo
// endpoint's merge_plan_id is a plan reached by ID too.
func TestReview_DemoUndoByForeignMergePlanIsNotFound(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_undo_merge_review_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = services.Client.Database(dbName).Drop(context.Background()) })
	router := NewRouterWith(services, Options{DemoMode: true, DemoTTL: time.Hour, DemoWriteLimit: 100, DemoMaxProjects: 5})
	t.Cleanup(router.Shutdown)

	code, resp, session1 := doDemo(t, router, "POST", "/api/v1/demo/session", "", nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	project1 := resp["project"].(string)
	code, resp, _ = doDemo(t, router, "POST", "/api/v1/demo/scenario", session1, nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	runBranch := resp["branch"].(string)
	code, resp, _ = doDemo(t, router, "POST", "/api/v1/projects/"+project1+"/branches/"+runBranch+"/merge-preview", session1, nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	planID := resp["id"].(string)

	code, resp, session2 := doDemo(t, router, "POST", "/api/v1/demo/session", "", nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	project2 := resp["project"].(string)

	// The plan route hides the foreign plan.
	code, _, _ = doDemo(t, router, "GET", "/api/v1/merge-plans/"+planID, session2, nil)
	require.Equal(t, http.StatusNotFound, code)

	undo := "/api/v1/projects/" + project2 + "/branches/main/undo"
	missingCode, missing, _ := doDemo(t, router, "POST", undo, session2,
		map[string]interface{}{"merge_plan_id": primitive.NewObjectID().Hex(), "dry_run": true})
	foreignCode, foreign, _ := doDemo(t, router, "POST", undo, session2,
		map[string]interface{}{"merge_plan_id": planID, "dry_run": true})
	t.Logf("missing plan: %d %v", missingCode, missing)
	t.Logf("foreign plan: %d %v", foreignCode, foreign)
	assert.Equal(t, http.StatusNotFound, foreignCode, "a foreign plan must be not found")
	assert.NotContains(t, fmt.Sprint(foreign["error"]), "pending", "the foreign plan's status must not be revealed")
}
