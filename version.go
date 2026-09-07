// Package argon exposes the version recorded in the source distribution.
package argon

import _ "embed"

// SourceVersion is shared by local CLI, API, and MCP builds.
//
//go:embed VERSION
var SourceVersion string
