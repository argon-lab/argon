package mcpserver

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An agent that needs more time pushes its sandbox's expiry out instead of
// keeping it forever. The tool has the name, arguments and reply Argon Cloud's
// control plane uses for the same tool.
func TestSandboxExtend(t *testing.T) {
	tool := listedTools(t, newBareServer())["argon_sandbox_extend"]
	require.NotNil(t, tool, "argon_sandbox_extend is listed")
	assert.ElementsMatch(t, []interface{}{"project", "branch", "ttl_minutes"}, tool["inputSchema"].(map[string]any)["required"])
	assert.NotContains(t, listedTools(t, newBareServer(ReadOnly())), "argon_sandbox_extend")

	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	c := serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(e.connect, in, out) })
	_, failed := c.tool("argon_branch_list", map[string]any{"project": "shop"})
	require.True(t, failed)
	services := e.services.Load()
	require.NotNil(t, services)
	_, err := services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed := c.tool("argon_sandbox_create", map[string]any{"project": "shop", "name": "run-1", "ttl_minutes": 5})
	require.False(t, failed, text)

	before := time.Now()
	text, failed = c.tool("argon_sandbox_extend", map[string]any{"project": "shop", "branch": "run-1", "ttl_minutes": 120})
	require.False(t, failed, text)
	assert.True(t, strings.HasPrefix(text, `Sandbox "run-1" now expires at `), text)
	project, err := services.Projects.GetProjectByName("shop")
	require.NoError(t, err)
	branch, err := services.Branches.GetBranch(project.ID, "run-1")
	require.NoError(t, err)
	require.NotNil(t, branch.ExpiresAt)
	assert.WithinDuration(t, before.Add(120*time.Minute), *branch.ExpiresAt, time.Minute)

	for _, bad := range []map[string]any{
		{"project": "shop", "branch": "run-1"},
		{"project": "shop", "branch": "run-1", "ttl_minutes": 0},
		{"project": "shop", "branch": "run-1", "ttl_minutes": -5},
		{"project": "shop", "branch": "run-1", "ttl_minutes": "60"},
		{"project": "shop", "branch": "main", "ttl_minutes": 60},
	} {
		text, failed = c.tool("argon_sandbox_extend", bad)
		assert.True(t, failed, "%v: %s", bad, text)
	}
}
