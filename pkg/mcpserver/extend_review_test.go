package mcpserver

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Passing guards: listing, restriction and the unknown-tool check know the
// new tool.
func TestExtendReviewListingAndRestriction(t *testing.T) {
	assert.Empty(t, UnknownTools([]string{"argon_sandbox_extend"}))
	assert.Len(t, toolDescriptors(), len(toolHandlers))
	assert.Len(t, toolTraits, len(toolHandlers))
	tools := listedTools(t, newBareServer(DisableTools("argon_sandbox_extend")))
	assert.NotContains(t, tools, "argon_sandbox_extend")
	assert.Len(t, tools, 13)
	ann := listedTools(t, newBareServer())["argon_sandbox_extend"]["annotations"].(map[string]any)
	assert.Equal(t, false, ann["readOnlyHint"])
	assert.Equal(t, false, ann["destructiveHint"])
}

// The tool is annotated idempotentHint=true, which the MCP spec defines as
// "calling repeatedly with the same arguments has no additional effect".
// Its effect is relative to now, so a repeated call moves the expiry again.
func TestExtendReviewIdempotentHintMatchesBehaviour(t *testing.T) {
	ann := listedTools(t, newBareServer())["argon_sandbox_extend"]["annotations"].(map[string]any)

	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	c := serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(e.connect, in, out) })
	_, _ = c.tool("argon_branch_list", map[string]any{"project": "shop"})
	services := e.services.Load()
	require.NotNil(t, services)
	_, err := services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed := c.tool("argon_sandbox_create", map[string]any{"project": "shop", "name": "idem-1", "ttl_minutes": 5})
	require.False(t, failed, text)

	expiry := func() time.Time {
		project, err := services.Projects.GetProjectByName("shop")
		require.NoError(t, err)
		branch, err := services.Branches.GetBranch(project.ID, "idem-1")
		require.NoError(t, err)
		require.NotNil(t, branch.ExpiresAt)
		return *branch.ExpiresAt
	}
	args := map[string]any{"project": "shop", "branch": "idem-1", "ttl_minutes": 60}
	text, failed = c.tool("argon_sandbox_extend", args)
	require.False(t, failed, text)
	first := expiry()
	time.Sleep(1100 * time.Millisecond)
	text, failed = c.tool("argon_sandbox_extend", args)
	require.False(t, failed, text)
	second := expiry()

	repeatedCallHadAnEffect := !second.Equal(first)
	assert.False(t, ann["idempotentHint"] == true && repeatedCallHadAnEffect,
		"idempotentHint=%v but the same call moved expiry from %s to %s", ann["idempotentHint"], first, second)
}
