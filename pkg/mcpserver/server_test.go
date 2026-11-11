package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchange sends one request through a server that has no engine behind it
// and decodes the reply. Listing tools, the handshake and refusals never
// reach the engine.
func exchange(t *testing.T, s *Server, method string, params interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	out := s.out.(*bytes.Buffer)
	out.Reset()
	s.dispatch(context.Background(), &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: raw})
	var reply map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &reply), out.String())
	return reply
}

func newBareServer(opts ...Option) *Server {
	return New(nil, bytes.NewReader(nil), &bytes.Buffer{}, opts...)
}

func listedTools(t *testing.T, s *Server) map[string]map[string]interface{} {
	t.Helper()
	result := exchange(t, s, "tools/list", nil)["result"].(map[string]interface{})
	tools := make(map[string]map[string]interface{})
	for _, raw := range result["tools"].([]interface{}) {
		tool := raw.(map[string]interface{})
		tools[tool["name"].(string)] = tool
	}
	return tools
}

func TestInitializeReportsTheRealVersionAndNegotiatesTheRevision(t *testing.T) {
	s := newBareServer()
	for requested, want := range map[string]string{
		"2024-11-05": "2024-11-05",
		"2025-03-26": "2025-03-26",
		"2025-06-18": "2025-06-18",
		"2099-01-01": "2025-06-18", // unknown: answer with the newest we speak
		"":           "2025-06-18",
	} {
		result := exchange(t, s, "initialize", map[string]interface{}{"protocolVersion": requested})["result"].(map[string]interface{})
		assert.Equal(t, want, result["protocolVersion"], "requested %q", requested)
		info := result["serverInfo"].(map[string]interface{})
		assert.Equal(t, "argon", info["name"])
		assert.Equal(t, version.String(), info["version"])
	}
}

func TestEveryToolDeclaresItsBehaviour(t *testing.T) {
	tools := listedTools(t, newBareServer())
	require.Len(t, tools, len(toolHandlers))
	for name := range toolHandlers {
		tool, ok := tools[name]
		require.True(t, ok, "%s is listed", name)
		_, described := toolTraits[name]
		require.True(t, described, "%s has traits", name)
		assert.NotEmpty(t, tool["title"], name)
		annotations := tool["annotations"].(map[string]interface{})
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			_, present := annotations[hint].(bool)
			assert.True(t, present, "%s declares %s", name, hint)
		}
		assert.False(t, annotations["readOnlyHint"].(bool) && annotations["destructiveHint"].(bool), "%s cannot be both", name)
	}
	// The operations that can lose or overwrite data are the ones a client
	// should stop and ask about.
	var destructive []string
	for name, tool := range tools {
		if tool["annotations"].(map[string]interface{})["destructiveHint"].(bool) {
			destructive = append(destructive, name)
		}
	}
	sort.Strings(destructive)
	assert.Equal(t, []string{"argon_merge_apply", "argon_sandbox_discard", "argon_undo"}, destructive)
}

func TestWithheldToolsAreNeitherListedNorCallable(t *testing.T) {
	call := func(s *Server, tool string) map[string]interface{} {
		return exchange(t, s, "tools/call", map[string]interface{}{"name": tool, "arguments": map[string]interface{}{"plan_id": "x", "project": "p"}})
	}

	s := newBareServer(DisableTools("argon_merge_apply"))
	tools := listedTools(t, s)
	assert.NotContains(t, tools, "argon_merge_apply")
	assert.Contains(t, tools, "argon_merge_preview")
	assert.Len(t, tools, len(toolHandlers)-1)
	reply := call(s, "argon_merge_apply")
	require.NotNil(t, reply["error"], "a withheld tool is refused even when called blind")
	assert.Contains(t, reply["error"].(map[string]interface{})["message"], "disabled")

	readOnly := newBareServer(ReadOnly())
	var names []string
	for name := range listedTools(t, readOnly) {
		names = append(names, name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"argon_branch_list", "argon_diff", "argon_pin_list"}, names)
	for _, tool := range []string{"argon_sandbox_create", "argon_undo", "argon_merge_apply", "argon_connect"} {
		assert.NotNil(t, call(readOnly, tool)["error"], tool)
	}

	assert.Equal(t, []string{"argon_merge_aply"}, UnknownTools([]string{"argon_merge_apply", "argon_merge_aply", ""}))
}
