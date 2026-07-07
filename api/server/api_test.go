package server

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

func TestAPI_PinFlow(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_pin_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = services.Client.Database(dbName).Drop(context.Background())
	})

	router := NewRouter(services)
	t.Cleanup(router.Shutdown)

	code, _ := do(t, router, "POST", "/api/v1/projects", map[string]string{"name": "pin-api"})
	require.Equal(t, http.StatusCreated, code)

	// Seed main through the public write surface.
	writer, err := services.WriterFor("pin-api", "main")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err := writer.Put(context.Background(), "docs", bson.M{"_id": fmt.Sprintf("d%d", i)})
		require.NoError(t, err)
	}

	// Pin the current head.
	code, resp := do(t, router, "POST", "/api/v1/projects/pin-api/pins",
		map[string]interface{}{"name": "dataset-v1", "note": "eval suite"})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	pinnedLSN := resp["lsn"].(float64)
	require.Positive(t, pinnedLSN)

	// Duplicate names conflict.
	code, _ = do(t, router, "POST", "/api/v1/projects/pin-api/pins",
		map[string]interface{}{"name": "dataset-v1"})
	require.Equal(t, http.StatusConflict, code)

	// The branch moves on; the pin does not.
	_, err = writer.Put(context.Background(), "docs", bson.M{"_id": "later"})
	require.NoError(t, err)

	code, resp = do(t, router, "GET", "/api/v1/projects/pin-api/pins", nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp["pins"], 1)

	// A sandbox forked from the pin starts at exactly the pinned state.
	code, resp = do(t, router, "POST", "/api/v1/projects/pin-api/pins/dataset-v1/sandboxes",
		map[string]interface{}{"ttl_minutes": 15})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	assert.EqualValues(t, pinnedLSN, resp["fork_lsn"])
	sandboxName := resp["branch"].(string)
	connStr := resp["connection_string"].(string)
	t.Cleanup(func() {
		branch, err := services.Branches.GetBranch(mustProjectID(t, services, "pin-api"), sandboxName)
		if err == nil && branch.PhysicalDB != "" {
			_ = services.Client.Database(branch.PhysicalDB).Drop(context.Background())
		}
	})

	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(connStr))
	require.NoError(t, err)
	defer func() { _ = client.Disconnect(context.Background()) }()
	count, err := client.Database(dbFromURI(connStr)).Collection("docs").CountDocuments(context.Background(), bson.M{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, count, "the sandbox sees the pinned state, not the later write")

	// A pinned branch refuses deletion until the pin is gone.
	code, resp = do(t, router, "DELETE", "/api/v1/projects/pin-api/pins/dataset-v1", nil)
	require.Equal(t, http.StatusOK, code, "%v", resp)
	code, _ = do(t, router, "DELETE", "/api/v1/projects/pin-api/pins/dataset-v1", nil)
	require.Equal(t, http.StatusNotFound, code)
}

