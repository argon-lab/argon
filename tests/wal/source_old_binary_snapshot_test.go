package wal_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
)

func TestSource_OldBinarySnapshotKeys(t *testing.T) {
	h := newHistoryFixture(t, "old-binary-snapshots")
	ctx := t.Context()
	var walReads, chunkReads atomic.Int32
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" {
			switch e.Command.Lookup("find").StringValue() {
			case "wal_log":
				walReads.Add(1)
			case "wal_snapshot_chunks":
				chunkReads.Add(1)
			}
		}
	}}
	db := electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name())
	f := newSnapshotFixture(t, db)
	snaps := f.snapshots
	store := snapshot.NewMongoChunkStore(db)
	compressor, err := wal.NewCompressor(nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, compressor.Close()) })
	empty := primitive.Binary{Subtype: 2, Data: []byte{}}
	four := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	emptyKey, fourKey := wal.DocumentIDString(empty), wal.DocumentIDString(four)
	oldLSNs := make(map[string]int64)
	for _, c := range []struct {
		coll, frameKey string
		frameID        any
		usable         bool
	}{
		{"shared-frame-key", fourKey, empty, false},
		{"shared-image-key", emptyKey, four, false},
		{"ordinary", "box", "box", true},
	} {
		t.Run(c.coll, func(t *testing.T) {
			ids := []any{empty, four}
			if c.usable {
				ids = []any{c.frameID}
			}
			for _, id := range ids {
				_, err := h.writer.Put(ctx, c.coll, bson.M{"_id": id})
				require.NoError(t, err)
			}
			main := h.reload()
			lsn := main.HeadLSN
			oldLSNs[c.coll] = lsn
			frame, err := bson.Marshal(bson.D{{Key: "k", Value: c.frameKey}, {Key: "d", Value: bson.M{"_id": c.frameID}}})
			require.NoError(t, err)
			chunk, err := compressor.Compress(frame)
			require.NoError(t, err)
			id, err := store.Put(ctx, chunk)
			require.NoError(t, err)
			_, err = h.db.Collection("wal_snapshots").InsertOne(ctx, bson.M{
				"project_id": h.project, "branch_id": main.ID, "collection": c.coll, "lsn": lsn,
				"visibility_rules": snapshot.CurrentVisibilityRules, "chunk_ids": bson.A{id}, "doc_count": 1,
			})
			require.NoError(t, err)

			wantLSN := int64(0)
			if c.usable {
				wantLSN = lsn
			}
			walReads.Store(0)
			chunkReads.Store(0)
			usable, ok, err := snaps.UsableLSN(main, c.coll, 0, lsn, lsn)
			require.NoError(t, err)
			require.Equal(t, c.usable, ok)
			require.Equal(t, wantLSN, usable)
			require.EqualValues(t, 1, walReads.Load())
			require.EqualValues(t, 1, chunkReads.Load())
			walReads.Store(0)
			chunkReads.Store(0)
			coverage, err := snaps.NewestUsableLSN(main, c.coll, lsn, lsn)
			require.NoError(t, err)
			require.Equal(t, wantLSN, coverage)
			require.Zero(t, walReads.Load())
			require.Zero(t, chunkReads.Load())
			got, err := f.mat.MaterializeCollection(main, c.coll)
			require.NoError(t, err)
			if c.usable {
				require.EqualValues(t, 1, chunkReads.Load())
			} else {
				require.Zero(t, chunkReads.Load())
			}
			require.Len(t, got, len(ids))
			for _, id := range ids {
				require.Equal(t, id, got[wal.DocumentIDString(id)]["_id"])
			}
		})
	}
	// A newly built snapshot includes both IDs and needs no chunk inspection
	// when only its coverage is requested.
	h.put("next", 1)
	main := h.reload()
	current, err := snaps.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	chunkReads.Store(0)
	walReads.Store(0)
	for _, snap := range current {
		require.True(t, snap.ExactOldBinaryKeys)
		lsn, _, err := snaps.UsableLSN(main, snap.Collection, 0, main.HeadLSN, main.HeadLSN)
		require.NoError(t, err)
		require.Equal(t, main.HeadLSN, lsn)
	}
	require.Zero(t, walReads.Load())
	require.Zero(t, chunkReads.Load())

	// Certification survives a process restart. Once complete, starting a
	// second service and requesting coverage for any of these old manifests
	// reads only metadata.
	_ = newSnapshotFixture(t, db)
	for coll, at := range oldLSNs {
		var m snapshot.Snapshot
		err := db.Collection("wal_snapshots").FindOne(ctx, bson.M{"branch_id": main.ID, "collection": coll, "lsn": at}).Decode(&m)
		require.NoError(t, err)
		require.NotNil(t, m.OldKeysTrusted)
		require.Equal(t, coll == "ordinary", *m.OldKeysTrusted)
		require.False(t, m.ExactOldBinaryKeys)
	}
	walReads.Store(0)
	chunkReads.Store(0)
	fresh := newSnapshotFixture(t, db).snapshots
	for coll, at := range oldLSNs {
		_, ok, err := fresh.UsableLSN(main, coll, 0, at, at)
		require.NoError(t, err)
		require.Equal(t, coll == "ordinary", ok)
	}
	require.Zero(t, walReads.Load())
	require.Zero(t, chunkReads.Load())
	at := oldLSNs["ordinary"]
	state, _, ok, err := fresh.FindUsable(main, "ordinary", 0, at, at)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, state, "box")
	require.Zero(t, walReads.Load())
	require.EqualValues(t, 1, chunkReads.Load())

	// An old process could snapshot while a resync was still writing a
	// fresh copy. Even a previously certified snapshot cannot stand in
	// for the subsequently published baseline.
	repaired := *main
	repaired.CollectionBaselines = []wal.CollectionBaseline{{Collection: "ordinary", From: at, Complete: main.HeadLSN}}
	_, ok, err = fresh.UsableLSN(&repaired, "ordinary", at, at, repaired.HeadLSN)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestSource_UnavailableLegacySnapshotChunk(t *testing.T) {
	h := newHistoryFixture(t, "unavailable-old-snapshot")
	at := h.put("row", 1)
	ctx := t.Context()
	store := snapshot.NewMongoChunkStore(h.db)
	compressor, err := wal.NewCompressor(nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, compressor.Close()) })
	frame, err := bson.Marshal(bson.M{"k": "row0", "d": bson.M{"_id": "row0"}})
	require.NoError(t, err)
	data, err := compressor.Compress(frame)
	require.NoError(t, err)
	id, err := store.Put(ctx, data)
	require.NoError(t, err)
	require.NoError(t, store.Delete(ctx, []string{id}))
	manifests := h.db.Collection("wal_snapshots")
	result, err := manifests.InsertOne(ctx, bson.M{
		"project_id": h.project, "branch_id": h.main.ID, "collection": "docs", "lsn": at,
		"visibility_rules": snapshot.CurrentVisibilityRules, "chunk_ids": bson.A{id}, "doc_count": 1,
	})
	require.NoError(t, err)
	filter := bson.M{"_id": result.InsertedID}
	_ = newSnapshotFixture(t, h.db)
	var m snapshot.Snapshot
	require.NoError(t, manifests.FindOne(ctx, filter).Decode(&m))
	require.Nil(t, m.OldKeysTrusted)
	_, err = store.Put(ctx, data)
	require.NoError(t, err)
	_ = newSnapshotFixture(t, h.db)
	require.NoError(t, manifests.FindOne(ctx, filter).Decode(&m))
	require.NotNil(t, m.OldKeysTrusted)
	require.True(t, *m.OldKeysTrusted)
}

