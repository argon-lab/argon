package mcpserver

import (
	"testing"
)

// Round 3 coverage: no Origin header, however crafted, panics the code that
// runs before the token check.
func FuzzRound3OriginNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"http://localhost", "http://[::1]:3000", "http://[::ffff:127.0.0.1]", "chrome-extension://abc",
		"http://[fe80::1%25en0]", "http://a:b@c", "http://%zz", "http://[", "http://[::1", "//x", "x:",
		"http://localhost:99999999999999999999", "http://é", "HTTP://LOCALHOST:080/",
	} {
		f.Add(seed)
	}
	allowed := map[string]bool{"https://console.example.com": true}
	f.Fuzz(func(t *testing.T, origin string) {
		_ = originAllowed(origin, allowed)
		if normalized, err := NormalizeOrigin(origin); err == nil {
			again, err := NormalizeOrigin(normalized)
			if err != nil || again != normalized {
				t.Fatalf("NormalizeOrigin(%q) = %q, which normalizes to %q, %v", origin, normalized, again, err)
			}
		}
	})
}
