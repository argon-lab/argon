package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFunnelAcceptsOnlyAnonymousVocabulary(t *testing.T) {
	router := gin.New()
	router.POST("/events", (&Router{}).recordEvent)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"event":"first_diff"}`, 204},
		{`{"event":"first_diff","project":"private"}`, 400},
		{`{"event":"not-allowed"}`, 400},
		{`{"event":"first_diff"} {}`, 400},
		{strings.Repeat("x", 257), 400},
	} {
		req := httptest.NewRequest("POST", "/events", strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, tc.code, rec.Code)
	}
}

func TestListenAddressRequiresExplicitPublicAccess(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		require.NoError(t, ValidateListenAddress(addr, Options{}))
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.2:8080"} {
		require.Error(t, ValidateListenAddress(addr, Options{}))
		require.NoError(t, ValidateListenAddress(addr, Options{Token: "test"}))
		require.NoError(t, ValidateListenAddress(addr, Options{DemoMode: true}))
	}
}

func TestOriginGuardRejectsCrossOriginWrites(t *testing.T) {
	for _, tc := range []struct {
		origin, allow string
		want          int
	}{
		{"", "", 200}, {"http://localhost:8080", "", 200},
		{"https://untrusted.example", "", 403}, {"null", "", 403},
		{"https://console.example", "https://console.example", 200},
	} {
		r := gin.New()
		r.Use(corsMiddleware(tc.allow))
		r.POST("/api/v1/projects", func(c *gin.Context) { c.Status(200) })
		req := httptest.NewRequest("POST", "http://localhost:8080/api/v1/projects", nil)
		req.Header.Set("Origin", tc.origin)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, tc.want, rec.Code, tc.origin)
	}
}

func TestDemoNeverReturnsNativeServiceCredentials(t *testing.T) {
	name := fmt.Sprintf("argon_api_security_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", name)
	require.NoError(t, err)
	defer func() { _ = services.Client.Database(name).Drop(context.Background()) }()
	services.MongoURI = "mongodb://service:secret@localhost:27017/?authSource=admin"
	r := NewRouterWith(services, Options{DemoMode: true, DemoWriteLimit: 100})
	defer r.Shutdown()
	code, body, cookie := doDemo(t, r, "POST", "/api/v1/demo/session", "", nil)
	require.Equal(t, http.StatusCreated, code)
	p := body["project"].(string)
	for _, path := range []string{
		"/api/v1/projects/" + p + "/branches/main/checkout",
		"/api/v1/projects/" + p + "/sandboxes",
		"/api/v1/projects/" + p + "/pins/baseline/sandboxes",
	} {
		code, body, _ = doDemo(t, r, "POST", path, cookie, nil)
		require.Equal(t, http.StatusForbidden, code)
		require.NotContains(t, fmt.Sprint(body), "service:secret")
	}
	code, body, _ = doDemo(t, r, "POST", "/api/v1/projects/"+p+"/sandboxes", cookie, map[string]bool{"credential": true})
	require.Equal(t, http.StatusForbidden, code)
	require.NotContains(t, body, "connection_string")
	project, err := services.Projects.GetProjectByName(p)
	require.NoError(t, err)
	main, err := services.Branches.GetBranch(project.ID, "main")
	require.NoError(t, err)
	box, err := services.Sandbox.Create(context.Background(), project.ID, main.ID, "internal-box", time.Hour)
	require.NoError(t, err)
	defer func() { _ = services.Sandbox.Discard(context.Background(), box.BranchID) }()
	for _, path := range []string{"/api/v1/projects/" + p + "/branches/internal-box", "/api/v1/projects/" + p + "/sandboxes"} {
		code, body, _ = doDemo(t, r, "GET", path, cookie, nil)
		require.Equal(t, http.StatusOK, code)
		require.NotContains(t, fmt.Sprint(body), "connection_string")
		require.NotContains(t, fmt.Sprint(body), "service:secret")
	}
}

func TestSandboxCredentialAPI(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_AUTH_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_AUTH_MONGODB_URI to test sandbox credentials")
	}
	name := fmt.Sprintf("argon_api_credential_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt(uri, name)
	require.NoError(t, err)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = services.Client.Database(name).Drop(ctx)
		_ = services.Client.Disconnect(ctx)
	})
	r := NewRouterWith(services, Options{})
	t.Cleanup(r.Shutdown)
	project, err := services.Projects.CreateProject("scoped")
	require.NoError(t, err)
	const path = "/api/v1/projects/scoped"
	for _, invalid := range []any{"true", nil} {
		status, _ := do(t, r, "POST", path+"/sandboxes", map[string]any{"name": "box", "credential": invalid})
		require.Equal(t, http.StatusBadRequest, status)
	}
	status, body := do(t, r, "POST", path+"/sandboxes", map[string]any{"name": "box", "credential": true})
	require.Equal(t, http.StatusCreated, status, "%v", body["error"])
	b, err := services.Branches.GetBranch(project.ID, "box")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, services.Sandbox.Discard(ctx, b.ID)) })
	raw, ok := body["connection_string"].(string)
	require.True(t, ok)
	u, err := url.Parse(raw)
	require.True(t, err == nil, "invalid connection string")
	require.NotEmpty(t, b.SandboxUsername)
	require.Equal(t, b.SandboxUsername, u.User.Username())
	require.Equal(t, b.PhysicalDB, u.Query().Get("authSource"))

	status, body = do(t, r, "GET", path+"/branches/box", nil)
	require.Equal(t, http.StatusOK, status)
	_, hasURI := body["connection_string"]
	require.False(t, hasURI, "branch details reissued a URI")
	status, body = do(t, r, "GET", path+"/sandboxes", nil)
	require.Equal(t, http.StatusOK, status)
	items := body["sandboxes"].([]any)
	require.Len(t, items, 1)
	require.Nil(t, items[0].(map[string]any)["capture_error"])
	_, hasURI = items[0].(map[string]any)["connection_string"]
	require.False(t, hasURI, "sandbox list reissued a URI")
	status, body = do(t, r, "POST", path+"/branches/box/checkout", nil)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, body["error"], "returned at creation")

	for _, request := range []map[string]any{
		{"name": "omitted"},
		{"name": "disabled", "credential": false},
	} {
		status, body := do(t, r, "POST", path+"/sandboxes", request)
		require.Equal(t, http.StatusCreated, status)
		legacy, err := services.Branches.GetBranch(project.ID, request["name"].(string))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, services.Sandbox.Discard(ctx, legacy.ID)) })
		keys := make([]string, 0, len(body))
		for key := range body {
			keys = append(keys, key)
		}
		require.ElementsMatch(t, []string{"branch", "connection_string", "expires_at", "forked_from", "fork_lsn", "indexes", "index_notes"}, keys)
		require.True(t, body["connection_string"] == services.BranchConnectionString(legacy.PhysicalDB),
			"sandboxes without a credential must return the original URI")
	}
}

func TestHostGuardAcceptsOnlyLoopbackAndListedHosts(t *testing.T) {
	serve := func(allowed, host, path string) int {
		r := gin.New()
		r.Use(hostGuard(allowed))
		r.GET("/health", func(c *gin.Context) { c.Status(200) })
		r.GET("/api/v1/projects", func(c *gin.Context) { c.Status(200) })
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, host := range []string{
		"localhost", "localhost:1818", "LOCALHOST:1818", "localhost.:1818", "console.localhost:1818",
		"127.0.0.1", "127.0.0.1:1818", "127.9.9.9:80", "[::1]", "[::1]:1818",
	} {
		require.Equal(t, 200, serve("", host, "/api/v1/projects"), host)
	}
	for _, host := range []string{
		"", "evil.example", "evil.example:1818", "localhost.evil.example:1818", "notlocalhost:1818",
		"192.168.1.10:1818", "0.0.0.0:1818", "[::]:1818", "10.0.0.1",
	} {
		require.Equal(t, 403, serve("", host, "/api/v1/projects"), host)
		require.Equal(t, 200, serve("", host, "/health"), "probes stay open: "+host)
	}
	// A tunnel or reverse proxy name is opted in by hostname, any port.
	require.Equal(t, 200, serve("console.internal, Other.Example.", "console.internal:8443", "/api/v1/projects"))
	require.Equal(t, 200, serve("console.internal, Other.Example.", "other.example", "/api/v1/projects"))
	require.Equal(t, 403, serve("console.internal", "evil.example", "/api/v1/projects"))
	require.Equal(t, 200, serve("*", "anything.example", "/api/v1/projects"))
}

// A page on another site can make the browser resolve its own hostname to
// 127.0.0.1 (DNS rebinding). Its requests then look same-origin, so only the
// Host header tells them apart from the user's own.
func TestUnauthenticatedControlPlaneRejectsReboundHosts(t *testing.T) {
	name := fmt.Sprintf("argon_api_host_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", name)
	require.NoError(t, err)
	defer func() { _ = services.Client.Database(name).Drop(context.Background()) }()

	request := func(r http.Handler, method, host, token string) int {
		req := httptest.NewRequest(method, "http://"+host+"/api/v1/projects", strings.NewReader(`{"name":"rebound"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://"+host)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	local := NewRouterWith(services, Options{})
	defer local.Shutdown()
	require.Equal(t, http.StatusForbidden, request(local, "GET", "evil.example:1818", ""))
	require.Equal(t, http.StatusForbidden, request(local, "POST", "evil.example:1818", ""))
	_, err = services.Projects.GetProjectByName("rebound")
	require.Error(t, err, "the rebound write must not have reached the engine")
	require.Equal(t, http.StatusOK, request(local, "GET", "127.0.0.1:1818", ""))
	require.Equal(t, http.StatusCreated, request(local, "POST", "localhost:1818", ""))

	// With a token the request is authenticated instead; public names work.
	tokened := NewRouterWith(services, Options{Token: "sesame"})
	defer tokened.Shutdown()
	require.Equal(t, http.StatusUnauthorized, request(tokened, "GET", "api.example.com", ""))
	require.Equal(t, http.StatusOK, request(tokened, "GET", "api.example.com", "sesame"))

	// An explicit allowlist names the extra hosts a local console answers to.
	listed := NewRouterWith(services, Options{AllowedHosts: "console.internal"})
	defer listed.Shutdown()
	require.Equal(t, http.StatusOK, request(listed, "GET", "console.internal:8443", ""))
	require.Equal(t, http.StatusForbidden, request(listed, "GET", "evil.example:1818", ""))
}

