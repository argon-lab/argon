package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBasePath(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", "/p/acme": "/p/acme", "/p/acme/": "/p/acme", "/p/my-proj_1.v2": "/p/my-proj_1.v2"} {
		got, err := NormalizeBasePath(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"p/acme", "/p//acme", "/p/../x", "/p/a b", `/p/"x`, "/p/<x>", "/p/a?b", "/p/a#b", "/p/%2e%2e", "https://x/p"} {
		_, err := NormalizeBasePath(in)
		assert.Error(t, err, in)
	}
}

// Behind a reverse proxy that mounts the console under a prefix, the page
// names that prefix in <base href> so its assets, routes and API calls go
// through the proxy; the API answers with or without the prefix.
func TestConsoleUnderABasePath(t *testing.T) {
	r := bareRouter(Options{BasePath: "/p/acme"})
	for _, path := range []string{"/p/acme/", "/p/acme/branches/main", "/branches/main", "/p/acme"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Contains(t, rec.Body.String(), `<base href="/p/acme/">`, path)
	}
	for _, path := range []string{"/p/acme/health", "/health"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, rec.Code, path)
	}
	// A path that only shares the prefix's characters is not under it.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p/acmeX/health", nil))
	assert.NotContains(t, rec.Body.String(), `"status"`)

	// At the root the page still carries a base, so relative assets resolve
	// from any route.
	root := bareRouter(Options{})
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branches/main", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `<base href="/">`)
	assert.Equal(t, 1, strings.Count(rec.Body.String(), "<base "))
}

// bareRouter serves /health and the console with no engine behind it.
func bareRouter(opts Options) *Router {
	r := &Router{Engine: gin.New(), opts: opts}
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.mountUI()
	return r
}
