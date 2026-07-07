// Serving the embedded web console. The SPA build is vendored into
// ui/dist by scripts/sync-ui.sh (a placeholder page ships by default), so
// one binary carries both the control plane and its UI.

package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed all:ui/dist
var uiFS embed.FS

// mountUI serves the embedded console on every path the API does not
// claim. Unknown paths fall back to index.html so the SPA router owns
// client-side routes.
func (r *Router) mountUI() {
	dist, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		return
	}
	fileServer := http.FileServer(http.FS(dist))
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/") || p == "/health" ||
			(c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if f, err := dist.Open(strings.TrimPrefix(p, "/")); err == nil {
			st, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !st.IsDir() {
				// Asset filenames are content-hashed, so they're safe to
				// cache hard and forever.
				if strings.HasPrefix(p, "/assets/") {
					c.Header("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
		}
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "console UI is not bundled in this build"})
			return
		}
		// The SPA entry must always revalidate — otherwise a returning
		// visitor keeps running a stale bundle (and pointing at asset
		// hashes a newer deploy no longer serves).
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	})
}
