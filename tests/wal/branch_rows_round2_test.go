package wal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Round 2, rows of the case table that no kept test reached. These pass on
// 88d08c9; they are here so the rows stay covered.

// DeleteBranchByID has the guards of DeleteBranch and deletes exactly the
// branch it is given, whatever has become of the name.
func TestBranchRound2_DeleteByIDRows(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "round2-delete-by-id"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	var hooked []string
	f.branches.SetDeleteHook(func(id string) { hooked = append(hooked, id) })
	deleteRecords := func(id string) int64 {
		n, err := db.Collection("wal_log").CountDocuments(ctx, bson.M{"project_id": project, "operation": wal.OpDeleteBranch, "branch_id": id})
		require.NoError(t, err)
		return n
	}

	// main is refused.
	assert.Error(t, f.branches.DeleteBranchByID(main.ID))
	// An ID no branch has.
	assert.Error(t, f.branches.DeleteBranchByID("no-such-branch"))
	assert.Error(t, f.branches.DeleteBranchByID(""))

	// A branch with a live child is refused; without one it goes.
	parent, err := f.branches.CreateBranch(project, "parent", main.ID)
	require.NoError(t, err)
	child, err := f.branches.CreateBranch(project, "child", parent.ID)
	require.NoError(t, err)
	assert.Error(t, f.branches.DeleteBranchByID(parent.ID))
	require.NoError(t, f.branches.DeleteBranchByID(child.ID))

	// The delete guard (a pin) refuses, and nothing is recorded.
	refuse := errors.New("pinned")
	f.branches.SetDeleteGuard(func(id string) error {
		if id == parent.ID {
			return refuse
		}
		return nil
	})
	assert.ErrorIs(t, f.branches.DeleteBranchByID(parent.ID), refuse)
	assert.Zero(t, deleteRecords(parent.ID))
	stillThere, err := f.branches.GetBranchByID(parent.ID)
	require.NoError(t, err)
	assert.False(t, stillThere.IsDeleted)
	f.branches.SetDeleteGuard(nil)
	require.NoError(t, f.branches.DeleteBranchByID(parent.ID))
	assert.Equal(t, []string{child.ID, parent.ID}, hooked, "the delete hook runs once for each deleted branch, with its ID")

	// The name is taken again. Deleting the old ID again is an error and
	// leaves the new branch, and the log, alone.
	again, err := f.branches.CreateBranch(project, "parent", main.ID)
	require.NoError(t, err)
	assert.Error(t, f.branches.DeleteBranchByID(parent.ID))
	assert.EqualValues(t, 1, deleteRecords(parent.ID))
	live, err := f.branches.GetBranch(project, "parent")
	require.NoError(t, err)
	assert.Equal(t, again.ID, live.ID)
	assert.Len(t, hooked, 2)
}

// CreateBranchWithData gives a branch without an ID one, as CreateBranch
// does, and sets CreatedLSN itself whatever the caller put there.
func TestBranchRound2_CreateWithDataEmptyIDAndCallersCreatedLSN(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "round2-with-data"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	for _, name := range []string{"first", "second"} {
		branch := &wal.Branch{ProjectID: project, Name: name, ParentID: main.ID, BaseLSN: main.HeadLSN, HeadLSN: main.HeadLSN, CreatedLSN: 999999, IsDeleted: true}
		require.NoError(t, f.branches.CreateBranchWithData(branch), name)
		require.NotEmpty(t, branch.ID, "%s: a branch without an ID is given one", name)
		stored, err := f.branches.GetBranch(project, name)
		require.NoError(t, err, "%s: the branch is live", name)
		assert.Equal(t, branch.ID, stored.ID)
		assert.Equal(t, main.HeadLSN, stored.HeadLSN)
		assert.Equal(t, main.HeadLSN, stored.BaseLSN)
		entry, err := f.wal.GetEntry(project, stored.CreatedLSN)
		require.NoError(t, err, "%s: created_lsn %d names a record", name, stored.CreatedLSN)
		assert.Equal(t, wal.OpCreateBranch, entry.Operation)
		assert.Equal(t, branch.ID, entry.BranchID)
		assert.Equal(t, stored.CreatedLSN, branch.CreatedLSN, "%s: the caller's branch agrees with the stored one", name)
	}
	empty, err := db.Collection("wal_branches").CountDocuments(ctx, bson.M{"_id": ""})
	require.NoError(t, err)
	assert.Zero(t, empty, "no branch is stored under an empty _id")
}
