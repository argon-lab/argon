package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A merge plan that exists and was applied to this branch, read while the
// metadata read fails (here: the request's context is already done): the
// failure is not "the plan does not exist", so it must not come back as
// ErrMergePlanNotFound, which the REST undo answers as 404.
func TestRound2_MergePlanReadFailureIsNotNotFound(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_round2_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = services.Client.Database(dbName).Drop(context.Background()) })
	router := NewRouter(services)
	t.Cleanup(router.Shutdown)
	ctx := context.Background()

	code, resp := do(t, router, "POST", "/api/v1/projects", map[string]string{"name": "proj-r2"})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	code, resp = do(t, router, "POST", "/api/v1/projects/proj-r2/branches", map[string]string{"name": "feature"})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	writer, err := services.WriterFor("proj-r2", "feature")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "notes", bson.M{"_id": "n1"})
	require.NoError(t, err)
	code, resp = do(t, router, "POST", "/api/v1/projects/proj-r2/branches/feature/merge-preview", nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	planID := resp["id"].(string)
	code, resp = do(t, router, "POST", "/api/v1/merge-plans/"+planID+"/apply", map[string]string{})
	require.Equal(t, http.StatusOK, code, "%v", resp)

	main, err := services.Branches.GetBranch(mustProjectID(t, services, "proj-r2"), "main")
	require.NoError(t, err)
	done, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = services.BuildMergeUndoPlan(done, planID, main.ID)
	require.Error(t, err)
	require.False(t, errors.Is(err, walcli.ErrMergePlanNotFound),
		"a failed read of an existing plan is reported as not found: %v", err)
}
