package wal_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// createBranchEntries counts the create_branch records the project's WAL
// holds for a branch name.
func createBranchEntries(t *testing.T, db *mongo.Database, projectID, name string) int64 {
	t.Helper()
	count, err := db.Collection("wal_log").CountDocuments(context.Background(), bson.M{
		"project_id": projectID, "operation": wal.OpCreateBranch, "metadata.branch_name": name,
	})
	require.NoError(t, err)
	return count
}

// Creates of one name that arrive together (two agents, or one client that
// retried): one makes the branch, and every other is told it exists, as it
// would have been a moment later. None leaves a create_branch record for a
// branch that was never made.
func TestBranch_ConcurrentCreatesOfOneNameMakeOneBranch(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "concurrent-create"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	for round := 0; round < 5; round++ {
		name := "run-" + string(rune('a'+round))
		const callers = 12
		var wg sync.WaitGroup
		errs := make([]error, callers)
		start := make(chan struct{})
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.branches.CreateBranch(project, name, main.ID)
			}(i)
		}
		close(start)
		wg.Wait()

		created := 0
		for _, err := range errs {
			if err == nil {
				created++
				continue
			}
			assert.Truef(t, errors.Is(err, branchwal.ErrBranchExists), "%s: a losing create must say the branch exists, got: %v", name, err)
		}
		assert.Equal(t, 1, created, "%s: exactly one create succeeds", name)
		assert.EqualValues(t, 1, createBranchEntries(t, db, project, name), "%s: one create_branch record", name)
		branch, err := f.branches.GetBranch(project, name)
		require.NoError(t, err)
		entry, err := f.wal.GetEntry(project, branch.CreatedLSN)
		require.NoError(t, err)
		assert.Equal(t, branch.ID, entry.BranchID, "%s: the record is the surviving branch's", name)
	}
}

// A name is free again once its branch is deleted: an agent that discards
// "run-1" and starts over under the same name gets a new branch.
func TestBranch_NameCanBeReusedAfterDelete(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "reuse-name"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	first, err := f.branches.CreateBranch(project, "run-1", main.ID)
	require.NoError(t, err)
	require.NoError(t, f.branches.DeleteBranch(project, "run-1"))
	second, err := f.branches.CreateBranch(project, "run-1", main.ID)
	require.NoError(t, err, "the name of a deleted branch can be used again")
	assert.NotEqual(t, first.ID, second.ID)

	// The deleted branch stays readable by ID for audit.
	old, err := f.branches.GetBranchByIDAny(first.ID)
	require.NoError(t, err)
	assert.True(t, old.IsDeleted)
	// And the name still refuses a second live branch.
	_, err = f.branches.CreateBranch(project, "run-1", main.ID)
	assert.ErrorIs(t, err, branchwal.ErrBranchExists)
}

// Creates of different names that arrive together all succeed.
func TestBranch_ConcurrentCreatesOfDifferentNamesAllSucceed(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "concurrent-distinct"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	const callers = 16
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.branches.CreateBranch(project, fmt.Sprintf("run-%d", i), main.ID)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "run-%d", i)
	}
	branches, err := f.branches.ListBranches(project)
	require.NoError(t, err)
	assert.Len(t, branches, callers+1)
}

// legacyBranchIndex creates the name index of releases up to 2.12.0.
func legacyBranchIndex(t *testing.T, db *mongo.Database) {
	t.Helper()
	_, err := db.Collection("wal_branches").Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	require.NoError(t, err)
}

func branchIndexNames(t *testing.T, db *mongo.Database) []string {
	t.Helper()
	specs, err := db.Collection("wal_branches").Indexes().ListSpecifications(context.Background())
	require.NoError(t, err)
	var names []string
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// A metadata database written by an earlier release has the legacy index
// and may hold deleted branches. Opening it retires that index, live names
// stay unique, and a deleted branch's name becomes usable.
func TestBranch_LegacyNameIndexIsRetiredOnOpen(t *testing.T) {
	db := setupTestDB(t)
	legacyBranchIndex(t, db)
	_, err := db.Collection("wal_branches").InsertMany(context.Background(), []interface{}{
		bson.M{"_id": "old-main", "project_id": "legacy", "name": "main", "is_deleted": false},
		bson.M{"_id": "old-run", "project_id": "legacy", "name": "run-1", "is_deleted": true},
	})
	require.NoError(t, err)

	f := newSnapshotFixture(t, db)
	names := branchIndexNames(t, db)
	assert.NotContains(t, names, "project_id_1_name_1")
	assert.Contains(t, names, "project_id_1_name_1_live")

	_, err = f.branches.CreateBranch("legacy", "run-1", "old-main")
	require.NoError(t, err, "the deleted branch's name is free")
	_, err = f.branches.CreateBranch("legacy", "main", "")
	assert.ErrorIs(t, err, branchwal.ErrBranchExists)
	// Opening again, as every command does, changes nothing.
	_, err = branchwal.NewBranchService(db, f.wal)
	require.NoError(t, err)
	assert.ElementsMatch(t, names, branchIndexNames(t, db))
}

// An older release that connects after the upgrade creates the legacy index
// again. A create that runs into it retires it and goes through.
func TestBranch_CreateRetiresALegacyIndexAnOlderReleasePutBack(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "mixed-versions"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	_, err = f.branches.CreateBranch(project, "run-1", main.ID)
	require.NoError(t, err)
	require.NoError(t, f.branches.DeleteBranch(project, "run-1"))
	legacyBranchIndex(t, db) // the older release starts

	_, err = f.branches.CreateBranch(project, "run-1", main.ID)
	require.NoError(t, err)
	assert.NotContains(t, branchIndexNames(t, db), "project_id_1_name_1")
	assert.EqualValues(t, 2, createBranchEntries(t, db, project, "run-1"), "one record per branch that was made")
	_, err = f.branches.CreateBranch(project, "run-1", main.ID)
	assert.ErrorIs(t, err, branchwal.ErrBranchExists)
}

// CreateBranchWithData (restore's branch from a point in time) follows the
// same rules.
func TestBranch_CreateWithDataRefusesANameInUseAndLeavesNoRecord(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "with-data"
	_, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.branches.CreateBranchWithData(&wal.Branch{
				ID: primitive.NewObjectID().Hex(), ProjectID: project, Name: "restored", HeadLSN: 1, BaseLSN: 1,
			})
		}(i)
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		if err == nil {
			created++
			continue
		}
		assert.ErrorIs(t, err, branchwal.ErrBranchExists)
	}
	assert.Equal(t, 1, created)
	assert.EqualValues(t, 1, createBranchEntries(t, db, project, "restored"))
}
