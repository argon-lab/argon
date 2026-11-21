package wal_test

import (
	"os"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoDB Atlas as the followed source. Each variable names an Atlas cluster
// to run against: ARGON_TEST_ATLAS_URI for one, or the shared-tier and
// dedicated-tier variables for both. Without them these tests skip; CI does
// not hold Atlas credentials.
func atlasSources(t *testing.T) map[string]string {
	t.Helper()
	sources := map[string]string{}
	for name, env := range map[string]string{"atlas": "ARGON_TEST_ATLAS_URI", "atlas shared tier": "ARGON_TEST_ATLAS_SHARED_URI", "atlas dedicated tier": "ARGON_TEST_ATLAS_DEDICATED_URI"} {
		if uri := os.Getenv(env); uri != "" {
			sources[name] = uri
		}
	}
	if len(sources) == 0 {
		t.Skip("set ARGON_TEST_ATLAS_URI, ARGON_TEST_ATLAS_SHARED_URI or ARGON_TEST_ATLAS_DEDICATED_URI to test against MongoDB Atlas")
	}
	return sources
}

// The whole path against Atlas: online import, following, a sandbox with the
// source's indexes, a write-back and the unique-key check. On a shared tier
// (M0, Flex) the replica-set ID cannot be read and the binding rests on the
// verified TLS connection instead; on a dedicated tier it records the ID.
func TestSourceAtlas_ImportFollowSandboxWriteBack(t *testing.T) {
	for name, uri := range atlasSources(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := newFollowFixtureAt(t, metadataDeploymentURI, uri)
			orders := f.source.Collection("orders")
			docs := make([]interface{}, 50)
			for i := range docs {
				docs[i] = bson.D{{Key: "_id", Value: i}, {Key: "sku", Value: "sku-" + string(rune('a'+i%26)) + string(rune('a'+i/26))}, {Key: "status", Value: "open"}}
			}
			_, err := orders.InsertMany(ctx, docs)
			require.NoError(t, err)
			_, err = orders.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "sku", Value: 1}}, Options: options.Index().SetUnique(true)})
			require.NoError(t, err)

			f.importOnline(t, walcli.OnlineImport{})
			main := f.main(t)
			binding, err := f.services.Sources.Get(ctx, main.ID)
			require.NoError(t, err)
			require.True(t, binding.ReplicaSetID != "" || binding.AtlasTLS, "an Atlas source can always be written back to: %+v", binding)
			t.Logf("binding identity: %s", binding.Strength())

			stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
			_, err = orders.UpdateOne(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"status": "paid"}})
			require.NoError(t, err)
			f.waitCaughtUp(t)
			require.NoError(t, stop())

			info, sandboxDB := f.agentSandbox(t, "atlas-agent")
			require.Contains(t, physicalIndexes(t, f.services, info.PhysicalDB, "orders"), "sku_1")
			_, err = sandboxDB.Collection("orders").UpdateOne(ctx, bson.M{"_id": 2}, bson.M{"$set": bson.M{"status": "refunded"}})
			require.NoError(t, err)
			plan := f.preview(t, info.BranchID)
			require.Len(t, plan.Changes, 1)

			t.Setenv("MONGODB_URI", metadataDeploymentURI)
			t.Setenv("ARGON_METADATA_DB", f.metadataName)
			report, err := walcli.CheckMergeUnique(ctx, plan.ID, "", uri)
			require.NoError(t, err)
			require.Empty(t, report.Collisions)

			result, err := f.services.WriteBackMerge(ctx, plan.ID, "", uri, approvalFor(t, plan))
			require.NoError(t, err)
			require.Equal(t, 1, result.Applied)
			var written bson.M
			require.NoError(t, orders.FindOne(ctx, bson.M{"_id": 2}).Decode(&written))
			require.Equal(t, "refunded", written["status"])
		})
	}
}

// A connection that weakens TLS is not accepted for an Atlas binding without
// an ID, even with the right cluster and credentials.
func TestSourceAtlas_IdentifyRequiresVerifiedTLS(t *testing.T) {
	for name, uri := range atlasSources(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			verified, err := source.Connect(ctx, uri)
			require.NoError(t, err)
			defer func() { _ = verified.Disconnect(ctx) }()
			id, err := source.IdentifyAt(ctx, verified, uri)
			require.NoError(t, err)
			require.True(t, id.VerifiedAtlas, "the Atlas connection string verifies TLS")

			weakened := uri + "&tlsInsecure=true"
			if !containsQuery(uri) {
				weakened = uri + "?tlsInsecure=true"
			}
			insecure, err := source.Connect(ctx, weakened)
			require.NoError(t, err)
			defer func() { _ = insecure.Disconnect(ctx) }()
			id, err = source.IdentifyAt(ctx, insecure, weakened)
			require.NoError(t, err)
			require.False(t, id.VerifiedAtlas)
			binding := &source.Binding{SetName: id.SetName, Hosts: id.Hosts, AtlasTLS: true, Display: "atlas"}
			require.Error(t, binding.Verify(id))
		})
	}
}

func containsQuery(uri string) bool { return strings.Contains(uri, "?") }
