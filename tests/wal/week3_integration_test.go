package wal_test

import (
	"context"
	"fmt"
	"sync"
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
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWeek3_IntegrationTests(t *testing.T) {
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
	
	t.Run("End-to-end time travel and restore", func(t *testing.T) {
		project, _ := projectService.CreateProject("e2e-test")
		branches, _ := branchService.ListBranches(project.ID)
		mainBranch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, mainBranch, branchService)
		
		// Simulate a day of operations
		checkpoints := make(map[string]int64)
		timestamps := make(map[string]time.Time)
		
		// Morning: Initial setup
		timestamps["morning"] = time.Now()
		_, err := interceptor.InsertOne(ctx, "config", bson.M{
			"_id": "app",
			"version": "1.0.0",
			"features": []string{"auth", "api"},
		})
		assert.NoError(t, err)
		
		_, err = interceptor.InsertOne(ctx, "users", bson.M{
			"_id": "admin",
			"role": "admin",
			"created": timestamps["morning"],
		})
		assert.NoError(t, err)
		checkpoints["morning"] = walService.GetCurrentLSN()
		
		// Noon: Feature development
		time.Sleep(10 * time.Millisecond)
		timestamps["noon"] = time.Now()
		
		// Add new feature
		_, err = interceptor.UpdateOne(ctx, "config",
			bson.M{"_id": "app"},
			bson.M{"$set": bson.M{
				"version": "1.1.0",
				"features": []string{"auth", "api", "newfeature"},
			}})
		assert.NoError(t, err)
		
		// Add test users
		for i := 0; i < 5; i++ {
			_, err = interceptor.InsertOne(ctx, "users", bson.M{
				"_id": fmt.Sprintf("user%d", i),
				"role": "user",
				"testAccount": true,
			})
			assert.NoError(t, err)
		}
		checkpoints["noon"] = walService.GetCurrentLSN()
		
		// Evening: Something went wrong
		time.Sleep(10 * time.Millisecond)
		timestamps["evening"] = time.Now()
		
		// Accidental deletion
		_, err = interceptor.DeleteOne(ctx, "users", bson.M{"role": "admin"})
		assert.NoError(t, err)
		
		// Bad config update
		_, err = interceptor.UpdateOne(ctx, "config",
			bson.M{"_id": "app"},
			bson.M{"$set": bson.M{"version": "2.0.0-broken"}})
		assert.NoError(t, err)
		checkpoints["evening"] = walService.GetCurrentLSN()
		
		// Update branch
		mainBranch, _ = branchService.GetBranchByID(mainBranch.ID)
		
		// Time travel verification
		t.Run("Verify time travel queries", func(t *testing.T) {
			// Query morning state
			morningState, err := timeTravelService.MaterializeAtLSN(mainBranch, "config", checkpoints["morning"])
			assert.NoError(t, err)
			assert.Equal(t, "1.0.0", morningState["app"]["version"])
			
			// Query noon state
			noonUsers, err := timeTravelService.MaterializeAtLSN(mainBranch, "users", checkpoints["noon"])
			assert.NoError(t, err)
			assert.Len(t, noonUsers, 6) // admin + 5 test users
			
			// Query by time
			noonState, err := timeTravelService.MaterializeAtTime(mainBranch, "config", timestamps["noon"].Add(5*time.Millisecond))
			assert.NoError(t, err)
			assert.Equal(t, "1.1.0", noonState["app"]["version"])
		})
		
		// Restore operations
		t.Run("Restore from disaster", func(t *testing.T) {
			// Preview what we're about to do
			preview, err := restoreService.GetRestorePreview(mainBranch.ID, checkpoints["noon"])
			assert.NoError(t, err)
			assert.Greater(t, preview.OperationsToDiscard, 0)
			
			// Create backup of current broken state
			_, err = restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "backup-broken", mainBranch.HeadLSN)
			assert.NoError(t, err)
			
			// Restore to noon (before problems)
			restored, err := restoreService.ResetBranchToLSN(mainBranch.ID, checkpoints["noon"])
			assert.NoError(t, err)
			assert.Equal(t, checkpoints["noon"], restored.HeadLSN)
			
			// Verify restoration
			configState, _ := materializerService.MaterializeCollection(restored, "config")
			assert.Equal(t, "1.1.0", configState["app"]["version"])
			
			userState, _ := materializerService.MaterializeCollection(restored, "users")
			assert.Len(t, userState, 6)
			assert.NotNil(t, userState["admin"]) // Admin is back!
		})
		
		// Branch from history
		t.Run("Create feature branch from stable point", func(t *testing.T) {
			// Create branch from morning (stable v1.0.0)
			featureBranch, err := restoreService.CreateBranchAtLSN(project.ID, mainBranch.ID, "feature-x", checkpoints["morning"])
			assert.NoError(t, err)
			
			// Verify branch sees morning state
			featureConfig, _ := materializerService.MaterializeCollection(featureBranch, "config")
			assert.Equal(t, "1.0.0", featureConfig["app"]["version"])
			
			// Develop on feature branch
			featureInterceptor := driverwal.NewInterceptor(walService, featureBranch, branchService)
			_, err = featureInterceptor.UpdateOne(ctx, "config",
				bson.M{"_id": "app"},
				bson.M{"$set": bson.M{"version": "1.0.1-feature-x"}})
			assert.NoError(t, err)
			
			// Branches are isolated
			featureBranch, _ = branchService.GetBranchByID(featureBranch.ID)
			mainBranch, _ = branchService.GetBranchByID(mainBranch.ID)
			
			mainConfig, _ := materializerService.MaterializeCollection(mainBranch, "config")
			featureConfig2, _ := materializerService.MaterializeCollection(featureBranch, "config")
			
			assert.NotEqual(t, mainConfig["app"]["version"], featureConfig2["app"]["version"])
		})
	})
	
	t.Run("Edge cases and error handling", func(t *testing.T) {
		project, _ := projectService.CreateProject("edge-cases")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		
		// Empty branch time travel
		t.Run("Time travel on empty branch", func(t *testing.T) {
			state, err := timeTravelService.MaterializeAtLSN(branch, "empty", branch.HeadLSN)
			assert.NoError(t, err)
			assert.Empty(t, state)
			
			info, err := timeTravelService.GetTimeTravelInfo(branch)
			assert.NoError(t, err)
			assert.Equal(t, 0, info.EntryCount)
		})
		
		// Invalid operations
		t.Run("Invalid restore operations", func(t *testing.T) {
			// Reset to future
			_, err := restoreService.ResetBranchToLSN(branch.ID, branch.HeadLSN+100)
			assert.Error(t, err)
			
			// Reset to negative
			_, err = restoreService.ResetBranchToLSN(branch.ID, -1)
			assert.Error(t, err)
			
			// Create branch with existing name
			interceptor := driverwal.NewInterceptor(walService, branch, branchService)
			_, _ = interceptor.InsertOne(ctx, "test", bson.M{"_id": "1"})
			branch, _ = branchService.GetBranchByID(branch.ID)
			
			_, err = restoreService.CreateBranchAtLSN(project.ID, branch.ID, "main", branch.HeadLSN)
			assert.Error(t, err) // Duplicate name
		})
		
		// Time-based operations edge cases
		t.Run("Time-based restore edge cases", func(t *testing.T) {
			// Reset to time before any operations
			_, err := restoreService.ResetBranchToTime(branch.ID, time.Now().Add(-24*time.Hour))
			assert.Error(t, err)
			
			// Create branch from future time
			_, err = restoreService.CreateBranchAtTime(project.ID, branch.ID, "future", time.Now().Add(1*time.Hour))
			assert.Error(t, err)
		})
	})
	
	t.Run("Concurrent operations with time travel", func(t *testing.T) {
		project, _ := projectService.CreateProject("concurrent-tt")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)
		
		// Create initial data
		for i := 0; i < 100; i++ {
			_, _ = interceptor.InsertOne(ctx, "data", bson.M{"_id": fmt.Sprintf("doc%d", i), "value": i})
		}
		
		branch, _ = branchService.GetBranchByID(branch.ID)
		midPoint := branch.HeadLSN
		
		// Add more data
		for i := 100; i < 200; i++ {
			_, _ = interceptor.InsertOne(ctx, "data", bson.M{"_id": fmt.Sprintf("doc%d", i), "value": i})
		}
		
		branch, _ = branchService.GetBranchByID(branch.ID)
		
		// Concurrent time travel queries
		var wg sync.WaitGroup
		errors := make(chan error, 20)
		
		// 10 goroutines querying different points
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(routineID int) {
				defer wg.Done()
				
				// Query at midpoint
				state1, err := timeTravelService.MaterializeAtLSN(branch, "data", midPoint)
				if err != nil {
					errors <- err
					return
				}
				if len(state1) != 100 {
					errors <- fmt.Errorf("routine %d: expected 100 docs at midpoint, got %d", routineID, len(state1))
					return
				}
				
				// Query at HEAD
				state2, err := timeTravelService.MaterializeAtLSN(branch, "data", branch.HeadLSN)
				if err != nil {
					errors <- err
					return
				}
				if len(state2) != 200 {
					errors <- fmt.Errorf("routine %d: expected 200 docs at HEAD, got %d", routineID, len(state2))
					return
				}
			}(i)
		}
		
		// 10 goroutines creating branches
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(routineID int) {
				defer wg.Done()
				
				branchName := fmt.Sprintf("concurrent-branch-%d", routineID)
				newBranch, err := restoreService.CreateBranchAtLSN(project.ID, branch.ID, branchName, midPoint)
				if err != nil {
					errors <- err
					return
				}
				
				// Verify branch state
				state, err := materializerService.MaterializeCollection(newBranch, "data")
				if err != nil {
					errors <- err
					return
				}
				if len(state) != 100 {
					errors <- fmt.Errorf("branch %s: expected 100 docs, got %d", branchName, len(state))
				}
			}(i)
		}
		
		wg.Wait()
		close(errors)
		
		// Check for errors
		for err := range errors {
			t.Errorf("Concurrent operation error: %v", err)
		}
	})
	
	t.Run("Complex nested document operations", func(t *testing.T) {
		project, _ := projectService.CreateProject("nested-docs")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)
		
		// Create complex document
		doc := bson.M{
			"_id": "complex1",
			"metadata": bson.M{
				"version": 1,
				"tags": []string{"important", "v1"},
			},
			"data": bson.M{
				"nested": bson.M{
					"deeply": bson.M{
						"value": 100,
					},
				},
			},
		}
		
		_, err := interceptor.InsertOne(ctx, "complex", doc)
		assert.NoError(t, err)
		checkpoint1 := walService.GetCurrentLSN()
		
		// Update nested fields
		_, err = interceptor.UpdateOne(ctx, "complex",
			bson.M{"_id": "complex1"},
			bson.M{
				"$set": bson.M{
					"metadata.version": 2,
					"data.nested.deeply.value": 200,
				},
				"$push": bson.M{
					"metadata.tags": "v2",
				},
			})
		assert.NoError(t, err)
		
		branch, _ = branchService.GetBranchByID(branch.ID)
		
		// Time travel verification
		oldState, _ := timeTravelService.MaterializeAtLSN(branch, "complex", checkpoint1)
		newState, _ := timeTravelService.MaterializeAtLSN(branch, "complex", branch.HeadLSN)
		
		// Verify nested values
		oldMeta := oldState["complex1"]["metadata"].(bson.M)
		newMeta := newState["complex1"]["metadata"].(bson.M)
		
		assert.Equal(t, int32(1), oldMeta["version"])
		assert.Equal(t, int32(2), newMeta["version"])
		
		// Create branch and modify
		featureBranch, _ := restoreService.CreateBranchAtLSN(project.ID, branch.ID, "nested-feature", checkpoint1)
		featureInt := driverwal.NewInterceptor(walService, featureBranch, branchService)
		
		_, err = featureInt.UpdateOne(ctx, "complex",
			bson.M{"_id": "complex1"},
			bson.M{"$set": bson.M{"metadata.version": 99}})
		assert.NoError(t, err)
		
		// Verify isolation
		featureBranch, _ = branchService.GetBranchByID(featureBranch.ID)
		featureState, _ := materializerService.MaterializeCollection(featureBranch, "complex")
		mainState, _ := materializerService.MaterializeCollection(branch, "complex")
		
		featureMeta := featureState["complex1"]["metadata"].(bson.M)
		mainMeta := mainState["complex1"]["metadata"].(bson.M)
		
		assert.Equal(t, int32(99), featureMeta["version"])
		assert.Equal(t, int32(2), mainMeta["version"])
	})
	
	t.Run("Large scale time travel", func(t *testing.T) {
		project, _ := projectService.CreateProject("large-scale")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		interceptor := driverwal.NewInterceptor(walService, branch, branchService)
		
		// Create checkpoints
		checkpoints := []int64{}
		
		// Insert 1000 documents with checkpoints every 100
		for i := 0; i < 1000; i++ {
			_, err := interceptor.InsertOne(ctx, "items", bson.M{
				"_id": primitive.NewObjectID(),
				"index": i,
				"batch": i / 100,
				"data": fmt.Sprintf("Item number %d in batch %d", i, i/100),
			})
			assert.NoError(t, err)
			
			if i%100 == 99 {
				checkpoints = append(checkpoints, walService.GetCurrentLSN())
			}
		}
		
		branch, _ = branchService.GetBranchByID(branch.ID)
		
		// Verify each checkpoint
		for i, checkpoint := range checkpoints {
			expectedCount := (i + 1) * 100
			state, err := timeTravelService.MaterializeAtLSN(branch, "items", checkpoint)
			assert.NoError(t, err)
			assert.Len(t, state, expectedCount, "Checkpoint %d should have %d items", i, expectedCount)
		}
		
		// Performance check
		start := time.Now()
		_, err = timeTravelService.MaterializeAtLSN(branch, "items", branch.HeadLSN)
		elapsed := time.Since(start)
		assert.NoError(t, err)
		assert.Less(t, elapsed, 500*time.Millisecond, "Materializing 1000 docs should be fast")
		
		// Create multiple branches at different points
		for i := 0; i < 5; i++ {
			branchName := fmt.Sprintf("scale-branch-%d", i)
			checkpoint := checkpoints[i]
			
			newBranch, err := restoreService.CreateBranchAtLSN(project.ID, branch.ID, branchName, checkpoint)
			assert.NoError(t, err)
			
			state, _ := materializerService.MaterializeCollection(newBranch, "items")
			assert.Len(t, state, (i+1)*100)
		}
	})
}

