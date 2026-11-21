package wal_test

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
)

// Match a successful command by its database, collection and request ID.
// Each monitored client runs a single storage operation synchronously.
func faultAfterWrite(db, command, collection string, interrupt func()) *event.CommandMonitor {
	var request atomic.Int64
	var fired atomic.Bool
	return &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.DatabaseName == db && e.CommandName == command {
				name, _ := e.Command.Lookup(command).StringValueOK()
				if collection == "" || collection == name {
					request.Store(e.RequestID)
				}
			}
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if e.CommandName == command && request.Load() == e.RequestID &&
				e.Reply.Lookup("writeErrors").Type == 0 && fired.CompareAndSwap(false, true) {
				interrupt()
			}
		},
	}
}

func TestFaultCheckoutCancelMidCopy(t *testing.T) {
	ctx := t.Context()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	main, err := f.branches.CreateBranch("fault-checkout", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	collections := []string{"red", "blue"}
	for _, name := range collections {
		_, err := writer.Put(ctx, name, bson.M{"_id": name, "v": int32(7)})
		require.NoError(t, err)
	}
	physical := db.Client().Database(checkout.PhysicalDBName(main.ID))
	dropPhysical(t, db.Client(), main.ID)
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	monitor := faultAfterWrite(physical.Name(), "insert", "", func() {
		cancel()
		names, err := physical.ListCollectionNames(ctx, bson.M{})
		require.NoError(t, err)
		require.Len(t, names, 1, "a strict prefix of the physical collections exists")
		count, err := physical.Collection(names[0]).CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
	})
	client := faultMongoClient(t, monitor)
	svc := checkout.NewService(client, db, f.branches, f.mat)
	_, err = svc.Checkout(copyCtx, main.ID)
	require.ErrorIs(t, err, context.Canceled)
	names, err := db.Client().ListDatabaseNames(ctx, bson.M{"name": physical.Name()})
	require.NoError(t, err)
	require.Empty(t, names, "half copy must be removed")
	count, err := db.Collection("wal_ingest_state").CountDocuments(ctx, bson.M{"_id": main.ID})
	require.NoError(t, err)
	require.Zero(t, count)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	require.False(t, main.IsLive())

	info, err := svc.Checkout(ctx, main.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, info.Documents)
	names, err = physical.ListCollectionNames(ctx, bson.M{})
	require.NoError(t, err)
	require.ElementsMatch(t, collections, names)
	for _, name := range collections {
		cursor, err := physical.Collection(name).Find(ctx, bson.M{})
		require.NoError(t, err)
		var got []bson.M
		require.NoError(t, cursor.All(ctx, &got))
		require.Equal(t, []bson.M{{"_id": name, "v": int32(7)}}, got)
	}
}

func TestFaultSnapshotCancelAfterChunk(t *testing.T) {
	ctx := t.Context()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	main, err := f.branches.CreateBranch("fault-snapshot", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	// The first document fills a chunk on its own. The small tail forces
	// two chunks for one collection, the snapshot's unit of publication.
	want := map[string]bson.M{
		"a": {"_id": "a", "v": strings.Repeat("a", 4<<20)},
		"z": {"_id": "z", "v": int32(9)},
	}
	_, err = writer.Put(ctx, "docs", want["a"])
	require.NoError(t, err)
	at, err := writer.Put(ctx, "docs", want["z"])
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	checkReads := func(published bool) {
		t.Helper()
		_, selected, usable, err := f.snapshots.FindUsable(main, "docs", at, at, at)
		require.NoError(t, err)
		require.Equal(t, published, usable)
		if published {
			require.Equal(t, at, selected)
		}
		state, err := f.mat.MaterializeCollectionAtLSN(main, "docs", at)
		require.NoError(t, err)
		require.True(t, reflect.DeepEqual(want, state), "reads must include both chunks' exact document values")
	}

	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := faultMongoClient(t, faultAfterWrite(db.Name(), "insert", "wal_snapshot_chunks", func() {
		cancel()
		count, err := db.Collection("wal_snapshot_chunks").CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		checkReads(false)
	}))
	build := newSnapshotFixture(t, client.Database(db.Name()))
	_, err = build.snapshots.CreateSnapshot(buildCtx, main.ID, at)
	require.ErrorIs(t, err, context.Canceled)
	checkReads(false)
	snaps, err := f.snapshots.CreateSnapshot(ctx, main.ID, at)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	require.Len(t, snaps[0].ChunkIDs, 2)
	checkReads(true)
}

func TestFaultGCStopsBetweenCollections(t *testing.T) {
	ctx := t.Context()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	main, err := f.branches.CreateBranch("fault-gc", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	names := []string{"red", "blue"}
	written := map[string]int64{}
	var at int64
	for _, name := range names {
		at, err = writer.Put(ctx, name, bson.M{"_id": name, "v": int32(1)})
		require.NoError(t, err)
		written[name] = at
	}
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, at)
	require.NoError(t, err)
	edit, err := writer.Put(ctx, "red", bson.M{"_id": "red", "v": int32(2)})
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := faultMongoClient(t, faultAfterWrite(db.Name(), "delete", "wal_log", cancel))
	runner := newSnapshotFixture(t, client.Database(db.Name()))
	collector := gc.NewService(runner.wal, runner.branches, runner.snapshots)
	_, err = collector.RunProject(runCtx, main.ProjectID, zeroRetention)
	require.ErrorIs(t, err, context.Canceled)
	floors, err := f.wal.ReclaimFloors(main.ID)
	require.NoError(t, err)
	require.Len(t, floors, 1)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	for _, name := range names {
		rows, err := f.wal.GetBranchEntries(main.ID, name, 0, at)
		require.NoError(t, err)
		if _, reclaimed := floors[name]; reclaimed {
			// The floor is published, and the deletion batch that was
			// interrupted is rolled back whole: the row is still there,
			// but reads below the floor are refused.
			require.Len(t, rows, 1)
			_, err = f.mat.MaterializeCollectionAtLSN(main, name, at-1)
			require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
		} else {
			require.Len(t, rows, 1)
			state, err := f.mat.MaterializeCollectionAtLSN(main, name, written[name])
			require.NoError(t, err)
			require.Equal(t, map[string]bson.M{name: {"_id": name, "v": int32(1)}}, state)
		}
	}
	checkHead := func(red int32) {
		t.Helper()
		main, err = f.branches.GetBranchByID(main.ID)
		require.NoError(t, err)
		state, err := f.mat.MaterializeBranch(main)
		require.NoError(t, err)
		require.Equal(t, map[string]map[string]bson.M{
			"red":  {"red": {"_id": "red", "v": red}},
			"blue": {"blue": {"_id": "blue", "v": int32(1)}},
		}, state)
	}
	checkHead(2)
	undos := undo.NewService(f.wal, f.branches, db.Client(), f.mat)
	_, err = undos.BuildPlan(main, at, at, "")
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	plan, err := undos.BuildPlan(main, edit, edit, "")
	require.NoError(t, err)
	restored, deleted, err := undos.Apply(ctx, main, plan)
	require.NoError(t, err)
	require.Equal(t, 1, restored)
	require.Zero(t, deleted)
	checkHead(1)
	report, err := collector.RunProject(ctx, main.ProjectID, zeroRetention)
	require.NoError(t, err)
	require.EqualValues(t, 2, report.EntriesRemoved, "the rolled-back batch and the other collection")
	report, err = collector.RunProject(ctx, main.ProjectID, zeroRetention)
	require.NoError(t, err)
	require.Zero(t, report.EntriesRemoved)
	checkHead(1)
}
