package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Round 3 (6aa0f7a: GetRestorePreview counts only entry.IsData(), "a
// branch's own create record, or a merge's audit record, is not an operation
// a reset takes away"). One control record IS state a reset takes away: the
// resync_collection marker. A resync that cannot tell what an old binary
// history holds starts the collection over from that marker
// (ingest/resync.go, "Start this collection from empty and stream its
// complete source contents again"), and a collection the source no longer
// has gets the marker and nothing else. The marker is the collection's
// baseline from then on: every read at or above it sees the collection as
// the resync left it, every read below it sees what was there before.
//
// A reset of that branch (after `argon source detach`; a follower cannot be
// reset) to a point below the marker brings the collection's earlier
// contents back. Until 6aa0f7a the preview of that reset said "Discards 1
// operation(s)" and listed the collection. Now it says "Discards 0
// operation(s)" and lists nothing, for a reset that changes what the
// collection holds.
//
// The marker and its baseline are written here the way the resync writes
// them (an OpResyncCollection entry for the collection, and a
// collection_baselines element from the marker's LSN, complete at the head
// the resync published); the resync itself needs a source deployment and an
// old binary history to get there.
func TestRound3_RestorePreviewShowsACollectionAResyncMarkerStartedOver(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("round3-preview-marker-%d", time.Now().UnixNano())
	f, _, mainID := newSandboxFixture(t, db, project) // main holds docs/seed
	main, err := f.branches.GetBranchByID(mainID)
	require.NoError(t, err)
	main = round2Writes(t, f, main, 3) // docs holds four documents
	before := main.HeadLSN

	// The resync starts "docs" over: the source no longer has it.
	marker := &wal.Entry{
		ProjectID: project, BranchID: main.ID, Operation: wal.OpResyncCollection, Collection: "docs",
		Actor: "source", Metadata: map[string]interface{}{"capture_operation": "resync"},
	}
	markerLSN, err := f.wal.Append(marker)
	require.NoError(t, err)
	require.NoError(t, f.branches.UpdateBranchHead(main.ID, markerLSN))
	_, err = db.Collection("wal_branches").UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{"$push": bson.M{
		"collection_baselines": wal.CollectionBaseline{Collection: "docs", From: markerLSN, Complete: markerLSN},
	}})
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)

	now, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	require.Empty(t, now, "the control: after the resync the branch's docs collection is empty")

	preview, err := f.restore.GetRestorePreview(main.ID, before)
	require.NoError(t, err)
	t.Logf("preview of a reset to LSN %d: discards %d operation(s), collections %v", before, preview.OperationsToDiscard, preview.AffectedCollections)

	// What the reset previewed really does.
	reset, err := f.restore.ResetBranchToLSN(main.ID, before)
	require.NoError(t, err)
	after, err := f.mat.MaterializeCollection(reset, "docs")
	require.NoError(t, err)
	require.Len(t, after, 4, "the control: the reset brings back the four documents the resync had removed")

	assert.Contains(t, preview.AffectedCollections, "docs",
		"the reset changes docs from %d to %d documents; its preview lists no collection", len(now), len(after))
	assert.NotZero(t, preview.OperationsToDiscard,
		"the reset discards the resync of docs; its preview says it discards nothing")
}
