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

// Values that would break out of <base href="..."> or change its meaning
// are refused, so nothing reaches withBase unescaped.
func TestReviewBasePathRefusesMarkup(t *testing.T) {
	for _, in := range []string{
		`/p/"><script>alert(1)</script>`, `/p/'x`, "/p/a\x00b", "/p/a\nb", "/p/a\tb",
		`/p/a\b`, "//evil.com", "/p/%22", "/p/a&b", "/p/a=b", "/p/a;b", "/p/a@b",
		"/p/é", "/p/a:b", "/./x", "/x/.", "/x/..", "javascript:alert(1)", " /p",
	} {
		_, err := NormalizeBasePath(in)
		assert.Error(t, err, "%q", in)
	}
	got, err := NormalizeBasePath("/p/...")
	require.NoError(t, err)
	assert.Equal(t, `<head>`+"\n    "+`<base href="/p/.../">`, string(withBase([]byte("<head>"), got)))
}

// securedRouter mirrors NewRouterWith's middleware order with sentinel
// handlers in place of the engine, so a bypass shows as "LEAK".
func securedRouter(base string, readOnly bool) *Router {
	opts := Options{BasePath: base, Token: "secret", AllowedHosts: "", ReadOnly: readOnly}
	r := &Router{Engine: gin.New(), opts: opts}
	r.Use(hostGuard(""))
	r.Use(corsMiddleware(""))
	r.Use(authMiddleware("secret"))
	if readOnly {
		r.Use(readOnlyMiddleware())
	}
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/api/v1/projects", func(c *gin.Context) { c.String(http.StatusOK, "LEAK") })
	r.POST("/api/v1/projects", func(c *gin.Context) { c.String(http.StatusOK, "LEAK") })
	r.mountUI()
	return r
}

// No spelling of a prefixed path reaches an API handler without the token,
// past the Host guard, or as a write on a read-only server.
func TestReviewPrefixStripDoesNotBypassMiddleware(t *testing.T) {
	paths := []string{
		"/p/acme/api/v1/projects", "/api/v1/projects",
		"/p/acme/../api/v1/projects", "/p/acme/./api/v1/projects",
		"/p/acme%2Fapi/v1/projects", "/p/acme%2fapi%2fv1%2fprojects",
		"/p/ac%6De/api/v1/projects", "/p/acme//api/v1/projects",
		"//p/acme/api/v1/projects", "/p/acme/p/acme/api/v1/projects",
		"/P/ACME/api/v1/projects", "/p/acme/api/v1/projects/",
		"/p/acme/%2e%2e/api/v1/projects", "/p/acme/api%2Fv1/projects",
	}
	for _, readOnly := range []bool{false, true} {
		r := securedRouter("/p/acme", readOnly)
		for _, p := range paths {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				req := httptest.NewRequest(method, "http://localhost"+p, nil)
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				assert.NotContains(t, rec.Body.String(), "LEAK", "%s %s without token", method, p)

				// With a foreign Host the guard answers first.
				req = httptest.NewRequest(method, "http://evil.example"+p, nil)
				req.Header.Set("Authorization", "Bearer secret")
				rec = httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				assert.NotContains(t, rec.Body.String(), "LEAK", "%s %s with foreign Host", method, p)

				if readOnly && method == http.MethodPost {
					req = httptest.NewRequest(method, "http://localhost"+p, nil)
					req.Header.Set("Authorization", "Bearer secret")
					rec = httptest.NewRecorder()
					r.ServeHTTP(rec, req)
					assert.NotContains(t, rec.Body.String(), "LEAK", "POST %s on read-only", p)
				}
			}
		}
	}
}

// The page is never cached hard, under the prefix or not, and the embedded
// FS cannot be left through the stripped path.
func TestReviewIndexCacheAndTraversal(t *testing.T) {
	r := bareRouter(Options{BasePath: "/p/acme"})
	for _, p := range []string{"/p/acme", "/p/acme/", "/p/acme/assets/", "/p/acme/assets/nope.js", "/p/acme/../../etc/passwd", "/p/acme/%2e%2e/go.mod", "/p/acme//index.html"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<base ") {
			assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), p)
		}
		assert.NotContains(t, rec.Body.String(), "root:", p)
		assert.NotContains(t, rec.Body.String(), "module ", p)
	}
	// FileServer's index.html redirect stays relative, so it keeps the prefix.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p/acme/index.html", nil))
	assert.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.Equal(t, "./", rec.Header().Get("Location"))
}

// A redirect answered for a prefixed request must stay under the prefix;
// otherwise the browser leaves this console for whatever the proxy serves
// at the root (another project's mount, the proxy's own app).
func TestReviewTrailingSlashRedirectKeepsBasePathOverHTTP(t *testing.T) {
	r := bareRouter(Options{BasePath: "/p/acme"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p/acme/health/", nil))
	require.Equal(t, http.StatusMovedPermanently, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "/p/acme/"),
		"Location %q drops the base path", rec.Header().Get("Location"))
}
