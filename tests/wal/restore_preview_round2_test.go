package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// round2Writes puts n documents on the branch and returns it reloaded.
func round2Writes(t *testing.T, f *snapshotFixture, branch *wal.Branch, n int) *wal.Branch {
	t.Helper()
	w := walwriter.New(f.wal, f.branches, f.mat, branch)
	for i := 0; i < n; i++ {
		_, err := w.Put(context.Background(), "docs", bson.M{"_id": fmt.Sprintf("%s-%d", branch.Name, i), "v": int32(i)})
		require.NoError(t, err)
	}
	reloaded, err := f.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	return reloaded
}

// Round 2 (effect of keying a restored branch's create record by its ID).
// `argon restore preview` reports "Discards N operation(s)" from
// GetRestorePreview, which counts every WAL entry of the branch above the
// target, control records included. Until 88d08c9 the create record of a
// branch made by `argon restore --as` or from a pin was keyed by the branch
// NAME, so it was not among them and the preview of such a branch counted
// its writes. Now the record is keyed by the branch ID and sits above the
// fork point (it is appended after the fork point is chosen), so a preview
// back to the fork point counts one operation the branch never made.
func TestBranchRound2_RestorePreviewOfARestoredBranchCountsItsWrites(t *testing.T) {
	db := setupTestDB(t)
	project := fmt.Sprintf("round2-preview-restored-%d", time.Now().UnixNano())
	f, _, mainID := newSandboxFixture(t, db, project)
	main, err := f.branches.GetBranchByID(mainID)
	require.NoError(t, err)

	restored, err := f.restore.CreateBranchFromPin(project, mainID, "from-pin", main.HeadLSN)
	require.NoError(t, err)
	restored = round2Writes(t, f, restored, 2)

	preview, err := f.restore.GetRestorePreview(restored.ID, restored.BaseLSN)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"docs": 2}, preview.AffectedCollections)
	assert.Equal(t, 2, preview.OperationsToDiscard,
		"the branch made two writes; the preview of a reset to its fork point counts %d operations (its create_branch record at LSN %d is one of them)",
		preview.OperationsToDiscard, restored.CreatedLSN)
}

// The same count for a branch made by CreateBranch, whose create record has
// always been keyed by its ID: the preview has always been one too high
// there. Pre-existing; here to show where the count comes from.
func TestBranchRound2_RestorePreviewOfAForkCountsItsWrites(t *testing.T) {
	db := setupTestDB(t)
	project := fmt.Sprintf("round2-preview-fork-%d", time.Now().UnixNano())
	f, _, mainID := newSandboxFixture(t, db, project)

	fork, err := f.branches.CreateBranch(project, "fork", mainID)
	require.NoError(t, err)
	fork = round2Writes(t, f, fork, 2)

	preview, err := f.restore.GetRestorePreview(fork.ID, fork.BaseLSN)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"docs": 2}, preview.AffectedCollections)
	assert.Equal(t, 2, preview.OperationsToDiscard,
		"the branch made two writes; the preview of a reset to its fork point counts %d operations", preview.OperationsToDiscard)
}
