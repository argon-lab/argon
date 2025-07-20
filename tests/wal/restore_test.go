package wal_test

import (
	"context"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	projectwal "github.com/argon-lab/argon/internal/project/wal"
	"github.com/argon-lab/argon/internal/restore"
	"github.com/argon-lab/argon/internal/timetravel"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestRestore_ResetBranchToLSN(t *testing.T) {
	db := setupTestDB(t)
	walService, err := wal.NewService(db)
	require.NoError(t, err)

	branchService, err := branchwal.NewBranchService(db, walService)
	require.NoError(t, err)

	projectService, err := projectwal.NewProjectService(db, walService, branchService)
	require.NoError(t, err)

	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()

	// Create project and get main branch
	project, err := projectService.CreateProject("restore-test")
	require.NoError(t, err)

	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]

	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Reset to previous LSN", func(t *testing.T) {
		// Create some history
		_, err := interceptor.InsertOne(ctx, "items", bson.M{"_id": "i1", "step": 1})
		assert.NoError(t, err)
		lsn1 := walService.GetCurrentLSN()

		_, err = interceptor.InsertOne(ctx, "items", bson.M{"_id": "i2", "step": 2})
		assert.NoError(t, err)
		lsn2 := walService.GetCurrentLSN()

		_, err = interceptor.InsertOne(ctx, "items", bson.M{"_id": "i3", "step": 3})
		assert.NoError(t, err)

		// Update branch to get latest HEAD
		branch, _ = branchService.GetBranchByID(branch.ID)
		originalHead := branch.HeadLSN

		// Reset to LSN2
		resetBranch, err := restoreService.ResetBranchToLSN(branch.ID, lsn2)
		assert.NoError(t, err)
		assert.Equal(t, lsn2, resetBranch.HeadLSN)

		// Verify state after reset
		state, err := materializerService.MaterializeCollection(resetBranch, "items")
		assert.NoError(t, err)
		assert.Len(t, state, 2) // Only i1 and i2
		assert.NotNil(t, state["i1"])
		assert.NotNil(t, state["i2"])
		assert.Nil(t, state["i3"])

		// Reset to even earlier LSN
		resetBranch2, err := restoreService.ResetBranchToLSN(branch.ID, lsn1)
		assert.NoError(t, err)
		assert.Equal(t, lsn1, resetBranch2.HeadLSN)

		// Verify state
		state2, err := materializerService.MaterializeCollection(resetBranch2, "items")
		assert.NoError(t, err)
		assert.Len(t, state2, 1) // Only i1
		assert.NotNil(t, state2["i1"])

		// Try to reset beyond current HEAD (should fail)
		_, err = restoreService.ResetBranchToLSN(branch.ID, originalHead+10)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "beyond branch HEAD")
	})

	t.Run("Reset validation", func(t *testing.T) {
		// Try to reset before base LSN
		_, err := restoreService.ResetBranchToLSN(branch.ID, -1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "before branch base LSN")

		// Validate restore
		err = restoreService.ValidateRestore(branch.ID, branch.BaseLSN)
		assert.NoError(t, err)

		err = restoreService.ValidateRestore(branch.ID, branch.BaseLSN-1)
		assert.Error(t, err)
	})
}

func TestRestore_ResetBranchToTime(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("time-reset-test")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Reset to specific time", func(t *testing.T) {
		// Create timed operations
		startTime := time.Now()

		_, err := interceptor.InsertOne(ctx, "events", bson.M{"_id": "e1", "time": "morning"})
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		midTime := time.Now()

		_, err = interceptor.InsertOne(ctx, "events", bson.M{"_id": "e2", "time": "noon"})
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)

		_, err = interceptor.InsertOne(ctx, "events", bson.M{"_id": "e3", "time": "evening"})
		assert.NoError(t, err)

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Reset to mid time
		resetBranch, err := restoreService.ResetBranchToTime(branch.ID, midTime.Add(25*time.Millisecond))
		assert.NoError(t, err)

		// Verify state
		state, err := materializerService.MaterializeCollection(resetBranch, "events")
		assert.NoError(t, err)
		assert.Len(t, state, 2) // e1 and e2
		assert.NotNil(t, state["e1"])
		assert.NotNil(t, state["e2"])
		assert.Nil(t, state["e3"])

		// Try to reset before any events
		_, err = restoreService.ResetBranchToTime(branch.ID, startTime.Add(-1*time.Second))
		assert.Error(t, err) // Should fail - no entries before this time
	})
}

