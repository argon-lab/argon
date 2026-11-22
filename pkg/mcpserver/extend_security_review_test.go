package mcpserver

import (
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A read-only server, or one started with the tool disabled, refuses the
// call even from a client that never listed it.
func TestReviewExtendRefusedWhenWithheld(t *testing.T) {
	for name, opt := range map[string]Option{"read-only": ReadOnly(), "disabled": DisableTools("argon_sandbox_extend")} {
		c := serve(t, func(in io.Reader, out io.Writer) *Server { return NewLazy(nil, in, out, opt) })
		reply := c.call("tools/call", map[string]interface{}{"name": "argon_sandbox_extend",
			"arguments": map[string]interface{}{"project": "shop", "branch": "run-1", "ttl_minutes": 60}})
		require.NotNil(t, reply["error"], name)
		assert.Contains(t, reply["error"].(map[string]interface{})["message"], "not available", name)
	}
}

// ttl_minutes beyond what time.Duration holds is converted with
// float64->int64, whose result Go leaves to the platform: arm64 saturates
// to ~292 years (the sandbox is in effect kept, though argon_sandbox_keep
// may be disabled), amd64 yields math.MinInt64 and the call fails with
// "must be positive". Either way the input should be refused as too large.
func TestReviewExtendRefusesOverflowingTTL(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	c := serve(t, func(in io.Reader, out io.Writer) *Server {
		return NewLazy(e.connect, in, out, DisableTools("argon_sandbox_keep"))
	})
	_, _ = c.tool("argon_branch_list", map[string]interface{}{"project": "shop"})
	services := e.services.Load()
	require.NotNil(t, services)
	_, err := services.Projects.CreateProject("shop")
	require.NoError(t, err)
	text, failed := c.tool("argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "run-1", "ttl_minutes": 5})
	require.False(t, failed, text)

	for _, minutes := range []float64{1e300, 1e12, 2e8} {
		text, failed = c.tool("argon_sandbox_extend", map[string]interface{}{"project": "shop", "branch": "run-1", "ttl_minutes": minutes})
		t.Logf("GOARCH=%s ttl_minutes=%g -> failed=%v %s", runtime.GOARCH, minutes, failed, text)
		if !failed {
			project, err := services.Projects.GetProjectByName("shop")
			require.NoError(t, err)
			branch, err := services.Branches.GetBranch(project.ID, "run-1")
			require.NoError(t, err)
			t.Logf("stored expiry %s", branch.ExpiresAt.UTC().Format(time.RFC3339))
		}
		assert.True(t, failed && strings.Contains(strings.ToLower(text), "large"),
			"ttl_minutes %g should be refused as too large, got failed=%v %q", minutes, failed, text)
	}
}
