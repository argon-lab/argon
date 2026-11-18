package wal_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/sandbox"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// agentSandbox forks a sandbox from the followed main, with capture running,
// the way the managed servers hand one to an agent.
func (f *followFixture) agentSandbox(t *testing.T, name string) (*sandbox.Info, *mongo.Database) {
	t.Helper()
	ctx := context.Background()
	main := f.main(t)
	info, err := f.services.Sandbox.Create(ctx, main.ProjectID, main.ID, name, time.Hour)
	require.NoError(t, err)
	require.NoError(t, f.services.StartCapture(ctx, info.BranchID, "agent:"+name))
	return info, f.services.Client.Database(info.PhysicalDB)
}

// preview drains the sandbox's capture and persists a plan for it.
func (f *followFixture) preview(t *testing.T, branchID string) *merge.Plan {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.services.SyncBranch(ctx, branchID))
	plan, err := f.services.Merge.Preview(ctx, branchID)
	require.NoError(t, err)
	return plan
}

func (f *followFixture) plan(t *testing.T, plan *merge.Plan) *merge.Plan {
	t.Helper()
	fresh, err := f.services.Merge.GetPlan(context.Background(), plan.ID)
	require.NoError(t, err)
	return fresh
}

// agentEdits makes the four kinds of change a plan can carry.
func agentEdits(t *testing.T, db *mongo.Database) {
	t.Helper()
	ctx := context.Background()
	catalog := db.Collection("catalog")
	_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"stock": int32(0), "note": "sold out"}})
	require.NoError(t, err)
	_, err = catalog.ReplaceOne(ctx, bson.M{"_id": "item-02"}, bson.D{{Key: "sku", Value: "B-2"}, {Key: "name", Value: "Item 2, relabelled"}})
	require.NoError(t, err)
	_, err = catalog.DeleteOne(ctx, bson.M{"_id": "item-03"})
	require.NoError(t, err)
	_, err = catalog.InsertOne(ctx, bson.D{{Key: "_id", Value: "item-new"}, {Key: "zeta", Value: 1}, {Key: "alpha", Value: 2}})
	require.NoError(t, err)
}

// A reviewed plan is written to the source in one transaction, main receives
// it through following like any other change to the source, and the plan and
// main's history both say where the change came from.
func TestWriteBack_ReviewedPlanReachesTheSource(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		f.importOnline(t, walcli.OnlineImport{})
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		info, sandboxDB := f.agentSandbox(t, "agent-run")
		agentEdits(t, sandboxDB)
		plan := f.preview(t, info.BranchID)
		require.Len(t, plan.Changes, 4)
		require.Empty(t, plan.Conflicts)

		// Production keeps writing other documents. Main's head moves on,
		// which would make the plan stale for an ordinary merge; write-back
		// compares the documents it is about to change instead.
		catalog := f.source.Collection("catalog")
		for i := 10; i < 15; i++ {
			_, err := catalog.UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$inc": bson.M{"stock": -1}})
			require.NoError(t, err)
		}
		f.waitCaughtUp(t)
		require.Greater(t, f.main(t).HeadLSN, plan.TargetHead)

		result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
		require.NoError(t, err)
		require.Equal(t, 4, result.Applied)

		// The source holds what the agent's sandbox held. Fields production
		// already had keep production's order; what the agent added follows.
		order := func(db *mongo.Database, id string) (bson.M, []string) {
			raw, err := db.Collection("catalog").FindOne(ctx, bson.M{"_id": id}).Raw()
			require.NoError(t, err)
			var doc bson.M
			require.NoError(t, bson.Unmarshal(raw, &doc))
			elements, err := raw.Elements()
			require.NoError(t, err)
			keys := make([]string, len(elements))
			for i, element := range elements {
				keys[i] = element.Key()
			}
			return doc, keys
		}
		for id, wantOrder := range map[string][]string{
			"item-01":  {"_id", "name", "stock", "note"},
			"item-02":  {"_id", "name", "sku"},
			"item-new": {"_id", "zeta", "alpha"},
		} {
			want, _ := order(sandboxDB, id)
			got, keys := order(f.source, id)
			require.Equal(t, want, got, "%s holds the sandbox's values", id)
			require.Equal(t, wantOrder, keys, "%s: field order", id)
		}
		require.ErrorIs(t, catalog.FindOne(ctx, bson.M{"_id": "item-03"}).Err(), mongo.ErrNoDocuments)

		// Main was not appended to by the write-back. It gets there by
		// following.
		f.waitCaughtUp(t)
		f.requireMainEqualsSource(t, 0, nil, "after the write-back was followed")
		applied := f.plan(t, plan)
		require.Equal(t, merge.StatusApplied, applied.Status)
		require.NotNil(t, applied.WriteBack)
		require.NotNil(t, applied.WriteBack.CommittedAt, "the writing process recorded the commit")
		require.NotNil(t, applied.WriteBack.SettledAt, "the follower saw the transaction in the stream")
		require.Equal(t, f.source.Name(), applied.WriteBack.Database)

		main := f.main(t)
		entries, err := f.services.WAL.GetEntries(bson.M{"branch_id": main.ID, "txn_id": "merge:" + plan.ID.Hex()}, options.Find().SetSort(bson.M{"lsn": 1}))
		require.NoError(t, err)
		require.Len(t, entries, 5, "four documents and the merge record")
		for _, entry := range entries[:4] {
			require.Equal(t, "merge:agent-run", entry.Actor)
		}
		require.Equal(t, wal.OpMerge, entries[4].Operation)
		require.Equal(t, true, entries[4].Metadata["write_back"])

		// Nothing is left to merge, and the plan cannot be written again.
		again := f.preview(t, info.BranchID)
		require.Empty(t, again.Changes)
		require.Empty(t, again.Conflicts)
		_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
		require.ErrorContains(t, err, "not pending")
		require.NoError(t, stop())
	})
}