func TestRestore_CreateBranchAtLSN(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("branch-at-lsn-test")
	branches, _ := branchService.ListBranches(project.ID)
	mainBranch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, mainBranch, branchService)

	t.Run("Create branch from historical point", func(t *testing.T) {
		// Create history on main
		_, err := interceptor.InsertOne(ctx, "docs", bson.M{"_id": "d1", "version": "v1"})
		assert.NoError(t, err)
		checkpoint1 := walService.GetCurrentLSN()

		_, err = interceptor.InsertOne(ctx, "docs", bson.M{"_id": "d2", "version": "v1"})
		assert.NoError(t, err)
		checkpoint2 := walService.GetCurrentLSN()

		_, err = interceptor.UpdateOne(ctx, "docs",
			bson.M{"_id": "d1"},
			bson.M{"$set": bson.M{"version": "v2"}})
		assert.NoError(t, err)

		// Update main branch
		mainBranch, _ = branchService.GetBranchByID(mainBranch.ID)

		// Create branch at checkpoint1
		branch1, err := restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "feature-v1", checkpoint1)
		assert.NoError(t, err)
		assert.Equal(t, checkpoint1, branch1.BaseLSN)
		assert.Equal(t, checkpoint1, branch1.HeadLSN)

		// Verify branch1 state
		state1, err := materializerService.MaterializeCollection(branch1, "docs")
		assert.NoError(t, err)
		assert.Len(t, state1, 1) // Only d1
		assert.Equal(t, "v1", state1["d1"]["version"])

		// Create branch at checkpoint2
		branch2, err := restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "feature-v2", checkpoint2)
		assert.NoError(t, err)
		assert.Equal(t, checkpoint2, branch2.BaseLSN)
		assert.Equal(t, checkpoint2, branch2.HeadLSN)

		// Verify branch2 state
		state2, err := materializerService.MaterializeCollection(branch2, "docs")
		assert.NoError(t, err)
		assert.Len(t, state2, 2)                       // d1 and d2
		assert.Equal(t, "v1", state2["d1"]["version"]) // Still v1
		assert.Equal(t, "v1", state2["d2"]["version"])

		// Main branch should have v2
		mainState, err := materializerService.MaterializeCollection(mainBranch, "docs")
		assert.NoError(t, err)
		assert.Equal(t, "v2", mainState["d1"]["version"])

		// Try invalid LSN
		_, err = restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "invalid", mainBranch.HeadLSN+10)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "outside source branch range")
	})

	t.Run("Branch operations isolation", func(t *testing.T) {
		// Get the created branches
		branch1, _ := branchService.GetBranch(project.ID, "feature-v1")
		branch2, _ := branchService.GetBranch(project.ID, "feature-v2")

		// Add data to branch1
		interceptor1 := driverwal.NewInterceptor(walService, branch1, branchService)
		_, err := interceptor1.InsertOne(ctx, "docs", bson.M{"_id": "d3", "branch": "feature-v1"})
		assert.NoError(t, err)

		// Add data to branch2
		interceptor2 := driverwal.NewInterceptor(walService, branch2, branchService)
		_, err = interceptor2.InsertOne(ctx, "docs", bson.M{"_id": "d3", "branch": "feature-v2"})
		assert.NoError(t, err)

		// Update branches
		branch1, _ = branchService.GetBranchByID(branch1.ID)
		branch2, _ = branchService.GetBranchByID(branch2.ID)

		// Verify isolation
		state1, _ := materializerService.MaterializeCollection(branch1, "docs")
		state2, _ := materializerService.MaterializeCollection(branch2, "docs")

		assert.Equal(t, "feature-v1", state1["d3"]["branch"])
		assert.Equal(t, "feature-v2", state2["d3"]["branch"])
	})
}

func TestRestore_CreateBranchAtTime(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("branch-at-time-test")
	branches, _ := branchService.ListBranches(project.ID)
	mainBranch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, mainBranch, branchService)

	t.Run("Create branch from timestamp", func(t *testing.T) {
		// Create timed history
		_, err := interceptor.InsertOne(ctx, "timeline", bson.M{"_id": "t1", "event": "past"})
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		checkpoint := time.Now()

		_, err = interceptor.InsertOne(ctx, "timeline", bson.M{"_id": "t2", "event": "present"})
		assert.NoError(t, err)

		time.Sleep(50 * time.Millisecond)

		_, err = interceptor.InsertOne(ctx, "timeline", bson.M{"_id": "t3", "event": "future"})
		assert.NoError(t, err)

		// Update main branch
		mainBranch, _ = branchService.GetBranchByID(mainBranch.ID)

		// Create branch at checkpoint
		timeBranch, err := restoreService.CreateBranchAtTime(project.ID, mainBranch.ID, "time-branch", checkpoint.Add(25*time.Millisecond))
		assert.NoError(t, err)

		// Verify state
		state, err := materializerService.MaterializeCollection(timeBranch, "timeline")
		assert.NoError(t, err)
		assert.Len(t, state, 2) // t1 and t2
		assert.NotNil(t, state["t1"])
		assert.NotNil(t, state["t2"])
		assert.Nil(t, state["t3"])
	})
}