func TestSource_EmptySnapshotAfterAmbiguousDelete(t *testing.T) {
	h := newHistoryFixture(t, "empty-old-snapshot")
	ctx := t.Context()
	// Reserve an LSN then replace its entry with an image-less old tombstone.
	tombLSN := h.put("used", 1)
	main := h.reload()
	four := primitive.Binary{Subtype: 2, Data: make([]byte, 4)}
	result, err := h.db.Collection("wal_log").ReplaceOne(ctx, bson.M{"branch_id": main.ID, "lsn": tombLSN}, bson.M{
		"project_id": h.project, "branch_id": main.ID, "lsn": tombLSN, "v": 2,
		"operation": "delete", "collection": "docs", "key_v": 1, "document_id": wal.DocumentIDString(four),
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.ModifiedCount)
	child, err := h.branches.CreateBranch(h.project, "child", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(h.wal, h.branches, h.mat, child)
	head, err := writer.Put(ctx, "other", bson.M{"_id": "one"})
	require.NoError(t, err)
	child, err = h.branches.GetBranchByID(child.ID)
	require.NoError(t, err)
	require.Equal(t, head, child.HeadLSN)
	for _, branch := range []*wal.Branch{main, child} {
		_, err := h.db.Collection("wal_snapshots").InsertOne(ctx, bson.M{
			"project_id": h.project, "branch_id": branch.ID, "collection": "docs", "lsn": branch.HeadLSN,
			"visibility_rules": snapshot.CurrentVisibilityRules, "chunk_ids": bson.A{}, "doc_count": 0,
		})
		require.NoError(t, err)
	}
	for _, branch := range []*wal.Branch{main, child} {
		got, err := h.mat.MaterializeCollection(branch, "docs")
		var ambiguous *wal.AmbiguousOldBinaryError
		require.ErrorAs(t, err, &ambiguous)
		require.Nil(t, got)
		require.Equal(t, main.ID, ambiguous.BranchID)
		require.Equal(t, tombLSN, ambiguous.LSN)
		coverage, err := h.snapshots.NewestUsableLSN(branch, "docs", branch.HeadLSN, branch.HeadLSN)
		require.NoError(t, err)
		require.Zero(t, coverage)
	}
}
