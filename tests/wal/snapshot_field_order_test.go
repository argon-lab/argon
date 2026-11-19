package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestWriteBack_SnapshotFieldOrder(t *testing.T) {
	const collection, key = "catalog", "item"
	ctx := t.Context()
	ordered := bson.D{
		{Key: "_id", Value: key}, {Key: "z", Value: 1}, {Key: "b", Value: 2},
		{Key: "a", Value: bson.D{{Key: "third", Value: 3}, {Key: "first", Value: 1}, {Key: "second", Value: 2}}},
		{Key: "y", Value: 4}, {Key: "c", Value: 5}, {Key: "x", Value: 6}, {Key: "d", Value: 7},
	}
	want, err := wal.MarshalDocument(ordered)
	require.NoError(t, err)

	cases := []struct {
		name    string
		reclaim bool
	}{
		{name: "retained WAL and older snapshot"},
		{name: "mixed descendant snapshot"},
		{name: "opaque legacy envelope"},
		{name: "two snapshots after reclaim", reclaim: true},
		{name: "older snapshot after reclaim", reclaim: true},
		{name: "older and ordered snapshots at same LSN after reclaim", reclaim: true},
	}
	for _, tc := range cases {
		mode := tc.name
		t.Run(mode, func(t *testing.T) {
			h := newHistoryFixture(t, "snapshot-field-order")
			at, err := h.wal.Append(&wal.Entry{BranchID: h.main.ID, ProjectID: h.project,
				Collection: collection, DocumentID: key, Operation: wal.OpPut, PostImage: want})
			require.NoError(t, err)
			require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
			branch := h.reload()
			version := want
			if mode == "mixed descendant snapshot" || mode == "opaque legacy envelope" {
				branch, err = h.branches.CreateBranch(h.project, "mixed", branch.ID)
				require.NoError(t, err)
				other := mustMarshalBSON(bson.D{{Key: "_id", Value: "other"}, {Key: "n", Value: 1}})
				if mode == "opaque legacy envelope" {
					// This segment starts with a typed row. Its first entry
					// alone cannot establish that every key is typed.
					_, err := h.wal.Append(&wal.Entry{BranchID: branch.ID, ProjectID: h.project,
						Collection: collection, DocumentID: "other", Operation: wal.OpPut, PostImage: other})
					require.NoError(t, err)
					updated := append(bson.D{}, ordered...)
					updated[1].Value = int32(900)
					version = mustMarshalBSON(updated)
					other = version
				}
				at, err = h.wal.Append(&wal.Entry{BranchID: branch.ID, ProjectID: h.project,
					Collection: collection, DocumentID: "other", Operation: wal.OpPut, PostImage: other})
				require.NoError(t, err)
				_, err = h.db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": branch.ID, "lsn": at},
					bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": "opaque-envelope"}})
				require.NoError(t, err)
				require.NoError(t, h.branches.UpdateBranchHead(branch.ID, at))
				branch, err = h.branches.GetBranchByID(branch.ID)
				require.NoError(t, err)
			}
			expected := version
			if mode == "two snapshots after reclaim" {
				_, err := h.snapshots.CreateSnapshot(ctx, branch.ID, at)
				require.NoError(t, err)
			} else {
				var values bson.M
				require.NoError(t, wal.UnmarshalDocument(version, &values))
				sorted, err := wal.MarshalDocument(values)
				require.NoError(t, err)
				var sortedFields bson.D
				require.NoError(t, wal.UnmarshalDocument(sorted, &sortedFields))
				insertReleasedSnapshot(t, h.db, branch, collection, at, key, sortedFields)
				if mode == "older snapshot after reclaim" {
					expected = sorted
				}
				if mode == "older and ordered snapshots at same LSN after reclaim" {
					// Rebuilding at the same LSN recovers the original bytes
					// from the WAL before GC leaves only the two manifests.
					_, err := h.snapshots.CreateSnapshot(ctx, branch.ID, at)
					require.NoError(t, err)
				}
			}
			if tc.reclaim {
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, branch.ID, collection, at))
				removed, err := h.wal.DeleteDataEntriesUpTo(branch.ID, collection, at)
				require.NoError(t, err)
				require.EqualValues(t, 1, removed)
			}
			if mode == "two snapshots after reclaim" {
				// A second snapshot of the untouched collection can now obtain
				// its original bytes only from the first snapshot's frame.
				writer := walwriter.New(h.wal, h.branches, h.mat, branch)
				later, err := writer.Put(ctx, "other", bson.M{"_id": "next"})
				require.NoError(t, err)
				_, err = h.snapshots.CreateSnapshot(ctx, branch.ID, later)
				require.NoError(t, err)
				branch = h.reload()
			}

			child, err := h.branches.CreateBranch(h.project, "sandbox", branch.ID)
			require.NoError(t, err)
			checkouts := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
			info, err := checkouts.Checkout(ctx, child.ID)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, checkouts.Discard(ctx, child.ID)) })
			got, err := h.db.Client().Database(info.PhysicalDB).Collection(collection).FindOne(ctx, bson.M{"_id": key}).Raw()
			require.NoError(t, err)
			require.Equal(t, expected, got, "checkout bytes")

			writer := walwriter.New(h.wal, h.branches, h.mat, branch)
			lsn, deleted, err := writer.Delete(ctx, collection, key)
			require.NoError(t, err)
			require.True(t, deleted)
			history, err := h.wal.GetDocumentHistory(branch.ID, collection, key, lsn, lsn)
			require.NoError(t, err)
			require.Len(t, history, 1)
			require.Equal(t, expected, history[0].PreImage, "point-read bytes")
		})
	}
}

