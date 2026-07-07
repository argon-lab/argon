package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// do drives one JSON request through the router.
func do(t *testing.T, router http.Handler, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	payload := bytes.NewBuffer(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewBuffer(raw)
	}
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var decoded map[string]interface{}
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded), "body: %s", rec.Body.String())
	}
	return rec.Code, decoded
}

func TestAPI_ControlPlaneFlow(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = services.Client.Database(dbName).Drop(context.Background())
	})

	router := NewRouter(services)
	t.Cleanup(router.Shutdown)

	// Project lifecycle.
	code, _ := do(t, router, "POST", "/api/v1/projects", map[string]string{"name": "api-test"})
	require.Equal(t, http.StatusCreated, code)
	code, resp := do(t, router, "GET", "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, resp["projects"], 1)

	// Sandbox: fork + checkout + TTL + supervised ingester, one call.
	code, resp = do(t, router, "POST", "/api/v1/projects/api-test/sandboxes",
		map[string]interface{}{"name": "agent-1", "ttl_minutes": 30})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	connStr := resp["connection_string"].(string)
	require.NotEmpty(t, connStr)
	physicalDB := resp["connection_string"].(string)
	_ = physicalDB
	t.Cleanup(func() {
		branch, err := services.Branches.GetBranch(mustProjectID(t, services, "api-test"), "agent-1")
		if err == nil && branch.PhysicalDB != "" {
			_ = services.Client.Database(branch.PhysicalDB).Drop(context.Background())
		}
	})

	// The data plane: write through a plain driver at the returned URI.
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(connStr))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()
	agentDB := client.Database(dbFromURI(connStr))
	_, err = agentDB.Collection("notes").InsertOne(context.Background(), bson.M{"_id": "n1", "text": "from-agent"})
	require.NoError(t, err)

	// The supervised ingester captures it (poll the branch head).
	projectID := mustProjectID(t, services, "api-test")
	sbx, err := services.Branches.GetBranch(projectID, "agent-1")
	require.NoError(t, err)
	deadline := time.Now().Add(20 * time.Second)
	for {
		b, err := services.Branches.GetBranchByID(sbx.ID)
		require.NoError(t, err)
		entries, err := services.WAL.GetBranchEntries(sbx.ID, "notes", 0, b.HeadLSN)
		require.NoError(t, err)
		if len(entries) >= 1 {
			break
		}
		require.True(t, time.Now().Before(deadline), "ingester never captured the driver write")
		time.Sleep(100 * time.Millisecond)
	}

	// Diff and the data PR over REST.
	code, resp = do(t, router, "GET", "/api/v1/projects/api-test/branches/agent-1/diff", nil)
	require.Equal(t, http.StatusOK, code)
	changes := resp["changes"].([]interface{})
	require.Len(t, changes, 1)

	code, resp = do(t, router, "POST", "/api/v1/projects/api-test/branches/agent-1/merge-preview", nil)
	require.Equal(t, http.StatusCreated, code)
	planID := resp["id"].(string)

	code, resp = do(t, router, "POST", "/api/v1/merge-plans/"+planID+"/apply", map[string]string{})
	require.Equal(t, http.StatusOK, code, "%v", resp)
	assert.EqualValues(t, 1, resp["applied"])

	// The merged doc is on main.
	main, err := services.Branches.GetBranch(projectID, "main")
	require.NoError(t, err)
	state, err := services.Materializer.MaterializeCollection(main, "notes")
	require.NoError(t, err)
	assert.Contains(t, state, "n1")

	// Undo over REST (dry run).
	code, resp = do(t, router, "POST", "/api/v1/projects/api-test/branches/main/undo",
		map[string]interface{}{"from_lsn": main.HeadLSN, "dry_run": true})
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, true, resp["dry_run"])

	// Time travel info + snapshot.
	code, _ = do(t, router, "GET", "/api/v1/projects/api-test/branches/main/time-travel", nil)
	require.Equal(t, http.StatusOK, code)
	code, _ = do(t, router, "POST", "/api/v1/projects/api-test/branches/main/snapshots", nil)
	require.Equal(t, http.StatusCreated, code)

	// Delete the sandbox; errors surface as JSON.
	code, _ = do(t, router, "DELETE", "/api/v1/projects/api-test/branches/agent-1", nil)
	require.Equal(t, http.StatusOK, code)
	code, resp = do(t, router, "GET", "/api/v1/projects/api-test/branches/agent-1", nil)
	require.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, resp["error"], "not found")
}

func mustProjectID(t *testing.T, services *walcli.Services, name string) string {
	t.Helper()
	p, err := services.Projects.GetProjectByName(name)
	require.NoError(t, err)
	return p.ID
}

// dbFromURI extracts the database path segment of a mongodb URI.
func dbFromURI(uri string) string {
	// mongodb://host[:port]/db[?opts]
	rest := uri[len("mongodb://"):]
	slash := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 {
		return ""
	}
	db := rest[slash+1:]
	for i := 0; i < len(db); i++ {
		if db[i] == '?' {
			return db[:i]
		}
	}
	return db
}