func TestRestore_GetRestorePreview(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("preview-test")
	branches, _ := branchService.ListBranches(project.ID)
	branch := branches[0]
	interceptor := driverwal.NewInterceptor(walService, branch, branchService)

	t.Run("Preview restore operation", func(t *testing.T) {
		// Create complex history
		_, err := interceptor.InsertOne(ctx, "users", bson.M{"_id": "u1"})
		assert.NoError(t, err)
		_, err = interceptor.InsertOne(ctx, "products", bson.M{"_id": "p1"})
		assert.NoError(t, err)

		checkpoint := walService.GetCurrentLSN()

		// Operations after checkpoint
		_, err = interceptor.UpdateOne(ctx, "users", bson.M{"_id": "u1"}, bson.M{"$set": bson.M{"active": true}})
		assert.NoError(t, err)
		_, err = interceptor.InsertOne(ctx, "orders", bson.M{"_id": "o1"})
		assert.NoError(t, err)
		_, err = interceptor.InsertOne(ctx, "orders", bson.M{"_id": "o2"})
		assert.NoError(t, err)
		_, err = interceptor.DeleteOne(ctx, "products", bson.M{"_id": "p1"})
		assert.NoError(t, err)

		// Update branch
		branch, _ = branchService.GetBranchByID(branch.ID)

		// Get preview
		preview, err := restoreService.GetRestorePreview(branch.ID, checkpoint)
		assert.NoError(t, err)

		assert.Equal(t, branch.ID, preview.BranchID)
		assert.Equal(t, branch.Name, preview.BranchName)
		assert.Equal(t, branch.HeadLSN, preview.CurrentLSN)
		assert.Equal(t, checkpoint, preview.TargetLSN)
		assert.Equal(t, 4, preview.OperationsToDiscard) // update, 2 inserts, 1 delete

		// Check affected collections
		assert.Equal(t, 3, len(preview.AffectedCollections))
		assert.Equal(t, 1, preview.AffectedCollections["users"])
		assert.Equal(t, 2, preview.AffectedCollections["orders"])
		assert.Equal(t, 1, preview.AffectedCollections["products"])

		// Current collections should include orders
		assert.Contains(t, preview.CurrentCollections, "orders")
		// Target collections should not include orders
		assert.NotContains(t, preview.TargetCollections, "orders")
	})
}

func TestRestore_ComplexScenario(t *testing.T) {
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()

	project, _ := projectService.CreateProject("complex-restore")
	branches, _ := branchService.ListBranches(project.ID)
	mainBranch := branches[0]

	t.Run("Development workflow with restore", func(t *testing.T) {
		interceptor := driverwal.NewInterceptor(walService, mainBranch, branchService)

		// Initial production state
		_, err := interceptor.InsertOne(ctx, "config", bson.M{"_id": "app", "version": "1.0", "features": []string{"basic"}})
		assert.NoError(t, err)
		prodLSN := walService.GetCurrentLSN()

		// Development changes
		_, err = interceptor.UpdateOne(ctx, "config",
			bson.M{"_id": "app"},
			bson.M{"$set": bson.M{"version": "2.0", "features": []string{"basic", "advanced"}}})
		assert.NoError(t, err)

		_, err = interceptor.InsertOne(ctx, "config", bson.M{"_id": "experimental", "enabled": true})
		assert.NoError(t, err)

		// Update main branch
		mainBranch, _ = branchService.GetBranchByID(mainBranch.ID)

		// Something went wrong, need to rollback
		// First, create a backup branch at current state
		backup, err := restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "backup-v2", mainBranch.HeadLSN)
		assert.NoError(t, err)

		// Reset main to production state
		restored, err := restoreService.ResetBranchToLSN(mainBranch.ID, prodLSN)
		assert.NoError(t, err)

		// Verify main is back to v1.0
		mainState, _ := materializerService.MaterializeCollection(restored, "config")
		assert.Equal(t, "1.0", mainState["app"]["version"])
		assert.Nil(t, mainState["experimental"])

		// Backup still has v2.0
		backupState, _ := materializerService.MaterializeCollection(backup, "config")
		assert.Equal(t, "2.0", backupState["app"]["version"])
		assert.NotNil(t, backupState["experimental"])

		// Create a feature branch from production for safe development
		feature, err := restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "feature-v2-safe", prodLSN)
		assert.NoError(t, err)

		// Develop on feature branch
		featureInterceptor := driverwal.NewInterceptor(walService, feature, branchService)
		_, err = featureInterceptor.UpdateOne(ctx, "config",
			bson.M{"_id": "app"},
			bson.M{"$set": bson.M{"version": "2.0-beta"}})
		assert.NoError(t, err)

		// Feature and main are independent
		feature, _ = branchService.GetBranchByID(feature.ID)
		featureState, _ := materializerService.MaterializeCollection(feature, "config")
		assert.Equal(t, "2.0-beta", featureState["app"]["version"])

		mainState2, _ := materializerService.MaterializeCollection(restored, "config")
		assert.Equal(t, "1.0", mainState2["app"]["version"])
	})
}
