// Package version provides one version for every Argon entry point.
package version

import (
	"strings"

	"github.com/argon-lab/argon"
)

// Build overrides VERSION in release builds. Set it with:
// -ldflags "-X github.com/argon-lab/argon/pkg/version.Build=2.0.1".
var Build string

// String returns the release tag version, or VERSION for a source build.
func String() string {
	if Build != "" {
		return strings.TrimSpace(Build)
	}
	return strings.TrimSpace(argon.SourceVersion)
}
