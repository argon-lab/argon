package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// Round 4 (0e6aae9: removeFailed treats ErrBranchNotFound from
// DiscardFailedSandbox as "someone discarded it while it was being set up;
// nothing of this create is left, and its name may be another sandbox's").
//
// DiscardFailedSandbox has a second answer for the same thing. Between its
// read of the branch and the update that marks it as being revoked
// (BeginSandboxRevocation) there is no fence, and that update reports a
// branch that is gone as ErrBranchChanged, because its filter matched
// nothing. A discard from another process that finishes in between leaves
// the failed create with
//
//	...; cleanup of sandbox "run-1" failed (branch changed concurrently;
//	reload and retry): retry argon sandbox discard -b run-1
//
// which is what round 3 objected to: nothing of the create is left, and
// "run-1" may by now be the sandbox the client started over with.
//
// The window is one metadata round trip wide. It is held open here on the
// failing create's own client, just before it sends that update.
func TestRound4_FailedCredentialCreateDiscardedBeforeItsRevocationDoesNotSayToDiscardItsName(t *testing.T) {
	ctx := context.Background()
	plain, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)
	dbName := fmt.Sprintf("argon_round4_sandbox_%d", time.Now().UnixNano())
	db := plain.Database(dbName)
	var physical []string
	t.Cleanup(func() {
		for _, name := range physical {
			_ = plain.Database(name).Drop(context.Background())
		}
		_ = db.Drop(context.Background())
		_ = plain.Disconnect(context.Background())
	})
	project := "round4-credential"

	retry := newRound3Process(t, db) // serves the discard and the create that starts over

	// The failing create runs on a client of its own, which stops just
	// before it marks its sandbox as being revoked.
	var once sync.Once
	var retryID string
	beforeRevocation := func() {}
	opts := options.Client().ApplyURI("mongodb://localhost:27017")
	opts.SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.DatabaseName != dbName || e.CommandName != "update" {
			return
		}
		if target, _ := e.Command.Lookup("update").StringValueOK(); target != "wal_branches" {
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

	main, err := retry.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	writer := walwriter.New(walService, retry.branches, materializer.NewService(walService, retry.branches), main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "seed"})
	require.NoError(t, err)

	beforeRevocation = func() {
		half, err := retry.branches.GetBranch(project, "run-1")
		require.NoError(t, err, "the half-made sandbox is in the branch list")
		physical = append(physical, wal.PhysicalDBName(half.ID))
		require.NoError(t, retry.sandbox.Discard(ctx, half.ID), "the client discards the half-made sandbox")
		again, err := retry.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
		require.NoError(t, err, "and starts over under the same name")
		physical = append(physical, again.PhysicalDB)
		retryID = again.BranchID
	}

	// The create fails at its checkout: its request is cancelled while it
	// copies the data.
	createCtx, cancelCreate := context.WithCancel(ctx)
	defer cancelCreate()
	slow.checkout.SetDuringLoad(func(context.Context, string) func(context.Context) {
		cancelCreate()
		return nil
	})
	baseURI := "mongodb://localhost:27017"
	_, createErr := slow.sandbox.create(createCtx, project, main.ID, "run-1", time.Hour, &baseURI)
	require.Error(t, createErr)
	t.Logf("create returned: %v", createErr)
	require.NotEmpty(t, retryID, "the control: the other process discarded the sandbox and started over before the revocation")

	holder, err := retry.branches.GetBranch(project, "run-1")
	require.NoError(t, err)
	require.Equal(t, retryID, holder.ID, "the control: run-1 is the sandbox that started over, and it is live")
	assert.NotContains(t, createErr.Error(), "argon sandbox discard -b run-1",
		"nothing of the failed create is left, and its error says to discard run-1, which is now sandbox %s", retryID)
}
