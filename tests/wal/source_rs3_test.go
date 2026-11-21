package wal_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type rs3Fixture struct {
	*followFixture
	oldPrimary *mongo.Database // Direct connection, including after it steps down.
	oldHost    string
}

func newRS3Fixture(t *testing.T) *rs3Fixture {
	t.Helper()
	uri := os.Getenv("ARGON_TEST_RS3_SOURCE_MONGODB_URI")
	if uri == "" {
		t.Skip("needs ARGON_TEST_RS3_SOURCE_MONGODB_URI")
	}
	f := newFollowFixture(t, uri)
	ctx := t.Context()
	id, err := source.Identify(ctx, f.sourceClient)
	require.NoError(t, err)
	engine, err := source.Identify(ctx, f.services.Client)
	require.NoError(t, err)
	require.NotEmpty(t, id.ReplicaSetID)
	require.NotEmpty(t, engine.ReplicaSetID)
	require.NotEqual(t, engine.ReplicaSetID, id.ReplicaSetID, "stepdown requires a separate source replica set")

	var hello struct {
		Me       string
		Hosts    []string
		Writable bool `bson:"isWritablePrimary"`
	}
	require.NoError(t, f.sourceClient.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello))
	require.True(t, hello.Writable)
	require.Len(t, hello.Hosts, 3)
	require.Contains(t, hello.Hosts, hello.Me)
	direct, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetHosts([]string{hello.Me}).SetDirect(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = direct.Disconnect(context.Background()) })
	return &rs3Fixture{followFixture: f, oldPrimary: direct.Database("admin"), oldHost: hello.Me}
}

func (f *rs3Fixture) stepDown(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	cmd := bson.D{{Key: "replSetStepDown", Value: 30}, {Key: "force", Value: true}}
	require.NoError(t, f.oldPrimary.RunCommand(ctx, cmd).Err())
	var after struct {
		Me       string
		Writable bool `bson:"isWritablePrimary"`
	}
	require.Eventually(t, func() bool {
		err := f.sourceClient.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&after)
		return err == nil && after.Writable && after.Me != "" && after.Me != f.oldHost
	}, 45*time.Second, 100*time.Millisecond, "a different source member must become writable")
	t.Logf("source primary moved from %s to %s", f.oldHost, after.Me)
}