// One document of the plan changed in the source after the review. The whole
// write-back is refused and the source is exactly as it was: the documents
// before the conflicting one in the plan were not written either.
func TestWriteBack_ConflictAbortsTheWholeBatch(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		f.importOnline(t, walcli.OnlineImport{})
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		info, sandboxDB := f.agentSandbox(t, "agent-run")
		agentEdits(t, sandboxDB)
		plan := f.preview(t, info.BranchID)
		require.Len(t, plan.Changes, 4)

		catalog := f.source.Collection("catalog")
		conflicts := []struct {
			name  string
			cause func()
		}{
			{"production created a document the plan inserts", func() {
				// item-new sorts last in the plan, so three documents are
				// compared and written inside the transaction before this
				// one is reached.
				_, err := catalog.InsertOne(ctx, bson.M{"_id": "item-new", "made_by": "production"})
				require.NoError(t, err)
			}},
			{"production deleted a document the plan replaces", func() {
				_, err := catalog.DeleteOne(ctx, bson.M{"_id": "item-new"})
				require.NoError(t, err)
				_, err = catalog.DeleteOne(ctx, bson.M{"_id": "item-02"})
				require.NoError(t, err)
			}},
		}
		for _, conflict := range conflicts {
			conflict.cause()
			before := f.sourceState(t, nil)
			_, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
			var stale *merge.ConflictError
			require.True(t, errors.As(err, &stale), "%s: want a conflict, got %v", conflict.name, err)
			require.Contains(t, err.Error(), "nothing was written")
			require.Equal(t, before, f.sourceState(t, nil), "%s: the source must be untouched", conflict.name)
			aborted := f.plan(t, plan)
			require.Equal(t, merge.StatusPending, aborted.Status, "%s: an aborted plan is pending again", conflict.name)
			require.Nil(t, aborted.WriteBack)
			require.Contains(t, aborted.WriteBackLastError, "nothing was written")
		}

		// Following brings the production change into main; a fresh preview
		// then shows it as the conflict it is, and the reviewer decides.
		f.waitCaughtUp(t)
		reviewed := f.preview(t, info.BranchID)
		require.Len(t, reviewed.Conflicts, 1)
		require.Equal(t, wal.DocumentIDString("item-02"), reviewed.Conflicts[0].DocumentID)
		_, err := f.services.WriteBackMerge(ctx, reviewed.ID, "", f.sourceURI)
		require.ErrorContains(t, err, "conflict")
		result, err := f.services.WriteBackMerge(ctx, reviewed.ID, merge.StrategyOurs, f.sourceURI)
		require.NoError(t, err)
		require.Equal(t, 3, result.Applied)
		require.Equal(t, 1, result.ConflictsResolved)
		require.ErrorIs(t, catalog.FindOne(ctx, bson.M{"_id": "item-02"}).Err(), mongo.ErrNoDocuments, "ours: production's delete stands")
		f.waitCaughtUp(t)
		f.requireMainEqualsSource(t, 0, nil, "after the resolved write-back")
		require.NoError(t, stop())
	})
}

