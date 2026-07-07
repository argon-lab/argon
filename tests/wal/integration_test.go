package wal_test

import (
	"context"
	"fmt"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"github.com/argon-lab/argon/internal/materializer"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestIntegration_Workflow walks the full non-checkout lifecycle: project,
// writes, checkpoints, branch isolation, time travel, disaster restore and
// document history.
func TestIntegration_Workflow(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	require.NoError(t, err)
	mat := materializer.NewService(walService, branchService)
	tt := timetravel.NewService(walService, mat)
	restoreService := restore.NewService(walService, branchService, mat, tt)
	ctx := context.Background()

	project, err := projectService.CreateProject("lifecycle")
	require.NoError(t, err)
	branches, err := branchService.ListBranches(project.ID)
	require.NoError(t, err)
	main := branches[0]
	writer := walwriter.New(walService, branchService, mat, main)

	// Morning: initial state.
	_, err = writer.Put(ctx, "config", bson.M{"_id": "app", "version": "1.0.0", "features": []string{"auth"}})
	require.NoError(t, err)
	_, err = writer.Put(ctx, "users", bson.M{"_id": "admin", "role": "admin"})
	require.NoError(t, err)
	morning := walService.GetCurrentLSN(project.ID)

	// Noon: feature work.
	_, err = writer.Put(ctx, "config", bson.M{"_id": "app", "version": "1.1.0", "features": []string{"auth", "api"}})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err = writer.Put(ctx, "users", bson.M{"_id": fmt.Sprintf("user%d", i), "role": "user"})
		require.NoError(t, err)
	}
	noon := walService.GetCurrentLSN(project.ID)

	// Evening: disaster — admin deleted, config broken.
	_, existed, err := writer.Delete(ctx, "users", "admin")
	require.NoError(t, err)
	require.True(t, existed)
	_, err = writer.Put(ctx, "config", bson.M{"_id": "app", "version": "2.0.0-broken"})
	require.NoError(t, err)

	main, _ = branchService.GetBranchByID(main.ID)

	t.Run("Time travel to checkpoints", func(t *testing.T) {
		morningConfig, err := tt.MaterializeAtLSN(main, "config", morning)
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", morningConfig["app"]["version"])

		noonUsers, err := tt.MaterializeAtLSN(main, "users", noon)
		require.NoError(t, err)
		assert.Len(t, noonUsers, 6, "admin plus five users at noon")
	})

	t.Run("Branch isolation with copy-on-write updates", func(t *testing.T) {
		feature, err := branchService.CreateBranch(project.ID, "feature", main.ID)
		require.NoError(t, err)
		featureWriter := walwriter.New(walService, branchService, mat, feature)

		_, err = featureWriter.Put(ctx, "users", bson.M{"_id": "user0", "role": "superuser"})
		require.NoError(t, err)

		feature, _ = branchService.GetBranchByID(feature.ID)
		featureState, err := mat.MaterializeCollection(feature, "users")
		require.NoError(t, err)
		mainState, err := mat.MaterializeCollection(main, "users")
		require.NoError(t, err)

		assert.Equal(t, "superuser", featureState["user0"]["role"])
		assert.Equal(t, "user", mainState["user0"]["role"], "main unaffected by the feature branch")
	})

	t.Run("Restore from disaster", func(t *testing.T) {
		preview, err := restoreService.GetRestorePreview(main.ID, noon)
		require.NoError(t, err)
		assert.Positive(t, preview.OperationsToDiscard)

		restored, err := restoreService.ResetBranchToLSN(main.ID, noon)
		require.NoError(t, err)

		users, err := mat.MaterializeCollection(restored, "users")
		require.NoError(t, err)
		assert.Contains(t, users, "admin", "the deleted admin is back")
		config, err := mat.MaterializeCollection(restored, "config")
		require.NoError(t, err)
		assert.Equal(t, "1.1.0", config["app"]["version"], "the broken config is gone")
	})

	t.Run("Document history", func(t *testing.T) {
		main, _ = branchService.GetBranchByID(main.ID)
		history, err := walService.GetDocumentHistory(main.ID, "config", "app", 0, walService.GetCurrentLSN(project.ID))
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(history), 3, "all config revisions recorded")
		assert.Equal(t, wal.OpPut, history[0].Operation)
	})
}
