package server

import (
	"fmt"
	"net/http"
	"strings"
)

// NormalizeBasePath validates the path prefix the console is served under
// behind a reverse proxy, e.g. "/p/acme", and returns it without a trailing
// slash ("" for the root). Only unreserved URL characters and "/" are
// allowed, without empty, "." or ".." segments, so the value can be written
// into the page as is.
func NormalizeBasePath(path string) (string, error) {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		return "", nil
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("base path %q must start with /", path)
	}
	for _, segment := range strings.Split(trimmed[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("base path %q has an empty, . or .. segment", path)
		}
		for _, r := range segment {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
				return "", fmt.Errorf("base path %q may contain only letters, digits, -._~ and /", path)
			}
		}
	}
	return trimmed, nil
}

// ServeHTTP strips the base path when a proxy forwards requests with it, so
// the API and console answer whether or not the proxy removed the prefix.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if base := r.opts.BasePath; base != "" {
		if p := req.URL.Path; p == base || strings.HasPrefix(p, base+"/") {
			stripped := strings.TrimPrefix(p, base)
			if stripped == "" {
				stripped = "/"
			}
			req.URL.Path = stripped
			if req.URL.RawPath != "" {
				req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, base)
				if req.URL.RawPath == "" {
					req.URL.RawPath = "/"
				}
			}
		}
	}
	r.Engine.ServeHTTP(w, req)
}

// withBase writes <base href> into the console's index.html, so its
// relative asset URLs, client routes and API calls resolve under the base
// path from any route.
func withBase(index []byte, base string) []byte {
	page := string(index)
	if strings.Contains(page, "<base ") {
		return index
	}
	return []byte(strings.Replace(page, "<head>", `<head>`+"\n    "+`<base href="`+base+`/">`, 1))
}
