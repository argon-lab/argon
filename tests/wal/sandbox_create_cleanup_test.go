package wal_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A sandbox create or adopt that fails at checkout leaves nothing behind: no
// live branch and no physical database. (Like any deleted branch, the record
// keeps its name.)
func TestSandboxThatFailsAtCheckoutLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), testDatabaseSeq.Add(1))
	metadataName := "argon_sandbox_cleanup_meta_" + id
	services, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
	require.NoError(t, err)
	metadata := services.Client.Database(metadataName)
	branches := metadata.Collection("wal_branches")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, status := range services.Ingest.Statuses() {
			_ = services.Ingest.Cancel(cleanup, status.BranchID)
		}
		ids, _ := branches.Distinct(cleanup, "_id", bson.M{})
		for _, branchID := range ids {
			_ = services.Client.Database(checkout.PhysicalDBName(branchID.(string))).Drop(cleanup)
		}
		_ = metadata.Drop(cleanup)
		_ = services.Client.Disconnect(cleanup)
	})

	project, err := services.Projects.CreateProject("cleanup")
	require.NoError(t, err)
	writer, err := services.WriterFor("cleanup", "main")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = writer.Put(ctx, "tickets", bson.M{"_id": fmt.Sprintf("T-%d", i)})
		require.NoError(t, err)
	}
	main, err := services.Branches.GetBranch(project.ID, "main")
	require.NoError(t, err)

	// databasesOf lists the physical databases of every record named name,
	// deleted ones included.
	databasesOf := func(t *testing.T, name string) []string {
		t.Helper()
		ids, err := branches.Distinct(ctx, "_id", bson.M{"project_id": project.ID, "name": name})
		require.NoError(t, err)
		require.NotEmpty(t, ids, "no record of branch %s", name)
		existing, err := services.Client.ListDatabaseNames(ctx, bson.M{})
		require.NoError(t, err)
		var left []string
		for _, branchID := range ids {
			if db := checkout.PhysicalDBName(branchID.(string)); slices.Contains(existing, db) {
				left = append(left, db)
			}
		}
		return left
	}
	requireGone := func(t *testing.T, name string) {
		t.Helper()
		_, err := services.Branches.GetBranch(project.ID, name)
		require.Error(t, err, "branch %s is still there", name)
		require.Empty(t, databasesOf(t, name), "physical database of %s is still there", name)
	}
	// changeDuringLoad moves the new branch's record on as checkout starts to
	// copy its data, so checkout fails after the copy, when it marks the
	// branch live.
	changeDuringLoad := func(name string) {
		services.Checkout.SetDuringLoad(func(context.Context, string) func(context.Context) {
			_, err := branches.UpdateOne(context.Background(),
				bson.M{"project_id": project.ID, "name": name, "is_deleted": false},
				bson.M{"$inc": bson.M{"mutation_version": 1}})
			require.NoError(t, err)
			return nil
		})
	}
	// cancelDuringLoad cancels the caller's context as the copy starts.
	cancelDuringLoad := func(cancel context.CancelFunc) {
		services.Checkout.SetDuringLoad(func(context.Context, string) func(context.Context) {
			cancel()
			return nil
		})
	}
	t.Cleanup(func() { services.Checkout.SetDuringLoad(nil) })

	t.Run("create fails after the copy", func(t *testing.T) {
		changeDuringLoad("copied")
		_, err := services.Sandbox.Create(ctx, project.ID, main.ID, "copied", time.Hour)
		require.ErrorContains(t, err, "failed to check the sandbox out")
		requireGone(t, "copied")

		services.Checkout.SetDuringLoad(nil)
		info, err := services.Sandbox.Create(ctx, project.ID, main.ID, "retried", time.Hour)
		require.NoError(t, err, "a later create works")
		count, err := services.Client.Database(info.PhysicalDB).Collection("tickets").CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		require.EqualValues(t, 3, count)
		require.NoError(t, services.Sandbox.Discard(ctx, info.BranchID))
	})

	t.Run("create's context is cancelled during the copy", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		cancelDuringLoad(cancel)
		_, err := services.Sandbox.Create(cancelled, project.ID, main.ID, "cancelled", time.Hour)
		require.ErrorIs(t, err, context.Canceled)
		requireGone(t, "cancelled")
	})

	t.Run("adopt fails after the copy", func(t *testing.T) {
		branch, err := services.Branches.CreateBranch(project.ID, "adopted", main.ID)
		require.NoError(t, err)
		changeDuringLoad("adopted")
		_, err = services.Sandbox.Adopt(ctx, branch.ID, time.Hour)
		require.ErrorContains(t, err, "failed to check the sandbox out")
		requireGone(t, "adopted")
	})
}
