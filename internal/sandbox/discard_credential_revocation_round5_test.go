package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Round 5 (4c24144, sandbox.discard: "Another discard, or the sweeper, may
// remove the sandbox at any of these steps; each then answers "branch not
// found", not the driver's "no documents"").
//
// The steps that read the branch do. A credential sandbox has one more step
// that notices a branch that is gone, and it is the one the same commit
// handles in removeFailed: checkout.discard's BeginSandboxRevocation, whose
// update matches nothing and reports ErrBranchChanged. removeFailed now
// reads the branch again and sees that it is gone. discard does not, so a
// discard of a credential sandbox that another discard (the agent and the
// TTL sweeper, a retried DELETE) removed in that window answers
//
//	failed to release the sandbox: branch changed concurrently; reload and retry
//
// for a sandbox that no longer exists.
func TestRound5_DiscardOfACredentialSandboxRemovedBeforeItsRevocationSaysTheBranchIsNotFound(t *testing.T) {
	ctx := context.Background()
	plain, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)
	dbName := fmt.Sprintf("argon_round5_credential_%d", time.Now().UnixNano())
	db := plain.Database(dbName)
	var physical []string
	t.Cleanup(func() {
		for _, name := range physical {
			_ = plain.Database(name).RunCommand(context.Background(), bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err()
			_ = plain.Database(name).Drop(context.Background())
		}
		_ = db.Drop(context.Background())
		_ = plain.Disconnect(context.Background())
	})
	project := "round5-credential"
	other := newRound3Process(t, db) // the discard that finishes first

	// The discard under test stops just before it marks the sandbox as
	// being revoked.
	var once sync.Once
	beforeRevocation := func() {}
	opts := options.Client().ApplyURI("mongodb://localhost:27017")
	opts.SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.DatabaseName != dbName || e.CommandName != "update" {
			return
		}
		if coll, _ := e.Command.Lookup("update").StringValueOK(); coll != "wal_branches" {
			return
		}
		if strings.Contains(e.Command.String(), "sandbox_revoking") {
			once.Do(func() { beforeRevocation() })
		}
	}})
	slowClient, err := mongo.Connect(ctx, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slowClient.Disconnect(context.Background()) })
	slow := newRound3Process(t, slowClient.Database(dbName))

	main, err := other.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	writer := walwriter.New(walService, other.branches, materializer.NewService(walService, other.branches), main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "seed"})
	require.NoError(t, err)

	baseURI := "mongodb://localhost:27017"
	info, err := other.sandbox.create(ctx, project, main.ID, "run-1", time.Hour, &baseURI)
	require.NoError(t, err)
	physical = append(physical, info.PhysicalDB)
	require.NotEmpty(t, info.CredentialURI, "the control: a credential sandbox")

	var otherErr error
	held := false
	beforeRevocation = func() {
		held = true
		otherErr = other.sandbox.Discard(ctx, info.BranchID)
	}
	discardErr := slow.sandbox.Discard(ctx, info.BranchID)
	t.Logf("the discard held before its revocation returned: %v", discardErr)

	require.True(t, held, "the control: the discard got as far as its revocation")
	require.NoError(t, otherErr, "the control: the other discard removed the sandbox")
	_, err = other.branches.GetBranchByID(info.BranchID)
	require.ErrorIs(t, err, mongo.ErrNoDocuments, "the control: the sandbox is gone")

	require.Error(t, discardErr)
	assert.ErrorIs(t, discardErr, branchwal.ErrBranchNotFound,
		"the sandbox is gone; the discard answered: %v", discardErr)
	assert.NotContains(t, discardErr.Error(), "reload and retry",
		"there is nothing to reload: the sandbox does not exist")
}