// interruptCommand waits for mongod to receive the specified operation against
// this test's collection. The pause injects no command error. The stepdown is
// what changes the operation's outcome.
func (f *rs3Fixture) interruptCommand(t *testing.T, command string, work func(context.Context) error, whilePaused func()) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	var configured struct{ Count int64 }
	require.NoError(t, f.oldPrimary.RunCommand(ctx, bson.D{
		{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 1}},
		{Key: "data", Value: bson.M{"failCommands": bson.A{command}, "namespace": f.source.Name() + ".catalog",
			"appName": "argon-source", "blockConnection": true, "blockTimeMS": 20000}},
	}).Decode(&configured))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		require.NoError(t, f.oldPrimary.RunCommand(cleanup, bson.D{
			{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"},
		}).Err())
	})
	done := make(chan struct{})
	var workErr error
	go func() {
		defer close(done)
		workErr = work(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	require.NoError(t, f.oldPrimary.RunCommand(ctx, bson.D{
		{Key: "waitForFailPoint", Value: "failCommand"}, {Key: "timesEntered", Value: configured.Count + 1},
		{Key: "maxTimeMS", Value: 30000},
	}).Err(), "the operation must reach %s on the original primary", command)
	whilePaused()
	select {
	case <-done:
		t.Fatalf("operation ended before the stepdown: %v", workErr)
	default:
	}
	f.stepDown(t)
	select {
	case <-done:
		return workErr
	case <-ctx.Done():
		t.Fatal("operation did not report an outcome after stepdown")
		return nil
	}
}

func TestSourceRS3Follow(t *testing.T) {
	f := newRS3Fixture(t)
	f.seedSmall(t)
	imported := f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	catalog := f.source.Collection("catalog")
	insert := func(id string) {
		_, err := catalog.InsertOne(t.Context(), bson.M{"_id": id})
		require.NoError(t, err)
	}

	insert("checkpoint")
	f.waitCaughtUp(t)
	insert("old-primary")
	f.stepDown(t)
	insert("new-primary")
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	var ids []string
	for _, entry := range f.followedEntries(t, imported.ConsistentLSN) {
		ids = append(ids, entry.DocumentID)
	}
	require.Equal(t, []string{"checkpoint", "old-primary", "new-primary"}, ids)
	f.requireMainEqualsSource(t, 0, nil, "after a running follower moved to the new primary")
}

func TestSourceRS3OnlineImport(t *testing.T) {
	f := newRS3Fixture(t)
	catalog := f.source.Collection("catalog")
	// A find's initial batch contains at most 101 documents. The paused
	// getMore proves the copy already opened its cursor on the old primary.
	docs := make([]any, 150)
	for i := range docs {
		docs[i] = bson.D{{Key: "_id", Value: fmt.Sprintf("item-%03d", i)}, {Key: "stock", Value: 1}}
	}
	_, err := catalog.InsertMany(t.Context(), docs)
	require.NoError(t, err)
	opts := walcli.OnlineImport{URI: f.sourceURI, Database: f.source.Name(), Project: f.project, BatchSize: 50}
	var result any
	run := func(ctx context.Context) error {
		var err error
		result, err = f.services.ImportDatabaseOnline(ctx, opts)
		return err
	}
	err = f.interruptCommand(t, "getMore", run, func() {
		// This row was copied in the first batch. A successful first
		// attempt must also replay the change made on the old primary.
		_, err := catalog.UpdateByID(t.Context(), "item-000", bson.M{"$set": bson.M{"stock": 2}})
		require.NoError(t, err)
	})
	if err != nil {
		t.Logf("interrupted import returned %v", err)
		_, lookupErr := f.services.Projects.GetProjectByName(f.project)
		require.ErrorContains(t, lookupErr, "project not found")
		require.NoError(t, run(t.Context()), "retry the same project after the interrupted copy")
	}
	point := result.(*importer.ImportResult)
	require.NotNil(t, point.ConsistentClusterTime)
	f.requireMainEqualsSource(t, point.ConsistentLSN, point.ConsistentClusterTime, "at the published import boundary")
	copied := f.mainState(t, point.ConsistentLSN)["catalog"]
	require.Len(t, copied, len(docs))
	require.EqualValues(t, 2, copied["item-000"]["stock"])
}

func TestSourceRS3WriteBack(t *testing.T) {
	f := newRS3Fixture(t)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.sourceState(t, nil)
	main := f.main(t)
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "shipment", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, branch.Name)
	require.NoError(t, err)
	_, err = writer.Put(t.Context(), "catalog", bson.M{"_id": "incoming", "stock": int32(9)})
	require.NoError(t, err)
	_, deleted, err := writer.Delete(t.Context(), "catalog", "item-00")
	require.NoError(t, err)
	require.True(t, deleted)
	plan := f.preview(t, branch.ID)
	require.Len(t, plan.Changes, 2)
	require.Equal(t, "incoming", plan.Changes[0].DocumentID)
	require.True(t, plan.Changes[1].Delete)
	approved := approvalFor(t, plan)
	var result *merge.ApplyResult
	apply := func(ctx context.Context) error {
		var err error
		result, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approved)
		return err
	}
	err = f.interruptCommand(t, "delete", apply, func() {
		// The insert succeeded inside the open transaction. Readers must
		// still see neither of the two changes before the stepdown.
		require.Equal(t, merge.StatusWritingBack, f.plan(t, plan).Status)
		require.Equal(t, before, f.sourceState(t, nil))
	})
	if err != nil {
		t.Logf("interrupted write-back returned %v", err)
		record := f.plan(t, plan)
		require.Equal(t, merge.StatusPending, record.Status, "a pre-commit failure must release its claim")
		require.NotEmpty(t, record.WriteBackLastError)
		require.Equal(t, before, f.sourceState(t, nil))
		require.NoError(t, apply(t.Context()), "retry only a plan released as pending")
	} else {
		record := f.plan(t, plan).WriteBack
		require.NotNil(t, record)
		require.NotNil(t, record.AfterTxn)
		require.NotNil(t, record.CommitTxn)
		require.Greater(t, *record.CommitTxn, *record.AfterTxn+1, "success after stepdown must come from a fresh transaction")
		t.Logf("source committed transaction %d after the interrupted attempt %d", *record.CommitTxn, *record.AfterTxn+1)
	}
	require.Equal(t, 2, result.Applied)
	require.Equal(t, merge.StatusApplied, f.plan(t, plan).Status)
	require.ErrorContains(t, apply(t.Context()), "not pending")
	want := before
	delete(want["catalog"], "item-00")
	want["catalog"]["incoming"] = bson.M{"_id": "incoming", "stock": int32(9)}
	require.Equal(t, want, f.sourceState(t, nil))

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	settled := f.plan(t, plan)
	require.NotNil(t, settled.WriteBack)
	require.NotNil(t, settled.WriteBack.SettledAt)
	entries := f.followedEntries(t, plan.TargetHead)
	require.Len(t, entries, 2)
	for i, entry := range entries {
		require.Equal(t, "merge:"+plan.ID.Hex(), entry.TxnID)
		require.Equal(t, plan.Changes[i].DocumentID, entry.DocumentID)
	}
	require.Equal(t, want, f.mainState(t, 0))
}