func TestWriteBack_OlderSnapshotOverOrderedFrame(t *testing.T) {
	const collection, key = "catalog", "item"
	ctx := t.Context()
	original := bson.D{
		{Key: "_id", Value: key}, {Key: "zone", Value: "north"}, {Key: "bin", Value: int32(3)},
		{Key: "cost", Value: bson.D{{Key: "tax", Value: int32(5)}, {Key: "base", Value: int32(100)}}},
		{Key: "aisle", Value: int32(9)}, {Key: "shelf", Value: int32(2)},
	}
	for _, mode := range []string{"same branch", "inherited", "verified edit", "unverified edit", "reclaimed edit"} {
		t.Run(mode, func(t *testing.T) {
			h := newHistoryFixture(t, "ordered-base")
			appendPut := func(branch *wal.Branch, doc bson.D, metadata map[string]any) int64 {
				lsn, err := h.wal.Append(&wal.Entry{ProjectID: h.project, BranchID: branch.ID,
					Collection: collection, DocumentID: key, Operation: wal.OpPut,
					PostImage: mustMarshalBSON(doc), Metadata: metadata})
				require.NoError(t, err)
				require.NoError(t, h.branches.UpdateBranchHead(branch.ID, lsn))
				return lsn
			}
			reclaim := func(branch *wal.Branch, lsn int64) {
				require.NoError(t, h.wal.RecordReclaimFloor(ctx, branch.ID, collection, lsn))
				n, err := h.wal.DeleteDataEntriesUpTo(branch.ID, collection, lsn)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			}
			at := appendPut(h.main, original, nil)
			_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
			require.NoError(t, err)
			parent := h.reload()
			reclaim(parent, at)

			branch := parent
			if mode != "same branch" {
				branch, err = h.branches.CreateBranch(h.project, "child", parent.ID)
				require.NoError(t, err)
			}
			latest := append(bson.D{}, original...)
			if mode == "inherited" || mode == "same branch" {
				writer := walwriter.New(h.wal, h.branches, h.mat, branch)
				at, err = writer.Put(ctx, "other", bson.M{"_id": "next"})
				require.NoError(t, err)
			} else {
				// Identical values in a different order are still a new image.
				latest[1], latest[3] = latest[3], latest[1]
				var metadata map[string]any
				if mode == "unverified edit" {
					metadata = map[string]any{"capture_images": "attached_unverified"}
				}
				at = appendPut(branch, latest, metadata)
			}

			want := mustMarshalBSON(latest)
			var values bson.M
			require.NoError(t, wal.UnmarshalDocument(want, &values))
			sorted, err := wal.MarshalDocument(values)
			require.NoError(t, err)
			var fields bson.D
			require.NoError(t, wal.UnmarshalDocument(sorted, &fields))
			insertReleasedSnapshot(t, h.db, branch, collection, at, key, fields)
			if mode == "reclaimed edit" {
				reclaim(branch, at)
			}
			if mode == "unverified edit" || mode == "reclaimed edit" {
				want = sorted
			}

			sandbox := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
			dropPhysical(t, h.db.Client(), branch.ID)
			info, err := sandbox.Checkout(ctx, branch.ID)
			require.NoError(t, err)
			got, err := h.db.Client().Database(info.PhysicalDB).Collection(collection).FindOne(ctx, bson.M{"_id": key}).Raw()
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}
