package sandbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type round3Process struct {
	branches *branchwal.BranchService
	checkout *checkout.Service
	sandbox  *Service
}

func newRound3Process(t *testing.T, db *mongo.Database) *round3Process {
	t.Helper()
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branches, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	co := checkout.NewService(db.Client(), db, branches, materializer.NewService(walService, branches))
	return &round3Process{branches: branches, checkout: co, sandbox: NewService(branches, co)}
}

// round3DiscardedWhileCreating runs a sandbox create whose half-made sandbox
// another process discards while the create is still copying its data, and
// which that process then starts again under the same name: the case
// removeFailed's own comment describes ("the name may belong to another
// sandbox by now, created after someone discarded this one while it was
// still being set up"). It returns the ID of the sandbox that now holds the
// name, and the failed create's error.
func round3DiscardedWhileCreating(t *testing.T, credential bool) (retryID string, retry *round3Process, createErr error) {
	t.Helper()
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)
	db := client.Database(fmt.Sprintf("argon_round3_sandbox_%d", time.Now().UnixNano()))
	var physical []string
	t.Cleanup(func() {
		for _, name := range physical {
			_ = client.Database(name).Drop(context.Background())
		}
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	project := "round3-credential"

	slow := newRound3Process(t, db) // serves the create that fails
	retry = newRound3Process(t, db) // serves the discard and the create that starts over

	main, err := slow.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	writer := walwriter.New(walService, slow.branches, materializer.NewService(walService, slow.branches), main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "seed"})
	require.NoError(t, err)

	var once sync.Once
	slow.checkout.SetDuringLoad(func(context.Context, string) func(context.Context) {
		once.Do(func() {
			half, err := retry.branches.GetBranch(project, "run-1")
			require.NoError(t, err, "the half-made sandbox is in the branch list")
			physical = append(physical, wal.PhysicalDBName(half.ID))
			require.NoError(t, retry.sandbox.Discard(ctx, half.ID), "the client discards the half-made sandbox")
			again, err := retry.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
			require.NoError(t, err, "and starts over under the same name")
			physical = append(physical, again.PhysicalDB)
			retryID = again.BranchID
		})
		return nil
	})

	if credential {
		// create() is what CreateWithCredential calls once the deployment
		// has been seen to enforce authentication. The test deployment does
		// not, and nothing below depends on it: the create fails at its
		// checkout, before a user is issued.
		baseURI := "mongodb://localhost:27017"
		_, createErr = slow.sandbox.create(ctx, project, main.ID, "run-1", time.Hour, &baseURI)
	} else {
		_, createErr = slow.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
	}
	require.Error(t, createErr, "the create's branch was discarded while it checked out")
	require.NotEmpty(t, retryID)
	return retryID, retry, createErr
}

// Round 3 (6aa0f7a: removeFailed treats ErrBranchNotFound from the delete as
// "already removed; nothing of this create is left" and returns the cause
// alone). That is the second half of removeFailed. For a sandbox with a
// credential the first half runs first: checkout.DiscardFailedSandbox, which
// reads the branch by ID and returns the driver's mongo.ErrNoDocuments when
// it is gone. removeFailed takes that for a cleanup that failed and answers
//
//	...; cleanup of sandbox "run-1" failed (mongo: no documents in result): retry argon sandbox discard -b run-1
//
// Nothing of the create is left to clean up, and "run-1" is by now the name
// of the sandbox the client started over with: the command the error tells
// it to run discards that one.
//
// The control is the same sequence for a sandbox without a credential, which
// answers with the cause alone.
func TestRound3_FailedCredentialCreateWhoseSandboxIsGoneDoesNotSayToDiscardItsName(t *testing.T) {
	t.Run("control: without a credential", func(t *testing.T) {
		_, _, createErr := round3DiscardedWhileCreating(t, false)
		t.Logf("create returned: %v", createErr)
		assert.NotContains(t, createErr.Error(), "argon sandbox discard -b run-1")
	})
	t.Run("with a credential", func(t *testing.T) {
		retryID, retry, createErr := round3DiscardedWhileCreating(t, true)
		t.Logf("create returned: %v", createErr)
		holder, err := retry.branches.GetBranch("round3-credential", "run-1")
		require.NoError(t, err)
		require.Equal(t, retryID, holder.ID, "the control: run-1 is the sandbox that started over, and it is live")
		assert.NotContains(t, createErr.Error(), "argon sandbox discard -b run-1",
			"nothing of the failed create is left, and its error says to discard run-1, which is now sandbox %s", retryID)
		assert.NotContains(t, createErr.Error(), "mongo: no documents in result")
	})
}
