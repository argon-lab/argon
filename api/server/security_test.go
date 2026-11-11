package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