// Write-back happens only when a call asks for it by name. Every other way
// of applying a plan to a followed branch is refused and changes nothing.
func TestWriteBack_OnlyWhenAskedFor(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	source, head := f.sourceState(t, nil), f.main(t).HeadLSN

	unchanged := func(label string) {
		t.Helper()
		require.Equal(t, source, f.sourceState(t, nil), "%s: the source must be untouched", label)
		require.Equal(t, head, f.main(t).HeadLSN, "%s: main must be untouched", label)
		require.Equal(t, merge.StatusPending, f.plan(t, plan).Status, "%s: the plan stays pending", label)
	}

	// The ordinary apply, which the CLI, the REST API, the SDKs and the MCP
	// server all call, never reaches the source and never diverges main.
	_, err := f.services.Merge.Apply(ctx, plan.ID, "")
	require.ErrorContains(t, err, "follows the external database")
	require.ErrorContains(t, err, "--write-back")
	unchanged("ordinary apply")

	// The MCP server offers no way to ask for a write-back.
	c := startMCPOn(t, f.services)
	c.call("initialize", map[string]interface{}{"protocolVersion": "2024-11-05"})
	for _, tool := range c.call("tools/list", nil)["tools"].([]interface{}) {
		definition := tool.(map[string]interface{})
		properties := definition["inputSchema"].(map[string]interface{})["properties"].(map[string]interface{})
		for _, forbidden := range []string{"write_back", "writeback", "uri", "source_uri"} {
			require.NotContains(t, properties, forbidden, "tool %s must not take %s", definition["name"], forbidden)
		}
	}
	text, isError := c.toolText("argon_merge_apply", map[string]interface{}{"plan_id": plan.ID.Hex(), "write_back": true, "uri": f.sourceURI})
	require.True(t, isError, "an agent asking for write-back gets an error: %s", text)
	require.Contains(t, text, "follows the external database")
	unchanged("MCP apply")

	// Other writers of a followed branch are refused too.
	writer, err := f.services.WriterFor(f.project, "main")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": "item-00", "stock": int32(-1)})
	require.ErrorContains(t, err, "follows the external database")
	require.ErrorContains(t, f.services.Branches.ResetHead(f.main(t), 1), "follows the external database")
	_, err = f.services.Checkout.Checkout(ctx, f.main(t).ID)
	require.ErrorContains(t, err, "cannot be checked out")
	unchanged("writer, reset and checkout")

	// A write-back needs the connection string on the call: nothing stored
	// can stand in for it.
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", "")
	require.ErrorContains(t, err, "connection string is required")
	unchanged("write-back without a connection string")

	// A plan whose target follows nothing cannot be written back anywhere.
	ordinary, err := f.services.Projects.CreateProject("ordinary-" + f.project)
	require.NoError(t, err)
	ordinaryWriter, err := f.services.WriterFor(ordinary.Name, "main")
	require.NoError(t, err)
	_, err = ordinaryWriter.Put(ctx, "docs", bson.M{"_id": 1})
	require.NoError(t, err)
	_, err = f.services.Branches.CreateBranch(ordinary.ID, "feature", ordinary.MainBranchID)
	require.NoError(t, err)
	featureWriter, err := f.services.WriterFor(ordinary.Name, "feature")
	require.NoError(t, err)
	_, err = featureWriter.Put(ctx, "docs", bson.M{"_id": 2})
	require.NoError(t, err)
	feature, err := f.services.Branches.GetBranch(ordinary.ID, "feature")
	require.NoError(t, err)
	ordinaryPlan, err := f.services.Merge.Preview(ctx, feature.ID)
	require.NoError(t, err)
	_, err = f.services.WriteBackMerge(ctx, ordinaryPlan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "does not follow an external database")
	unchanged("write-back of an ordinary project's plan")
}