func TestAPI_ConsoleReadSurface(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_console_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = services.Client.Database(dbName).Drop(context.Background())
	})

	router := NewRouter(services)
	t.Cleanup(router.Shutdown)

	// Meta and status come up before any data exists.
	code, resp := do(t, router, "GET", "/api/v1/meta", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "dev", resp["version"])
	assert.Equal(t, false, resp["read_only"])
	code, resp = do(t, router, "GET", "/api/v1/status/ingesters", nil)
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 0, resp["count"])

	// Seed history from two actors through the public write surface.
	code, _ = do(t, router, "POST", "/api/v1/projects", map[string]string{"name": "console-api"})
	require.Equal(t, http.StatusCreated, code)
	writer, err := services.WriterFor("console-api", "main")
	require.NoError(t, err)
	writer.SetActor("agent:a")
	var secondLSN int64
	for i := 0; i < 3; i++ {
		lsn, err := writer.Put(context.Background(), "notes", bson.M{"_id": fmt.Sprintf("n%d", i)})
		require.NoError(t, err)
		if i == 1 {
			secondLSN = lsn
		}
	}
	writer.SetActor("agent:b")
	_, err = writer.Put(context.Background(), "notes", bson.M{"_id": "n3"})
	require.NoError(t, err)
	_, err = writer.Put(context.Background(), "orders", bson.M{"_id": "o1"})
	require.NoError(t, err)

	// The timeline reads newest-first and pages by limit.
	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/branches/main/entries?limit=2", nil)
	require.Equal(t, http.StatusOK, code)
	entries := resp["entries"].([]interface{})
	require.Len(t, entries, 2)
	assert.Equal(t, true, resp["has_more"])
	first := entries[0].(map[string]interface{})
	second := entries[1].(map[string]interface{})
	assert.Greater(t, first["lsn"].(float64), second["lsn"].(float64))

	// Actor and collection filters.
	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/branches/main/entries?actor=agent:b", nil)
	require.Equal(t, http.StatusOK, code)
	entries = resp["entries"].([]interface{})
	require.Len(t, entries, 2)
	for _, e := range entries {
		assert.Equal(t, "agent:b", e.(map[string]interface{})["actor"])
	}
	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/branches/main/entries?collection=orders&order=asc", nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp["entries"], 1)

	// Time-travel summary at head, then a collection at an earlier LSN.
	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/branches/main/time-travel/query", nil)
	require.Equal(t, http.StatusOK, code)
	collections := resp["collections"].(map[string]interface{})
	assert.EqualValues(t, 4, collections["notes"])
	assert.EqualValues(t, 1, collections["orders"])

	path := fmt.Sprintf("/api/v1/projects/console-api/branches/main/time-travel/query?collection=notes&lsn=%d", secondLSN)
	code, resp = do(t, router, "GET", path, nil)
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 2, resp["total"])
	code, resp = do(t, router, "GET", path+"&skip=1&limit=1", nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp["documents"], 1)

	// Beyond the head is an error, stated plainly.
	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/branches/main/time-travel/query?lsn=99999", nil)
	require.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, resp["error"], "beyond")

	// Merge plans are listable and fetchable once previewed.
	code, _ = do(t, router, "POST", "/api/v1/projects/console-api/branches", map[string]string{"name": "exp", "from": "main"})
	require.Equal(t, http.StatusCreated, code)
	expWriter, err := services.WriterFor("console-api", "exp")
	require.NoError(t, err)
	_, err = expWriter.Put(context.Background(), "notes", bson.M{"_id": "exp1"})
	require.NoError(t, err)
	code, resp = do(t, router, "POST", "/api/v1/projects/console-api/branches/exp/merge-preview", nil)
	require.Equal(t, http.StatusCreated, code)
	planID := resp["id"].(string)

	code, resp = do(t, router, "GET", "/api/v1/merge-plans?project=console-api", nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp["plans"], 1)
	code, resp = do(t, router, "GET", "/api/v1/merge-plans/"+planID, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, planID, resp["id"])
	code, _ = do(t, router, "GET", "/api/v1/merge-plans", nil)
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = do(t, router, "GET", "/api/v1/merge-plans/not-a-hex-id", nil)
	require.Equal(t, http.StatusBadRequest, code)

	// Sandbox lifecycle: list shows the TTL, extend moves it, keep clears
	// it, discard removes the branch.
	code, resp = do(t, router, "POST", "/api/v1/projects/console-api/sandboxes",
		map[string]interface{}{"name": "box", "ttl_minutes": 5})
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	t.Cleanup(func() {
		branch, err := services.Branches.GetBranch(mustProjectID(t, services, "console-api"), "box")
		if err == nil && branch.PhysicalDB != "" {
			_ = services.Client.Database(branch.PhysicalDB).Drop(context.Background())
		}
	})

	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/sandboxes", nil)
	require.Equal(t, http.StatusOK, code)
	boxes := resp["sandboxes"].([]interface{})
	require.Len(t, boxes, 1)
	box := boxes[0].(map[string]interface{})
	assert.NotEmpty(t, box["connection_string"])
	assert.NotNil(t, box["branch"].(map[string]interface{})["expires_at"])

	// The supervised ingester shows up in status.
	code, resp = do(t, router, "GET", "/api/v1/status/ingesters", nil)
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 1, resp["count"])

	code, resp = do(t, router, "POST", "/api/v1/projects/console-api/sandboxes/box/extend",
		map[string]interface{}{"ttl_minutes": 60})
	require.Equal(t, http.StatusOK, code, "%v", resp)
	assert.NotEmpty(t, resp["expires_at"])

	code, _ = do(t, router, "POST", "/api/v1/projects/console-api/sandboxes/box/keep", nil)
	require.Equal(t, http.StatusOK, code)
	code, resp = do(t, router, "GET", "/api/v1/projects/console-api/sandboxes", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, resp["sandboxes"], 0, "keep converts the sandbox into an ordinary branch")

	code, _ = do(t, router, "DELETE", "/api/v1/projects/console-api/sandboxes/box", nil)
	require.Equal(t, http.StatusOK, code)
	code, _ = do(t, router, "GET", "/api/v1/projects/console-api/branches/box", nil)
	require.Equal(t, http.StatusNotFound, code)
}

func TestAPI_TokenReadOnlyAndCORS(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_guard_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = services.Client.Database(dbName).Drop(context.Background())
	})

	router := NewRouterWith(services, Options{Token: "sesame", ReadOnly: true, Version: "test"})
	t.Cleanup(router.Shutdown)

	// Meta stays open so a client can discover it needs a token.
	code, resp := do(t, router, "GET", "/api/v1/meta", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, resp["read_only"])
	assert.Equal(t, "test", resp["version"])

	// Everything else requires the bearer token.
	code, _ = do(t, router, "GET", "/api/v1/projects", nil)
	require.Equal(t, http.StatusUnauthorized, code)

	req := httptest.NewRequest("GET", "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer sesame")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Read-only rejects writes even with a valid token.
	req = httptest.NewRequest("POST", "/api/v1/projects", bytes.NewBufferString(`{"name":"nope"}`))
	req.Header.Set("Authorization", "Bearer sesame")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// CORS preflight passes without a token and echoes the origin.
	req = httptest.NewRequest("OPTIONS", "/api/v1/projects", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "http://localhost:5173", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")

	// The embedded UI serves without a token: assets are public, data is
	// not. Unknown non-API paths fall back to the SPA entry point.
	for _, path := range []string{"/", "/projects/whatever"} {
		req = httptest.NewRequest("GET", path, nil)
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html", path)
	}

	// Unknown API paths stay JSON 404s, not HTML.
	req = httptest.NewRequest("GET", "/api/v1/nope", nil)
	req.Header.Set("Authorization", "Bearer sesame")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
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
