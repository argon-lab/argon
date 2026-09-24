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

func TestAPI_CreateBranchDrainsNativeParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	dbName := fmt.Sprintf("argon_api_fork_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	router := NewRouterWith(services, Options{})
	t.Cleanup(func() {
		router.Shutdown()
		projects, _ := services.Projects.ListProjects()
		for _, project := range projects {
			branches, _ := services.Branches.ListBranches(project.ID)
			for _, branch := range branches {
				if branch.PhysicalDB != "" {
					_ = services.Client.Database(branch.PhysicalDB).Drop(context.Background())
				}
			}
		}
		_ = services.Client.Database(dbName).Drop(context.Background())
		_ = services.Client.Disconnect(context.Background())
	})
	code, _ := do(t, router, "POST", "/api/v1/projects", map[string]string{"name": "fork"})
	require.Equal(t, http.StatusCreated, code)
	code, _ = do(t, router, "POST", "/api/v1/projects/fork/branches/main/checkout", nil)
	require.Equal(t, http.StatusOK, code)
	projectID := mustProjectID(t, services, "fork")
	parent, err := services.Branches.GetBranch(projectID, "main")
	require.NoError(t, err)

	// Stop capture to make the otherwise timing-dependent gap deterministic.
	// The REST fork must restart/drain it, including a completed native write.
	require.NoError(t, services.Ingest.Stop(ctx, parent.ID))
	physical := services.Client.Database(parent.PhysicalDB)
	_, err = physical.Collection("notes").InsertOne(ctx, bson.M{"_id": "pending", "value": "must inherit"})
	require.NoError(t, err)
	before, err := services.Branches.GetBranchByID(parent.ID)
	require.NoError(t, err)
	code, response := do(t, router, "POST", "/api/v1/projects/fork/branches", map[string]string{"name": "child"})
	require.Equal(t, http.StatusCreated, code, "%v", response)
	child, err := services.Branches.GetBranch(projectID, "child")
	require.NoError(t, err)
	require.Equal(t, parent.ID, child.ParentID, "omitted from defaults to main")
	require.Greater(t, child.BaseLSN, before.HeadLSN)
	code, response = do(t, router, "POST", "/api/v1/projects/fork/branches/child/checkout", nil)
	require.Equal(t, http.StatusOK, code, "%v", response)
	child, err = services.Branches.GetBranchByID(child.ID)
	require.NoError(t, err)
	var inherited bson.M
	require.NoError(t, services.Client.Database(child.PhysicalDB).Collection("notes").FindOne(ctx, bson.M{"_id": "pending"}).Decode(&inherited))
	require.Equal(t, "must inherit", inherited["value"])

	// A DDL gap cannot be made durable by draining. It must refuse the fork,
	// rather than creating an apparently valid branch at an incomplete head.
	require.NoError(t, physical.Collection("notes").Drop(ctx))
	require.Eventually(t, func() bool {
		for _, status := range services.Ingest.Statuses() {
			if status.BranchID == parent.ID && status.State == "degraded" {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	code, response = do(t, router, "POST", "/api/v1/projects/fork/branches", map[string]string{"name": "unsafe", "from": "main"})
	require.Equal(t, http.StatusServiceUnavailable, code, "%v", response)
	_, err = services.Branches.GetBranch(projectID, "unsafe")
	require.Error(t, err)
}
