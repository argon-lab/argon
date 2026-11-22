package mcpserver

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Over HTTP, sandbox creates with one name can arrive together. One makes
// the sandbox; the others get the answer a later duplicate gets, and leave
// no create_branch record for a branch that was never made (#220).
func TestConcurrentSandboxCreatesWithOneNameMakeOneSandbox(t *testing.T) {
	e := newEngine(t)
	t.Setenv("MONGODB_URI", replicaSet)
	s := NewLazy(e.connect, nil, nil)
	endpoint, cancel, done := serveHTTPForReview(t, s)
	ctx := context.Background()

	_, _, err := reviewToolCall(ctx, endpoint, 1, "argon_branch_list", map[string]interface{}{"project": "shop"})
	require.NoError(t, err)
	services := e.services.Load()
	require.NotNil(t, services)
	project, err := services.Projects.CreateProject("shop")
	require.NoError(t, err)

	text, failed, err := reviewToolCall(ctx, endpoint, 2, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "serial"})
	require.NoError(t, err)
	require.False(t, failed, text)
	refusal, failed, err := reviewToolCall(ctx, endpoint, 3, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "serial"})
	require.NoError(t, err)
	require.True(t, failed, "a later duplicate is refused: %s", refusal)
	assert.NotContains(t, refusal, "E11000")

	const callers = 8
	var wg sync.WaitGroup
	texts, failures := make([]string, callers), make([]bool, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var callErr error
			texts[i], failures[i], callErr = reviewToolCall(ctx, endpoint, 100+i, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "together"})
			if callErr != nil {
				texts[i], failures[i] = callErr.Error(), true
			}
		}(i)
	}
	close(start)
	wg.Wait()
	succeeded := 0
	for i := range texts {
		if !failures[i] {
			succeeded++
			continue
		}
		assert.Equal(t, refusal, texts[i], "call %d: a concurrent duplicate gets the answer a later one gets", i)
	}
	assert.Equal(t, 1, succeeded, "exactly one of the creates that arrived together succeeds")

	branches, err := services.Branches.ListBranches(project.ID)
	require.NoError(t, err)
	live := map[string]bool{}
	named := 0
	for _, b := range branches {
		live[b.ID] = true
		if b.Name == "together" {
			named++
		}
	}
	assert.Equal(t, 1, named)
	cursor, err := services.Client.Database(e.metadataDB).Collection("wal_log").Find(ctx,
		bson.M{"project_id": project.ID, "operation": "create_branch"})
	require.NoError(t, err)
	var entries []struct {
		BranchID string `bson:"branch_id"`
		LSN      int64  `bson:"lsn"`
	}
	require.NoError(t, cursor.All(ctx, &entries))
	var orphans []string
	for _, entry := range entries {
		if !live[entry.BranchID] {
			orphans = append(orphans, fmt.Sprintf("lsn %d branch %s", entry.LSN, entry.BranchID))
		}
	}
	assert.Empty(t, orphans, "create_branch records for branches that were never made")

	// A discarded sandbox's name is free: an agent that starts over under
	// the same name gets a new sandbox.
	text, failed, err = reviewToolCall(ctx, endpoint, 200, "argon_sandbox_discard", map[string]interface{}{"project": "shop", "branch": "serial"})
	require.NoError(t, err)
	require.False(t, failed, text)
	text, failed, err = reviewToolCall(ctx, endpoint, 201, "argon_sandbox_create", map[string]interface{}{"project": "shop", "name": "serial"})
	require.NoError(t, err)
	assert.False(t, failed, "the discarded sandbox's name can be used again: %s", text)

	cancel()
	require.NoError(t, <-done)
}
