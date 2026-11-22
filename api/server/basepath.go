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
	switch first := strings.ToLower(strings.SplitN(trimmed[1:], "/", 2)[0]); first {
	case "api", "assets", "health":
		// A proxy that strips the prefix would have it stripped again here.
		return "", fmt.Errorf("base path %q starts with /%s, which the console itself serves; choose another prefix", path, first)
	}
	for _, segment := range strings.Split(trimmed[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("base path %q has an empty, . or .. segment", path)
		}
		for _, r := range segment {
			if !unreserved(r) {
				return "", fmt.Errorf("base path %q may contain only letters, digits, -._~ and /", path)
			}
		}
	}
	return trimmed, nil
}

// ServeHTTP strips the base path when a proxy forwards requests with it, so
// the API and console answer whether or not the proxy removed the prefix. A
// request without the prefix gets no X-Forwarded-Prefix from the client:
// it is the server's to set.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req.Header.Del("X-Forwarded-Prefix")
	if base := r.opts.BasePath; base != "" {
		if p := req.URL.Path; p == base || strings.HasPrefix(p, base+"/") {
			stripped := strings.TrimPrefix(p, base)
			if stripped == "" {
				stripped = "/"
			}
			req.URL.Path = stripped
			// Redirects gin builds (e.g. for a trailing slash) prefix this.
			req.Header.Set("X-Forwarded-Prefix", base)
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

// unreserved reports whether r is an RFC 3986 unreserved character.
func unreserved(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-._~", r)
}
