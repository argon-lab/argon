package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/pkg/walcli"
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
	defer services.Client.Database(name).Drop(context.Background())
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
	defer services.Sandbox.Discard(context.Background(), box.BranchID)
	for _, path := range []string{"/api/v1/projects/" + p + "/branches/internal-box", "/api/v1/projects/" + p + "/sandboxes"} {
		code, body, _ = doDemo(t, r, "GET", path, cookie, nil)
		require.Equal(t, http.StatusOK, code)
		require.NotContains(t, fmt.Sprint(body), "connection_string")
		require.NotContains(t, fmt.Sprint(body), "service:secret")
	}
}
