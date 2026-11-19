package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_ProgrammaticImageProvenance(t *testing.T) {
	f, branch := planFixture(t)
	ctx := t.Context()
	for _, prepared := range []bool{true, false} {
		p := f.preview(t, branch)
		p.ID = primitive.NewObjectID()
		if !prepared {
			p.ProvenanceVersion = 0
			p.FollowedDatabase, p.FollowedReplicaSetID = "", ""
		}
		p.Changes = []merge.Change{{Collection: "catalog", DocumentID: "parcel", ID: "parcel", Document: bson.M{"_id": "parcel", "stock": int32(1)}}}
		p.Conflicts = []merge.Conflict{{Collection: "catalog", DocumentID: "lost", Ours: bson.M{"_id": "lost"}}}
		_, err := merge.ApprovalDigest(p)
		require.Error(t, err)
		require.NoError(t, f.services.Merge.SavePlan(ctx, p))
		loaded := f.plan(t, p)
		require.Equal(t, f.source.Name(), p.FollowedDatabase)
		require.NotEmpty(t, p.FollowedReplicaSetID)
		require.Equal(t, approvalFor(t, loaded), approvalFor(t, p))
		require.Contains(t, loaded.Changes[0].SortedImages, "document")
		require.Contains(t, loaded.Conflicts[0].SortedImages, "ours")
		_, err = f.services.WriteBackMerge(ctx, p.ID, merge.StrategyOurs, f.sourceURI, approvalFor(t, p))
		require.ErrorContains(t, err, "unverified after-image")
		require.Nil(t, f.plan(t, p).WriteBack)

		plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
		_, err = plans.UpdateByID(ctx, p.ID, bson.M{"$unset": bson.M{"provenance_version": ""}})
		require.NoError(t, err)
		old := f.plan(t, p)
		err = f.services.Merge.SavePlan(ctx, old)
		require.True(t, mongo.IsDuplicateKeyError(err), "%v", err)
		require.Zero(t, old.ProvenanceVersion)
		require.Zero(t, f.plan(t, old).ProvenanceVersion)
	}
}

func TestWriteBack_AfterImageIDOrder(t *testing.T) {
	f, branch := planFixture(t)
	ctx := t.Context()
	plan, err := f.services.Merge.PreparePlan(ctx, branch)
	require.NoError(t, err)
	image := func(id string, idFirst bool) bson.Raw {
		t.Helper()
		fields := bson.D{{Key: "z", Value: int64(8)}, {Key: "_id", Value: id}, {Key: "a", Value: int32(9)}}
		if idFirst {
			fields[0], fields[1] = fields[1], fields[0]
		}
		raw, err := bson.Marshal(fields)
		require.NoError(t, err)
		return raw
	}
	change, conflict := image("package", false), image("shipment", false)
	plan.Changes = []merge.Change{{Collection: "catalog", DocumentID: "package", DocumentRaw: change}}
	plan.Conflicts = []merge.Conflict{{Collection: "catalog", DocumentID: "shipment", TheirsRaw: conflict}}
	require.NoError(t, f.services.Merge.SavePlan(ctx, plan))
	t.Run("save", func(t *testing.T) {
		saved := f.plan(t, plan)
		require.Equal(t, image("package", true), saved.Changes[0].DocumentRaw)
		require.Equal(t, image("shipment", true), saved.Conflicts[0].TheirsRaw)
		require.Equal(t, approvalFor(t, plan), approvalFor(t, saved))
	})
	t.Run("previously saved", func(t *testing.T) {
		plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
		_, err := plans.UpdateByID(ctx, plan.ID, bson.M{"$set": bson.M{"changes.0.document": change, "conflicts.0.theirs": conflict}})
		require.NoError(t, err)
		old := f.plan(t, plan)
		before := f.sourceState(t, nil)
		result, err := f.services.WriteBackMerge(ctx, old.ID, merge.StrategyTheirs, f.sourceURI, approvalFor(t, old))
		require.ErrorContains(t, err, "after-image must start with _id")
		require.ErrorContains(t, err, "catalog/package")
		require.ErrorContains(t, err, "catalog/shipment")
		require.ErrorContains(t, err, "preview again")
		require.Nil(t, result)
		require.Equal(t, before, f.sourceState(t, nil))
		saved := f.plan(t, old)
		require.Equal(t, merge.StatusPending, saved.Status)
		require.Nil(t, saved.WriteBack)
		require.Empty(t, saved.WriteBackLastError)
	})
}

func TestWriteBack_ZeroTimestampID(t *testing.T) {
	f, branch := planFixture(t)
	ctx := t.Context()
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	key := primitive.Timestamp{}
	_, err = writer.Put(ctx, "catalog", bson.M{
		"_id": key, "nested": bson.M{"ts": primitive.Timestamp{}}, "ts": primitive.Timestamp{I: 1},
	})
	require.NoError(t, err)
	plan := f.preview(t, branch)
	require.Len(t, plan.Changes, 1)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	stored, err := f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": key}).Raw()
	require.NoError(t, err)
	require.Equal(t, plan.Changes[0].DocumentRaw, stored)
}

func TestWriteBack_DeleteMustBeAbsentAfterAllWrites(t *testing.T) {
	f, branch := planFixture(t)
	ctx := t.Context()
	before := readSourceCase(t, f.source.Collection("catalog"), "item-00")
	p, err := f.services.Merge.PreparePlan(ctx, branch)
	require.NoError(t, err)
	p.Changes = []merge.Change{
		{Collection: "catalog", DocumentID: "item-00", BeforeRaw: before, Delete: true},
		{Collection: "catalog", DocumentID: "item-00", DocumentRaw: sourceCaseImage(t, "item-00", bson.D{{Key: "stock", Value: int32(9)}})},
	}
	require.NoError(t, f.services.Merge.SavePlan(ctx, p))
	result, err := f.services.WriteBackMerge(ctx, p.ID, "", f.sourceURI, approvalFor(t, p))
	var mismatch *merge.StoredImageError
	require.ErrorAs(t, err, &mismatch)
	require.Equal(t, "item-00", mismatch.DocumentID)
	require.Nil(t, result)
	require.Equal(t, before, readSourceCase(t, f.source.Collection("catalog"), "item-00"))
	saved := f.plan(t, p)
	require.Equal(t, merge.StatusPending, saved.Status)
	require.Nil(t, saved.WriteBack)
	require.Nil(t, saved.AppliedAt)
}