// The demo write budget is keyed on the client address. A caller must not be
// able to mint fresh budgets by inventing X-Forwarded-For values.
func TestDemoWriteBudgetIgnoresUntrustedForwardingHeaders(t *testing.T) {
	name := fmt.Sprintf("argon_api_proxy_test_%d", time.Now().UnixNano())
	services, err := walcli.NewServicesAt("mongodb://localhost:27017", name)
	require.NoError(t, err)
	defer func() { _ = services.Client.Database(name).Drop(context.Background()) }()

	write := func(r http.Handler, forwardedFor string) int {
		req := httptest.NewRequest("POST", "/api/v1/events", strings.NewReader(`{"event":"first_diff"}`))
		req.Header.Set("X-Forwarded-For", forwardedFor)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	direct := NewRouterWith(services, Options{DemoMode: true, DemoWriteLimit: 3})
	defer direct.Shutdown()
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusUnauthorized, write(direct, fmt.Sprintf("203.0.113.%d", i)), "within budget, but no session")
	}
	require.Equal(t, http.StatusTooManyRequests, write(direct, "203.0.113.99"), "a new forged address shares the peer's budget")

	// Behind a declared proxy the forwarded address is the client.
	proxied := NewRouterWith(services, Options{DemoMode: true, DemoWriteLimit: 3, TrustedProxies: "192.0.2.0/24"})
	defer proxied.Shutdown()
	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusUnauthorized, write(proxied, fmt.Sprintf("203.0.113.%d", i)), "each real client has its own budget")
	}
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusUnauthorized, write(proxied, "203.0.113.200"))
	}
	require.Equal(t, http.StatusUnauthorized, write(proxied, "203.0.113.200"))
	require.Equal(t, http.StatusTooManyRequests, write(proxied, "203.0.113.200"))
}
