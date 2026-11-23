package wal_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review (pre-existing, kept as is by the reviewed commit): the create
// record CreateBranchWithData writes carries the branch NAME in branch_id,
// where createBranch writes the branch ID ("data entries key on branch IDs,
// so control entries must too"). Deleting the branch reclaims its WAL by
// branch ID, so the record of a branch forked from a pin or a point in time
// is never reclaimed; now that a name can be used again, every run under one
// name leaves another record that no branch ID leads to.
func TestReview_RestoredBranchCreateRecordIsKeyedByIDAndReclaimed(t *testing.T) {
	db := setupTestDB(t)
	project := fmt.Sprintf("review-restore-record-%d", time.Now().UnixNano())
	f, _, mainID := newSandboxFixture(t, db, project) // wires the delete hook as walcli does
	main, err := f.branches.GetBranchByID(mainID)
	require.NoError(t, err)

	// Control: a branch made by CreateBranch leaves nothing behind.
	_, err = f.branches.CreateBranch(project, "plain", mainID)
	require.NoError(t, err)
	require.NoError(t, f.branches.DeleteBranch(project, "plain"))
	require.Zero(t, createBranchEntries(t, db, project, "plain"), "control: a deleted branch's create record is reclaimed")

	for run := 1; run <= 3; run++ {
		branch, err := f.restore.CreateBranchFromPin(project, mainID, "eval-run", main.HeadLSN)
		require.NoError(t, err, "run %d", run)
		entries, err := f.wal.GetProjectEntries(project, "", 0, 1<<60)
		require.NoError(t, err)
		for _, entry := range entries {
			if entry.Metadata["branch_name"] == "eval-run" {
				assert.Equal(t, branch.ID, entry.BranchID, "run %d: the create record is keyed by the branch ID", run)
			}
		}
		require.NoError(t, f.branches.DeleteBranch(project, "eval-run"))
	}
	assert.Zero(t, createBranchEntries(t, db, project, "eval-run"), "create records of deleted branches left in the WAL")
}
