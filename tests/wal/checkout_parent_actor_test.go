package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestCheckoutLeavesUncapturedParentActorUnset(t *testing.T) {
	ctx := context.Background()
	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), testDatabaseSeq.Add(1))
	metadataName := "argon_parent_actor_meta_" + id
	open := func() *walcli.Services {
		services, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
		require.NoError(t, err)
		services.SynchronousSnapshots()
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_ = services.Ingest.Shutdown(cleanup)
			_ = services.Client.Disconnect(cleanup)
		})
		return services
	}

	services := open()
	metadata := services.Client.Database(metadataName)
	var branchIDs []string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, branchID := range branchIDs {
			_ = services.Client.Database(checkout.PhysicalDBName(branchID)).Drop(cleanup)
		}
		_ = metadata.Drop(cleanup)
	})

	// Fork before main is checked out, so branching never starts its capture.
	project, err := services.Projects.CreateProject("parent-actor-" + id)
	require.NoError(t, err)
	mainID := project.MainBranchID
	branchIDs = append(branchIDs, mainID)
	require.NoError(t, services.SyncBranch(ctx, mainID))
	child, err := services.Branches.CreateBranch(project.ID, "feature", mainID)
	require.NoError(t, err)
	branchIDs = append(branchIDs, child.ID)

	main, err := services.Checkout.Checkout(ctx, mainID)
	require.NoError(t, err)
	assertNoActor := func() {
		var state bson.M
		require.NoError(t, metadata.Collection("wal_ingest_state").FindOne(ctx, bson.M{"_id": mainID}).Decode(&state))
		require.NotContains(t, state, "actor")
	}
	assertNoActor()
	_, err = services.Client.Database(main.PhysicalDB).Collection("items").InsertOne(ctx, bson.M{"_id": "seed", "n": int32(1)})
	require.NoError(t, err)

	// The child checkout and the labelled watch run in separate CLI processes.
	checkoutServices := open()
	_, err = checkoutServices.Checkout.Checkout(ctx, child.ID)
	require.NoError(t, err)
	assertNoActor()

	watchServices := open()
	capture, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	require.NoError(t, watchServices.StartCapture(capture, mainID, "etl"))
	actor, found, err := watchServices.Ingest.PersistedActor(ctx, mainID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "etl", actor)

	var entry bson.M
	filter := bson.M{"branch_id": mainID, "collection": "items", "document_id": "seed"}
	require.NoError(t, metadata.Collection("wal_log").FindOne(ctx, filter).Decode(&entry))
	require.Equal(t, "etl", entry["actor"], "main's pre-watch writes carry the watch's label")
}
