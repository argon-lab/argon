package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// The engine itself on MongoDB Atlas: its metadata, main's checkout and the
// sandboxes all live on the Atlas cluster, while it follows a source on
// another deployment (the CI replica set). This is the hosted layout, where
// a user's Atlas holds everything Argon keeps and the source is the database
// they import. Atlas as a source is covered by TestSourceAtlas_*.
//
// Runs against every ARGON_TEST_ATLAS_* cluster that is set. Its databases
// on Atlas are argon_fm_<id> and argon_br_<id>; the fixture drops
// them, and the Atlas workflow's cleanup step removes what a killed run left.
func TestEngineAtlas_FollowSandboxWriteBack(t *testing.T) {
	for name, atlas := range atlasSources(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			// argon_fm_<24 hex> is 33 bytes, inside the 38 Atlas shared tiers allow.
			f := newFollowFixtureNamed(t, atlas, metadataDeploymentURI, "argon_fm_")
			orders := f.source.Collection("orders")
			docs := make([]interface{}, 30)
			for i := range docs {
				docs[i] = bson.D{{Key: "_id", Value: i}, {Key: "status", Value: "open"}}
			}
			_, err := orders.InsertMany(ctx, docs)
			require.NoError(t, err)

			f.importOnline(t, walcli.OnlineImport{})
			stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
			started := time.Now()
			_, err = orders.UpdateOne(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"status": "paid"}})
			require.NoError(t, err)
			_, err = orders.InsertOne(ctx, bson.D{{Key: "_id", Value: 100}, {Key: "status", Value: "new"}})
			require.NoError(t, err)
			f.waitCaughtUp(t)
			t.Logf("follow with the engine on Atlas caught up in %v", time.Since(started).Round(time.Millisecond))
			require.NoError(t, stop())

			// A sandbox on Atlas starts from the followed state and is
			// captured through an Atlas change stream. (A followed main
			// itself cannot be checked out.)
			info, sandboxDB := f.agentSandbox(t, "atlas-engine-agent")
			var paid bson.M
			require.NoError(t, sandboxDB.Collection("orders").FindOne(ctx, bson.M{"_id": 1}).Decode(&paid))
			require.Equal(t, "paid", paid["status"])
			count, err := sandboxDB.Collection("orders").CountDocuments(ctx, bson.M{})
			require.NoError(t, err)
			require.EqualValues(t, 31, count)
			_, err = sandboxDB.Collection("orders").UpdateOne(ctx, bson.M{"_id": 2}, bson.M{"$set": bson.M{"status": "refunded"}})
			require.NoError(t, err)
			plan := f.preview(t, info.BranchID)
			require.Len(t, plan.Changes, 1)

			// Write the reviewed change back to the source.
			t.Setenv("MONGODB_URI", atlas)
			t.Setenv("ARGON_METADATA_DB", f.metadataName)
			report, err := walcli.CheckMergeUnique(ctx, plan.ID, "", f.sourceURI)
			require.NoError(t, err)
			require.Empty(t, report.Collisions)
			result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
			require.NoError(t, err)
			require.Equal(t, 1, result.Applied)
			var written bson.M
			require.NoError(t, orders.FindOne(ctx, bson.M{"_id": 2}).Decode(&written))
			require.Equal(t, "refunded", written["status"])
			require.NoError(t, f.services.Sandbox.Discard(ctx, info.BranchID))

			// A restarted engine resumes following from what it stored on Atlas.
			restarted, err := walcli.NewServicesAt(atlas, f.metadataName)
			require.NoError(t, err)
			restarted.SynchronousSnapshots()
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				for _, status := range restarted.Ingest.Statuses() {
					_ = restarted.Ingest.Cancel(cleanup, status.BranchID)
				}
				_ = restarted.WaitAuto(cleanup)
				_ = restarted.Client.Disconnect(cleanup)
			})
			stop = f.follow(t, restarted, walcli.FollowSourceOptions{})
			_, err = orders.UpdateOne(ctx, bson.M{"_id": 3}, bson.M{"$set": bson.M{"status": "shipped"}})
			require.NoError(t, err)
			f.waitCaughtUp(t)
			require.NoError(t, stop())
		})
	}
}
