package wal_test

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
)

// cancelAfter calls interrupt once the n-th successful command named
// command has been answered.
func cancelAfter(command string, n int64, interrupt func()) *event.CommandMonitor {
	var seen atomic.Int64
	return &event.CommandMonitor{
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if e.CommandName == command && seen.Add(1) == n {
				interrupt()
			}
		},
	}
}

func dataLSNs(t *testing.T, db *mongo.Database, branchID string, upTo int64) []int64 {
	t.Helper()
	cursor, err := db.Collection("wal_log").Find(t.Context(), bson.M{"branch_id": branchID, "operation": bson.M{"$in": bson.A{"put", "delete"}}, "lsn": bson.M{"$lte": upTo}})
	require.NoError(t, err)
	var rows []struct {
		LSN int64 `bson:"lsn"`
	}
	require.NoError(t, cursor.All(t.Context(), &rows))
	lsns := make([]int64, len(rows))
	for i, r := range rows {
		lsns[i] = r.LSN
	}
	sort.Slice(lsns, func(i, j int) bool { return lsns[i] < lsns[j] })
	return lsns
}

// Garbage collection deletes oldest first, one transaction per batch. Wherever
// it is interrupted, the rows left are the newest ones, so no document keeps
// an older row whose newer one was removed (issues 155 and 168), and the
// single-document read agrees with the collection read.
func TestGCInterruptedReclaimKeepsNewestRows(t *testing.T) {
	defer wal.SetReclaimBatchSizeForTest(2)()
	const docs, rounds = 5, 3
	for _, stop := range []struct {
		name    string
		command string
	}{{"between batches", "commitTransaction"}, {"inside a batch", "delete"}} {
		for n := int64(1); n <= docs*rounds/2; n++ {
			t.Run(fmt.Sprintf("%s/%d", stop.name, n), func(t *testing.T) {
				ctx := t.Context()
				db := setupTestDB(t)
				f := newSnapshotFixture(t, db)
				main, err := f.branches.CreateBranch("gc-order", "main", "")
				require.NoError(t, err)
				writer := walwriter.New(f.wal, f.branches, f.mat, main)
				var head int64
				for r := 1; r <= rounds; r++ {
					for d := 0; d < docs; d++ {
						head, err = writer.Put(ctx, "items", bson.M{"_id": fmt.Sprintf("d%d", d), "v": int32(r)})
						require.NoError(t, err)
					}
				}
				_, err = f.snapshots.CreateSnapshot(ctx, main.ID, head)
				require.NoError(t, err)
				before := dataLSNs(t, db, main.ID, head)

				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				client := faultMongoClient(t, cancelAfter(stop.command, n, cancel))
				runner := newSnapshotFixture(t, client.Database(db.Name()))
				_, err = gc.NewService(runner.wal, runner.branches, runner.snapshots).RunProject(runCtx, main.ProjectID, zeroRetention)
				require.ErrorIs(t, err, context.Canceled)

				left := dataLSNs(t, db, main.ID, head)
				require.Equal(t, before[len(before)-len(left):], left, "the rows left are the newest ones")
				if stop.command == "delete" {
					require.Zero(t, (len(before)-len(left))%2, "an interrupted batch is removed whole or not at all")
				}
				check := func() {
					t.Helper()
					main, err = f.branches.GetBranchByID(main.ID)
					require.NoError(t, err)
					state, err := f.mat.MaterializeCollection(main, "items")
					require.NoError(t, err)
					for d := 0; d < docs; d++ {
						id := fmt.Sprintf("d%d", d)
						want := bson.M{"_id": id, "v": int32(rounds)}
						require.Equal(t, want, state[id])
						got, err := f.mat.MaterializeDocument(main, "items", id)
						require.NoError(t, err)
						require.Equal(t, want, got, "single-document read of %s", id)
					}
				}
				check()

				report, err := gc.NewService(f.wal, f.branches, f.snapshots).RunProject(ctx, main.ProjectID, zeroRetention)
				require.NoError(t, err)
				require.EqualValues(t, len(left), report.EntriesRemoved)
				require.Empty(t, dataLSNs(t, db, main.ID, head))
				check()
			})
		}
	}
}

// A history already left in the state of issue 155 (floor recorded) or 168
// (no floor, as from releases up to 2.1.5) by an earlier interrupted run is
// repaired by the next garbage collection: it deletes the older row too,
// after which the single-document read, the writer's pre-image and undo all
// use the covering snapshot.
func TestGCRepairsRowLeftBehindByInterruptedReclaim(t *testing.T) {
	for _, recordedFloor := range []bool{true, false} {
		t.Run(fmt.Sprintf("floor=%t", recordedFloor), func(t *testing.T) {
			const collection, id = "items", "q"
			ctx := t.Context()
			h := newHistoryFixture(t, "gc-repair")
			a := bson.M{"_id": id, "version": "A"}
			b := bson.M{"_id": id, "version": "B"}
			_, err := h.writer.Put(ctx, collection, a)
			require.NoError(t, err)
			last, err := h.writer.Put(ctx, collection, b)
			require.NoError(t, err)
			_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, last)
			require.NoError(t, err)
			if recordedFloor {
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, collection, last))
			}
			removed, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": last})
			require.NoError(t, err)
			require.EqualValues(t, 1, removed.DeletedCount)

			// Later history moves the retention boundary past the snapshot,
			// as it does in a project that keeps being written.
			_, err = h.writer.Put(ctx, "other", bson.M{"_id": "later"})
			require.NoError(t, err)

			branch := h.reload()
			stale, err := h.mat.MaterializeDocument(branch, collection, id)
			require.NoError(t, err)
			require.Equal(t, a, stale, "the damaged state reads the older row")

			report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
			require.NoError(t, err)
			require.EqualValues(t, 1, report.EntriesRemoved)

			branch = h.reload()
			singular, err := h.mat.MaterializeDocument(branch, collection, id)
			require.NoError(t, err)
			require.Equal(t, b, singular)
			state, err := h.mat.MaterializeCollection(branch, collection)
			require.NoError(t, err)
			require.Equal(t, b, state[id])
			checkWriterOverwriteAndUndo(t, h, branch, collection, b)
		})
	}
}
