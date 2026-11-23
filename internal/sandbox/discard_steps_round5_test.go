package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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

// Round 5 (4c24144: "Another discard, or the sweeper, may remove the sandbox
// at any of these steps; each then answers "branch not found", not the
// driver's "no documents"").
//
// A discard of an ordinary checked-out sandbox reads its branch by ID six
// times on the way:
//
//	1  sandbox.discard                      "sandbox not found: %w"
//	2  RequireDeletable                     returned as it is
//	3  checkout.discard                     "failed to release the sandbox: %w"
//	4  checkout.discard, the reload         "failed to release the sandbox: %w"
//	5  DeleteBranchByID                     returned as it is
//	6  deleteBranch's RequireDeletable      returned as it is
//
// The kept tests hold a discard open before reads 4 (round 3) and 5 (round
// 2). This one holds it open before each of the six in turn, on the
// discard's own client, while another process discards the sandbox; every
// row must say the branch is not found, match ErrBranchNotFound, and not
// show the driver's text.
func TestRound5_DiscardOverlappingAnotherAtEachReadSaysTheBranchIsNotFound(t *testing.T) {
	rows := []struct {
		read int
		want string
	}{
		{1, "sandbox not found: branch not found"},
		{2, "branch not found"},
		{3, "failed to release the sandbox: branch not found"},
		{4, "failed to release the sandbox: branch not found"},
		{5, "branch not found"},
		{6, "branch not found"},
	}
	for _, row := range rows {
		t.Run(fmt.Sprintf("before read %d", row.read), func(t *testing.T) {
			ctx := context.Background()
			plain, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
			require.NoError(t, err)
			dbName := fmt.Sprintf("argon_round5_sandbox_%d_%d", time.Now().UnixNano(), row.read)
			db := plain.Database(dbName)
			var physical []string
			t.Cleanup(func() {
				for _, name := range physical {
					_ = plain.Database(name).Drop(context.Background())
				}
				_ = db.Drop(context.Background())
				_ = plain.Disconnect(context.Background())
			})
			project := "round5-discard"
			other := newRound3Process(t, db) // the discard that finishes first

			// The discard under test runs on a client of its own, which
			// stops just before it sends its Nth read of the branch.
			var target atomic.Value
			target.Store("")
			var reads atomic.Int64
			var once sync.Once
			var otherErr error
			opts := options.Client().ApplyURI("mongodb://localhost:27017")
			opts.SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
				if e.DatabaseName != dbName || e.CommandName != "find" {
					return
				}
				if coll, _ := e.Command.Lookup("find").StringValueOK(); coll != "wal_branches" {
					return
				}
				id, _ := e.Command.Lookup("filter", "_id").StringValueOK()
				if id == "" || id != target.Load().(string) {
					return
				}
				if reads.Add(1) == int64(row.read) {
					once.Do(func() { otherErr = other.sandbox.Discard(ctx, id) })
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

			info, err := other.sandbox.Create(ctx, project, main.ID, "run-1", time.Hour)
			require.NoError(t, err)
			physical = append(physical, info.PhysicalDB)

			target.Store(info.BranchID)
			discardErr := slow.sandbox.Discard(ctx, info.BranchID)
			t.Logf("the discard held before its read %d returned: %v (it made %d reads of the branch)", row.read, discardErr, reads.Load())

			require.GreaterOrEqual(t, reads.Load(), int64(row.read), "the control: the discard got as far as read %d", row.read)
			require.NoError(t, otherErr, "the control: the other discard removed the sandbox")
			_, err = other.branches.GetBranchByID(info.BranchID)
			require.ErrorIs(t, err, mongo.ErrNoDocuments, "the control: the sandbox is gone, and a lookup by ID answers with the driver's error")

			require.Error(t, discardErr, "the sandbox was removed by the other discard")
			assert.Equal(t, row.want, discardErr.Error())
			assert.ErrorIs(t, discardErr, branchwal.ErrBranchNotFound)
			assert.NotContains(t, discardErr.Error(), "no documents")
		})
	}
}

// The same for the two reads of DeleteBranchByID on its own, which
// removeFailed calls and tests with errors.Is(err, ErrBranchNotFound): a
// branch removed before either read is "already removed; nothing of this
// create is left".
func TestRound5_DeleteByIDOfABranchRemovedAtEitherReadIsErrBranchNotFound(t *testing.T) {
	for read := 1; read <= 2; read++ {
		t.Run(fmt.Sprintf("before read %d", read), func(t *testing.T) {
			ctx := context.Background()
			plain, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
			require.NoError(t, err)
			dbName := fmt.Sprintf("argon_round5_delete_%d_%d", time.Now().UnixNano(), read)
			db := plain.Database(dbName)
			t.Cleanup(func() {
				_ = db.Drop(context.Background())
				_ = plain.Disconnect(context.Background())
			})
			other := newRound3Process(t, db)

			var target atomic.Value
			target.Store("")
			var reads atomic.Int64
			var once sync.Once
			var otherErr error
			opts := options.Client().ApplyURI("mongodb://localhost:27017")
			opts.SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
				if e.DatabaseName != dbName || e.CommandName != "find" {
					return
				}
				if coll, _ := e.Command.Lookup("find").StringValueOK(); coll != "wal_branches" {
					return
				}
				id, _ := e.Command.Lookup("filter", "_id").StringValueOK()
				if id == "" || id != target.Load().(string) {
					return
				}
				if reads.Add(1) == int64(read) {
					once.Do(func() { otherErr = other.branches.DeleteBranchByID(id) })
				}
			}})
			slowClient, err := mongo.Connect(ctx, opts)
			require.NoError(t, err)
			t.Cleanup(func() { _ = slowClient.Disconnect(context.Background()) })
			slow := newRound3Process(t, slowClient.Database(dbName))

			main, err := other.branches.CreateBranch("round5-delete", "main", "")
			require.NoError(t, err)
			branch, err := other.branches.CreateBranch("round5-delete", "feature", main.ID)
			require.NoError(t, err)

			target.Store(branch.ID)
			deleteErr := slow.branches.DeleteBranchByID(branch.ID)
			t.Logf("DeleteBranchByID held before its read %d returned: %v", read, deleteErr)

			require.GreaterOrEqual(t, reads.Load(), int64(read), "the control: the delete got as far as read %d", read)
			require.NoError(t, otherErr, "the control: the other delete removed the branch")
			require.Error(t, deleteErr)
			assert.ErrorIs(t, deleteErr, branchwal.ErrBranchNotFound)
			assert.Equal(t, "branch not found", deleteErr.Error())
		})
	}
}