func TestWeek3_StressTest(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}
	
	db := setupTestDB(t)
	walService, _ := wal.NewService(db)
	branchService, _ := branchwal.NewBranchService(db, walService)
	projectService, _ := projectwal.NewProjectService(db, walService, branchService)
	materializerService := materializer.NewService(walService)
	timeTravelService := timetravel.NewService(walService, materializerService)
	restoreService := restore.NewService(walService, branchService, materializerService, timeTravelService)
	ctx := context.Background()
	
	t.Run("Stress test time travel under load", func(t *testing.T) {
		project, _ := projectService.CreateProject("stress-test")
		branches, _ := branchService.ListBranches(project.ID)
		branch := branches[0]
		
		// Multiple writers
		var wg sync.WaitGroup
		numWriters := 5
		opsPerWriter := 200
		
		for w := 0; w < numWriters; w++ {
			wg.Add(1)
			go func(writerID int) {
				defer wg.Done()
				interceptor := driverwal.NewInterceptor(walService, branch, branchService)
				
				for i := 0; i < opsPerWriter; i++ {
					doc := bson.M{
						"_id": fmt.Sprintf("w%d-doc%d", writerID, i),
						"writer": writerID,
						"seq": i,
						"timestamp": time.Now(),
					}
					_, _ = interceptor.InsertOne(ctx, "stress", doc)
					
					if i%50 == 0 {
						// Occasional updates
						_, _ = interceptor.UpdateOne(ctx, "stress",
							bson.M{"_id": fmt.Sprintf("w%d-doc%d", writerID, i-10)},
							bson.M{"$set": bson.M{"updated": true}})
					}
				}
			}(w)
		}
		
		// Concurrent readers doing time travel
		numReaders := 10
		for r := 0; r < numReaders; r++ {
			wg.Add(1)
			go func(readerID int) {
				defer wg.Done()
				time.Sleep(50 * time.Millisecond) // Let some writes happen
				
				for i := 0; i < 20; i++ {
					branch, _ := branchService.GetBranchByID(branch.ID)
					
					// Random LSN between 0 and current
					targetLSN := branch.HeadLSN / 2
					if targetLSN > 0 {
						_, err := timeTravelService.MaterializeAtLSN(branch, "stress", targetLSN)
						assert.NoError(t, err)
					}
					
					time.Sleep(10 * time.Millisecond)
				}
			}(r)
		}
		
		// Branch creators
		for b := 0; b < 3; b++ {
			wg.Add(1)
			go func(branchID int) {
				defer wg.Done()
				time.Sleep(100 * time.Millisecond) // Let some writes happen
				
				branch, _ := branchService.GetBranchByID(branch.ID)
				midPoint := branch.HeadLSN / 2
				
				if midPoint > 0 {
					branchName := fmt.Sprintf("stress-branch-%d", branchID)
					_, err := restoreService.CreateBranchAtLSN(project.ID, branch.ID, branchName, midPoint)
					// Ignore duplicate name errors
					if err != nil && !assert.Contains(t, err.Error(), "already exists") {
						t.Errorf("Branch creation error: %v", err)
					}
				}
			}(b)
		}
		
		wg.Wait()
		
		// Final verification
		branch, _ = branchService.GetBranchByID(branch.ID)
		finalState, err := materializerService.MaterializeCollection(branch, "stress")
		assert.NoError(t, err)
		
		// Should have many documents
		assert.Greater(t, len(finalState), numWriters*opsPerWriter/2) // At least half (accounting for updates)
	})
}