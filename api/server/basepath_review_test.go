package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reviewRouter(opts Options) *Router {
	r := &Router{Engine: gin.New(), opts: opts}
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/api/v1/meta", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"meta": true}) })
	r.mountUI()
	return r
}

func reviewGet(r http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Passing guards: the root console is unchanged for existing users.
func TestReviewRootConsoleUnchanged(t *testing.T) {
	r := reviewRouter(Options{})
	for _, p := range []string{"/", "/p/shop", "/p/shop/b/main", "/p/shop/merges/abc"} {
		rec := reviewGet(r, p)
		require.Equal(t, http.StatusOK, rec.Code, p)
		assert.Contains(t, rec.Body.String(), `<base href="/">`, p)
		assert.Contains(t, rec.Body.String(), `src="./assets/`, p)
	}
	rec := reviewGet(r, "/assets/index-nZ0jnZDD.js")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusOK, reviewGet(r, "/api/v1/meta").Code)

	b := reviewRouter(Options{BasePath: "/p/acme"})
	rec = reviewGet(b, "/p/acme/assets/index-nZ0jnZDD.js")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	assert.Contains(t, reviewGet(b, "/p/acme/api/v1/meta").Body.String(), `"meta"`)
}

// The commit says the server answers with or without the prefix, so a
// proxy may strip it. NormalizeBasePath accepts a base that is also a
// server route ("/api", "/assets"); a stripping proxy's request is then
// stripped a second time and the API or asset is lost.
func TestReviewBasePathCollidingWithServerRoutes(t *testing.T) {
	// A base whose first segment is one of the server's own routes would be
	// stripped a second time from a proxy that already removed it, so it is
	// refused rather than half working.
	for _, base := range []string{"/api", "/api/v1", "/assets", "/health", "/API"} {
		_, err := NormalizeBasePath(base)
		assert.Error(t, err, base)
	}
	_, err := NormalizeBasePath("/apis")
	assert.NoError(t, err)
}

// gin's trailing-slash redirect is computed from the stripped path, so its
// Location leaves the base path and a browser behind the proxy is sent
// outside the mount.
func TestReviewTrailingSlashRedirectKeepsBasePath(t *testing.T) {
	r := reviewRouter(Options{BasePath: "/p/acme"})
	rec := reviewGet(r, "/p/acme/api/v1/meta/")
	require.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "/p/acme/"), "Location %q", rec.Header().Get("Location"))
}

func TestReviewNormalizeBasePathEdges(t *testing.T) {
	for in, want := range map[string]string{"//": "", "/p/acme//": "/p/acme", "/...": "/...", "/~u": "/~u"} {
		got, err := NormalizeBasePath(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{" /p", "/p/acme\n", "/p/./x", "/.", "/p/é", "\\p"} {
		_, err := NormalizeBasePath(in)
		assert.Error(t, err, in)
	}
	// An invalid base is reported even on loopback; flag overriding env works through Options.
	assert.Error(t, ValidateListenAddress("127.0.0.1:1818", Options{BasePath: "p"}))
	assert.NoError(t, ValidateListenAddress("127.0.0.1:1818", Options{BasePath: "/p/acme/"}))
}
