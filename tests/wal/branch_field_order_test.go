package wal_test

import (
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestWriteBack_EditedBranchFieldOrder(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	ctx := t.Context()
	_, err := f.source.Collection("catalog").InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	info, physical := f.agentSandbox(t, "edits")
	coll := physical.Collection("catalog")

	// Seed the branch through native capture, so both historical images
	// originate in MongoDB independently of how its first checkout was loaded.
	shelf := bson.D{
		{Key: "zone", Value: "A"}, {Key: "price", Value: int32(5)}, {Key: "aisle", Value: "B"},
		{Key: "stock", Value: int32(9)}, {Key: "bin", Value: int32(1)},
	}
	doc := bson.D{
		{Key: "_id", Value: "item"}, {Key: "shelf", Value: shelf}, {Key: "batch", Value: shelf},
		{Key: "price", Value: int32(5)}, {Key: "name", Value: "lamp"}, {Key: "copies", Value: bson.A{shelf}},
	}
	_, err = coll.InsertOne(ctx, doc)
	require.NoError(t, err)
	first := readSourceCase(t, coll, "item")
	require.NoError(t, f.services.SyncBranch(ctx, info.BranchID))
	branch, err := f.services.Branches.GetBranchByID(info.BranchID)
	require.NoError(t, err)
	firstLSN := branch.HeadLSN

	_, err = coll.UpdateByID(ctx, "item", bson.M{"$set": bson.M{"price": int32(8)}})
	require.NoError(t, err)
	second := readSourceCase(t, coll, "item")
	require.NoError(t, f.services.Checkout.Release(ctx, branch.ID))
	branch, err = f.services.Branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	require.Greater(t, branch.HeadLSN, firstLSN)

	t.Run("re-checkout captured edit", func(t *testing.T) {
		_, err := f.services.Checkout.Checkout(ctx, branch.ID)
		require.NoError(t, err)
		require.Equal(t, second, readSourceCase(t, coll, "item"))
	})

	t.Run("restore fork", func(t *testing.T) {
		historical, err := f.services.Restore.CreateBranchAtLSN(branch.ProjectID, branch.ID, "historical", firstLSN)
		require.NoError(t, err)
		co, err := f.services.Checkout.Checkout(ctx, historical.ID)
		require.NoError(t, err)
		require.Equal(t, first, readSourceCase(t, f.services.Client.Database(co.PhysicalDB).Collection("catalog"), "item"))
	})

	require.NoError(t, f.services.Checkout.Release(ctx, branch.ID))
	_, err = f.services.Restore.ResetBranchToLSN(branch.ID, firstLSN)
	require.NoError(t, err)
	_, err = f.services.Checkout.Checkout(ctx, branch.ID)
	require.NoError(t, err)
	t.Run("restore reset", func(t *testing.T) {
		require.Equal(t, first, readSourceCase(t, coll, "item"))
	})

	t.Run("merge into checked-out branch", func(t *testing.T) {
		child, err := f.services.Sandbox.Create(ctx, branch.ProjectID, branch.ID, "repriced", time.Hour)
		require.NoError(t, err)
		require.NoError(t, f.services.StartCapture(ctx, child.BranchID, "repriced"))
		childColl := f.services.Client.Database(child.PhysicalDB).Collection("catalog")
		_, err = childColl.UpdateByID(ctx, "item", bson.M{"$set": bson.M{"price": int32(8)}})
		require.NoError(t, err)
		plan := f.preview(t, child.BranchID)
		require.Empty(t, plan.Conflicts)
		require.Len(t, plan.Changes, 1)
		result, err := f.services.Merge.Apply(ctx, plan.ID, "")
		require.NoError(t, err)
		require.Equal(t, 1, result.Applied)
		actual := readSourceCase(t, coll, "item")
		require.Equal(t, plan.Changes[0].DocumentRaw, actual, "live merge writes the exact saved after-image")
		require.Equal(t, second, actual, "same update as the captured edit")
	})
}

func TestWriteBack_CheckoutMixedImageProvenance(t *testing.T) {
	ctx := t.Context()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	svc := checkout.NewService(db.Client(), db, f.branches, f.mat)
	branch, err := f.branches.CreateBranch("mixed-images", "main", "")
	require.NoError(t, err)
	dropPhysical(t, db.Client(), branch.ID)

	inner := bson.D{{Key: "z", Value: int32(1)}, {Key: "b", Value: int32(2)}, {Key: "a", Value: int32(3)}}
	sortedInner := bson.D{{Key: "a", Value: int32(3)}, {Key: "b", Value: int32(2)}, {Key: "z", Value: int32(1)}}
	expected := make(map[string]bson.Raw)
	for _, id := range []string{"typed", "legacy", "attached"} {
		image, err := bson.Marshal(bson.D{
			{Key: "_id", Value: id}, {Key: "zone", Value: "north"}, {Key: "nested", Value: inner},
			{Key: "bin", Value: int32(3)}, {Key: "array", Value: bson.A{inner}},
		})
		require.NoError(t, err)
		entry := &wal.Entry{ProjectID: branch.ProjectID, BranchID: branch.ID, Collection: "catalog",
			Operation: wal.OpPut, DocumentID: id, PostImage: image}
		if id == "attached" {
			// The previous entry is verified and contains the same values.
			// Its order cannot establish the current unverified image's order.
			_, err = f.wal.Append(entry)
			require.NoError(t, err)
			entry.Metadata = map[string]any{"capture_images": "attached_unverified"}
		}
		lsn, err := f.wal.Append(entry)
		require.NoError(t, err)
		require.NoError(t, f.branches.UpdateBranchHead(branch.ID, lsn))
		if id == "legacy" {
			result, err := db.Collection("wal_log").UpdateOne(ctx, bson.M{"branch_id": branch.ID, "lsn": lsn},
				bson.M{"$unset": bson.M{"key_v": ""}})
			require.NoError(t, err)
			require.EqualValues(t, 1, result.ModifiedCount)
		}
		if id != "attached" {
			expected[id] = image
			continue
		}
		expected[id], err = bson.Marshal(bson.D{
			{Key: "_id", Value: id}, {Key: "array", Value: bson.A{sortedInner}}, {Key: "bin", Value: int32(3)},
			{Key: "nested", Value: sortedInner}, {Key: "zone", Value: "north"},
		})
		require.NoError(t, err)
	}

	info, err := svc.Checkout(ctx, branch.ID)
	require.NoError(t, err)
	coll := db.Client().Database(info.PhysicalDB).Collection("catalog")
	for id, want := range expected {
		t.Run(id, func(t *testing.T) {
			require.Equal(t, want, readSourceCase(t, coll, id))
		})
	}
}
