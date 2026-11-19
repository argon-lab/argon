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
