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
)

// doDemo drives one request with an optional demo cookie and returns the
// session cookie when the response sets one.
func doDemo(t *testing.T, router http.Handler, method, path, cookie string, body interface{}) (int, map[string]interface{}, string) {
	t.Helper()
	payload := bytes.NewBuffer(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewBuffer(raw)
	}
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: demoCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var decoded map[string]interface{}
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded), "body: %s", rec.Body.String())
	}
	set := ""
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == demoCookie {
			set = ck.Value
		}
	}
	return rec.Code, decoded, set
}

func TestAPI_DemoGateway(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_demo_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = services.Client.Database(dbName).Drop(context.Background())
	})

	router := NewRouterWith(services, Options{
		DemoMode: true, DemoTTL: time.Hour, DemoWriteLimit: 100, DemoMaxProjects: 5,
	})
	t.Cleanup(router.Shutdown)

	// No session, no data.
	code, _, _ := doDemo(t, router, "GET", "/api/v1/projects", "", nil)
	require.Equal(t, http.StatusUnauthorized, code)

	// Meta stays open and says demo.
	code, resp, _ := doDemo(t, router, "GET", "/api/v1/meta", "", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, resp["demo"])

	// First contact provisions a seeded project and sets the cookie.
	code, resp, session1 := doDemo(t, router, "POST", "/api/v1/demo/session", "", nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	project1 := resp["project"].(string)
	require.Regexp(t, `^demo-[0-9a-f]{12}$`, project1)
	require.Equal(t, project1, session1)

	// The same cookie returns the same project.
	code, resp, _ = doDemo(t, router, "POST", "/api/v1/demo/session", session1, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, project1, resp["project"])

	// The visitor sees exactly their project, pre-seeded and pinned.
	code, resp, _ = doDemo(t, router, "GET", "/api/v1/projects", session1, nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp["projects"], 1)
	code, resp, _ = doDemo(t, router, "GET", "/api/v1/projects/"+project1+"/branches/main/entries?limit=50", session1, nil)
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, resp["entries"])
	code, resp, _ = doDemo(t, router, "GET", "/api/v1/projects/"+project1+"/pins", session1, nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, resp["pins"], 1)

	// Creating projects directly is the gateway's job, not the visitor's.
	code, _, _ = doDemo(t, router, "POST", "/api/v1/projects", session1, map[string]string{"name": "mine"})
	require.Equal(t, http.StatusForbidden, code)

	// A second visitor gets their own world and cannot see the first.
	code, resp, session2 := doDemo(t, router, "POST", "/api/v1/demo/session", "", nil)
	require.Equal(t, http.StatusCreated, code)
	project2 := resp["project"].(string)
	require.NotEqual(t, project1, project2)
	code, _, _ = doDemo(t, router, "GET", "/api/v1/projects/"+project1+"/branches", session2, nil)
	require.Equal(t, http.StatusNotFound, code)
	code, _, _ = doDemo(t, router, "GET", "/api/v1/merge-plans?project="+project1, session2, nil)
	require.Equal(t, http.StatusNotFound, code)

	// The scripted agent session: a fresh branch, two actors, and a
	// conflict against main that the merge preview must surface.
	code, resp, _ = doDemo(t, router, "POST", "/api/v1/demo/scenario", session1, nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	runBranch := resp["branch"].(string)
	assert.Equal(t, "agent-run-1", runBranch)

	code, resp, _ = doDemo(t, router, "GET",
		"/api/v1/projects/"+project1+"/branches/"+runBranch+"/entries?actor=agent:planner", session1, nil)
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, resp["entries"])

	code, resp, _ = doDemo(t, router, "POST",
		"/api/v1/projects/"+project1+"/branches/"+runBranch+"/merge-preview", session1, nil)
	require.Equal(t, http.StatusCreated, code, "%v", resp)
	planID := resp["id"].(string)
	require.Len(t, resp["conflicts"], 1, "the human edit on main must surface as a conflict")

	// Plans are reachable by their owner and invisible across sessions.
	code, _, _ = doDemo(t, router, "GET", "/api/v1/merge-plans/"+planID, session1, nil)
	require.Equal(t, http.StatusOK, code)
	code, _, _ = doDemo(t, router, "GET", "/api/v1/merge-plans/"+planID, session2, nil)
	require.Equal(t, http.StatusNotFound, code)
}

func TestAPI_DemoWriteBudgetAndSweep(t *testing.T) {
	dbName := fmt.Sprintf("argon_api_demosweep_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = services.Client.Database(dbName).Drop(context.Background())
	})

	router := NewRouterWith(services, Options{
		DemoMode: true, DemoTTL: time.Nanosecond, DemoWriteLimit: 2, DemoMaxProjects: 5,
	})
	t.Cleanup(router.Shutdown)

	// The budget keys on ip|cookie: two session writes fit, the third
	// in the same minute is refused.
	code, resp, session := doDemo(t, router, "POST", "/api/v1/demo/session", "", nil)
	require.Equal(t, http.StatusCreated, code)
	project := resp["project"].(string)
	code, _, _ = doDemo(t, router, "POST", "/api/v1/projects/"+project+"/branches", session,
		map[string]string{"name": "b1", "from": "main"})
	require.Equal(t, http.StatusCreated, code)
	code, _, _ = doDemo(t, router, "POST", "/api/v1/projects/"+project+"/branches", session,
		map[string]string{"name": "b2", "from": "main"})
	require.Equal(t, http.StatusCreated, code)
	code, resp, _ = doDemo(t, router, "POST", "/api/v1/projects/"+project+"/branches", session,
		map[string]string{"name": "b3", "from": "main"})
	require.Equal(t, http.StatusTooManyRequests, code, "%v", resp)

	// The nanosecond TTL has long passed: one sweep reclaims everything.
	router.sweepDemo()
	_, err = services.Projects.GetProjectByName(project)
	require.Error(t, err, "swept demo project must be gone")
	code, _, _ = doDemo(t, router, "GET", "/api/v1/projects/"+project+"/branches", session, nil)
	require.Equal(t, http.StatusUnauthorized, code, "a swept session is no session")
}
