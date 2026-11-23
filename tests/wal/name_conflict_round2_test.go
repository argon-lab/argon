package wal_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round 2 (nameConflict reads the index from the server's message with
// strings.Contains). The message ends with the duplicate key's values, and
// one of them is the branch name, which nothing validates:
//
//	E11000 duplicate key error collection: <db>.wal_branches index: project_id_1_name_1 dup key: { project_id: "...", name: "<the name>" }
//
// A conflict on the legacy index is therefore taken for one on the live
// index when the name itself contains the live index's text. The create is
// then answered "branch already exists" for a name no live branch has, and
// the legacy index is not retired.
//
// Control: the same steps with an ordinary name go through
// (TestBranch_CreateRetiresALegacyIndexAnOlderReleasePutBack).
func TestBranchRound2_LegacyConflictIsClassifiedByItsIndexNotByTheBranchName(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "round2-name-in-message"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	const name = "notes on argon.wal_branches index: project_id_1_name_1_live and its uses"
	_, err = f.branches.CreateBranch(project, name, main.ID)
	require.NoError(t, err)
	require.NoError(t, f.branches.DeleteBranch(project, name))
	legacyBranchIndex(t, db) // an older release starts and puts its index back

	_, err = f.branches.GetBranch(project, name)
	require.Error(t, err, "no live branch has the name")
	_, err = f.branches.CreateBranch(project, name, main.ID)
	assert.NoError(t, err, "the name is free: the create should retire the legacy index and go through")
	assert.NotContains(t, branchIndexNames(t, db), "project_id_1_name_1", "the legacy index should have been retired")
}