// A connection string that leads to a different deployment than the one the
// project follows is refused before anything is read or written there.
func TestWriteBack_RefusesAnotherDeployment(t *testing.T) {
	deployments := sourceDeployments()
	if len(deployments) < 2 {
		t.Skip("set ARGON_TEST_SOURCE_MONGODB_URI to a second replica set")
	}
	ctx := context.Background()
	other := deployments["source on a separate deployment"]
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)

	_, err := f.services.WriteBackMerge(ctx, plan.ID, "", other)
	require.ErrorContains(t, err, "different deployment")
	followErr := f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: other})
	require.ErrorContains(t, followErr, "different deployment")

	otherClient, err := mongo.Connect(ctx, options.Client().ApplyURI(other))
	require.NoError(t, err)
	defer func() { _ = otherClient.Disconnect(ctx) }()
	names, err := otherClient.Database(f.source.Name()).ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	require.Empty(t, names, "nothing may be created on the other deployment")
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
}

// The writing process dies after the source transaction committed and before
// it could record that. The plan is left claimed. It cannot be written a
// second time, and the follower closes it when the transaction arrives in
// the stream.
func TestWriteBack_CommitWithoutRecordIsSettledByFollowing(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)

	// Nothing is following, so only the writing process could have closed
	// the plan. Put it back to what a crash at that moment leaves behind.
	_, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	_, err = plans.UpdateOne(ctx, bson.M{"_id": plan.ID}, bson.M{
		"$set":   bson.M{"status": merge.StatusWritingBack},
		"$unset": bson.M{"applied_at": "", "strategy": "", "write_back.committed_at": ""},
	})
	require.NoError(t, err)
	written := f.sourceState(t, nil)

	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "will not be written again")
	_, err = f.services.Merge.Apply(ctx, plan.ID, "")
	require.Error(t, err)
	require.Equal(t, written, f.sourceState(t, nil))

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	settled := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, settled.Status)
	require.NotNil(t, settled.WriteBack.SettledAt)
	require.Nil(t, settled.WriteBack.CommittedAt)
	require.NotNil(t, settled.AppliedAt)
	f.requireMainEqualsSource(t, 0, nil, "after following settled the write-back")
}

// Main follows a source under production traffic while a sandbox is merged.
// An ordinary merge is refused throughout. A write-back lands among the
// production writes, and main ends up equal to the source with every change
// from either side exactly once.
func TestFollowedMain_SandboxMergeUnderProductionTraffic(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedShop(t)
		f.seedSmall(t)
		f.importOnline(t, walcli.OnlineImport{})
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})

		// Production writes users and orders the whole time; the agent
		// works on the catalog.
		traffic := f.startTraffic(t, 2, 2*time.Millisecond)
		info, sandboxDB := f.agentSandbox(t, "agent-run")
		agentEdits(t, sandboxDB)
		plan := f.preview(t, info.BranchID)
		require.Len(t, plan.Changes, 4)
		time.Sleep(200 * time.Millisecond)

		_, err := f.services.Merge.Apply(ctx, plan.ID, "")
		require.ErrorContains(t, err, "follows the external database")
		result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
		require.NoError(t, err, "production writes to other documents do not make the plan stale")
		require.Equal(t, 4, result.Applied)

		// A second agent's plan touches a document production then changes.
		second, secondDB := f.agentSandbox(t, "agent-two")
		_, err = secondDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-10"}, bson.M{"$set": bson.M{"stock": int32(500)}})
		require.NoError(t, err)
		_, err = secondDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-11"}, bson.M{"$set": bson.M{"stock": int32(500)}})
		require.NoError(t, err)
		secondPlan := f.preview(t, second.BranchID)
		_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-11"}, bson.M{"$set": bson.M{"stock": int32(7)}})
		require.NoError(t, err)
		_, err = f.services.WriteBackMerge(ctx, secondPlan.ID, "", f.sourceURI)
		var stale *merge.ConflictError
		require.True(t, errors.As(err, &stale), "want a conflict, got %v", err)
		var untouched bson.M
		require.NoError(t, f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "item-10"}).Decode(&untouched))
		require.EqualValues(t, 10, untouched["stock"], "the document before the conflict in the plan was not written")

		time.Sleep(200 * time.Millisecond)
		traffic.halt()
		require.Zero(t, traffic.fails.Load())
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		f.requireMainEqualsSource(t, 0, nil, "after traffic and the write-back")
		require.Equal(t, merge.StatusApplied, f.plan(t, plan).Status)
		require.Equal(t, merge.StatusPending, f.plan(t, secondPlan).Status)

		// The first sandbox has nothing left to merge.
		again := f.preview(t, info.BranchID)
		require.Empty(t, again.Changes)
		require.Empty(t, again.Conflicts)
		t.Logf("%d production writes around the merge", traffic.ops.Load())
	})
}
