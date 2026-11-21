package wal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// ReadMetrics counts what the metadata database holds, so a fresh process
// (every CLI invocation) reports the same figures as the one that wrote them.
func TestReadMetricsCountsStoredState(t *testing.T) {
	ctx := context.Background()
	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), testDatabaseSeq.Add(1))
	metadataName := "argon_metrics_meta_" + id
	services, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
	require.NoError(t, err)
	metadata := services.Client.Database(metadataName)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, status := range services.Ingest.Statuses() {
			_ = services.Ingest.Cancel(cleanup, status.BranchID)
		}
		ids, _ := metadata.Collection("wal_branches").Distinct(cleanup, "_id", bson.M{})
		for _, branchID := range ids {
			_ = services.Client.Database(checkout.PhysicalDBName(branchID.(string))).Drop(cleanup)
		}
		_ = metadata.Drop(cleanup)
		_ = services.Client.Disconnect(cleanup)
	})

	empty, err := services.ReadMetrics(ctx)
	require.NoError(t, err)
	encoded, err := json.Marshal(empty)
	require.NoError(t, err)
	require.JSONEq(t, `{"projects":[],"branches":0,"sandboxes":0,"checked_out":0}`, string(encoded),
		"no projects is an empty list, not null")

	busy, err := services.Projects.CreateProject("busy")
	require.NoError(t, err)
	writer, err := services.WriterFor("busy", "main")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = writer.Put(ctx, "tickets", bson.M{"_id": fmt.Sprintf("T-%d", i)})
		require.NoError(t, err)
	}
	main, err := services.Branches.GetBranch(busy.ID, "main")
	require.NoError(t, err)
	_, err = services.Branches.CreateBranch(busy.ID, "plain", main.ID)
	require.NoError(t, err)
	gone, err := services.Branches.CreateBranch(busy.ID, "gone", main.ID)
	require.NoError(t, err)
	require.NoError(t, services.Branches.DeleteBranch(busy.ID, gone.Name))
	box, err := services.Sandbox.Create(ctx, busy.ID, main.ID, "box", time.Hour)
	require.NoError(t, err)
	quiet, err := services.Projects.CreateProject("quiet")
	require.NoError(t, err)

	lsn, err := services.WAL.CurrentLSN(busy.ID)
	require.NoError(t, err)
	quietLSN, err := services.WAL.CurrentLSN(quiet.ID)
	require.NoError(t, err)
	require.Greater(t, lsn, quietLSN, "three writes allocate more LSNs than creating a project")

	// A second set of services is a fresh process's view.
	fresh, err := walcli.NewServicesAt(metadataDeploymentURI, metadataName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Client.Disconnect(context.Background()) })
	got, err := fresh.ReadMetrics(ctx)
	require.NoError(t, err)

	byName := map[string]walcli.ProjectMetrics{}
	for _, row := range got.Projects {
		byName[row.Name] = row
	}
	require.Len(t, byName, 2)
	require.Equal(t, walcli.ProjectMetrics{
		Name: "busy", ID: busy.ID,
		Branches: 3, Sandboxes: 1, CheckedOut: 1, CurrentLSN: lsn,
	}, byName["busy"], "main, plain and the sandbox; the deleted branch is not counted")
	require.Equal(t, walcli.ProjectMetrics{
		Name: "quiet", ID: quiet.ID, Branches: 1, CurrentLSN: quietLSN,
	}, byName["quiet"], "a project with no writes has only main")
	require.Equal(t, 4, got.Branches)
	require.Equal(t, 1, got.Sandboxes)
	require.Equal(t, 1, got.CheckedOut)

	require.NoError(t, services.Sandbox.Discard(ctx, box.BranchID))
	after, err := fresh.ReadMetrics(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, after.Branches, "a discarded sandbox is no longer counted")
	require.Zero(t, after.Sandboxes)
	require.Zero(t, after.CheckedOut)
}
