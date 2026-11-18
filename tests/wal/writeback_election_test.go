package wal_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func electionFixture(t *testing.T) (*followFixture, *merge.Plan) {
	t.Helper()
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, reproSourceURI(t))
	sourceID, err := source.Identify(t.Context(), f.sourceClient)
	require.NoError(t, err)
	metaID, err := source.Identify(t.Context(), f.services.Client)
	require.NoError(t, err)
	if sourceID.ReplicaSetID == "" || metaID.ReplicaSetID == "" || sourceID.ReplicaSetID == metaID.ReplicaSetID {
		t.Skip("election recovery needs two independently identified replica sets")
	}
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "shipment", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, branch.Name)
	require.NoError(t, err)
	_, err = writer.PutMany(t.Context(), "catalog", []bson.M{{"_id": "parcel-a"}, {"_id": "parcel-b"}})
	require.NoError(t, err)
	plan := f.preview(t, branch.ID)
	require.Len(t, plan.Changes, 2)
	return f, plan
}

// Step down only an isolated, single-member primary. Its next election
// changes electionId, even though the elected member has the same address.
func singleMemberStepDown(t *testing.T, client *mongo.Client) func() {
	t.Helper()
	admin := client.Database("admin")
	var before struct {
		Hosts      []string           `bson:"hosts"`
		Passives   []string           `bson:"passives"`
		Arbiters   []string           `bson:"arbiters"`
		ElectionID primitive.ObjectID `bson:"electionId"`
	}
	hello := bson.D{{Key: "hello", Value: 1}}
	require.NoError(t, admin.RunCommand(t.Context(), hello).Decode(&before))
	if len(before.Hosts) != 1 || len(before.Passives)+len(before.Arbiters) != 0 || before.ElectionID.IsZero() {
		t.Skip("forced stepdown needs a single-member replica set with a primary")
	}
	return func() {
		command := bson.D{{Key: "replSetStepDown", Value: 3}, {Key: "force", Value: true}}
		err := admin.RunCommand(t.Context(), command).Err()
		var refusal mongo.CommandError
		if errors.As(err, &refusal) {
			t.Skipf("server refused forced single-member stepdown: %v", err)
		}
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var after struct {
				Writable   bool               `bson:"isWritablePrimary"`
				ElectionID primitive.ObjectID `bson:"electionId"`
			}
			require.Eventually(t, func() bool {
				return admin.RunCommand(ctx, hello).Decode(&after) == nil && after.Writable && after.ElectionID != before.ElectionID
			}, 15*time.Second, 100*time.Millisecond, "the member must elect a primary again")
		})
	}
}

func electionClient(t *testing.T, uri string, monitor *event.CommandMonitor) *mongo.Client {
	t.Helper()
	client, err := mongo.Connect(t.Context(), options.Client().ApplyURI(uri).
		SetMonitor(monitor).SetServerSelectionTimeout(500*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	return client
}

func parcelCount(t *testing.T, f *followFixture) int64 {
	t.Helper()
	count, err := f.source.Collection("catalog").CountDocuments(t.Context(), bson.M{"_id": bson.M{"$in": bson.A{"parcel-a", "parcel-b"}}})
	require.NoError(t, err)
	return count
}

func TestWriteBack_SourceElectionAbortsOpenTransaction(t *testing.T) {
	f, plan := electionFixture(t)
	stepDown := singleMemberStepDown(t, f.sourceClient)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var firstInsert sync.Once
	var steppedDown bool
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName == "insert" {
			firstInsert.Do(func() {
				stepDown()
				steppedDown = true
				// End driver retries while the primary is still unavailable.
				time.AfterFunc(time.Second, cancel)
			})
		}
	}}
	client := electionClient(t, f.sourceURI, monitor)
	_, err := f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: client.Database(f.source.Name())})
	require.True(t, steppedDown)
	require.Error(t, err)
	record := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, record.Status)
	require.Nil(t, record.WriteBack)
	require.Zero(t, parcelCount(t, f), "neither insert committed")
	result, err := f.services.WriteBackMerge(t.Context(), plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Equal(t, 2, result.Applied)
	_, err = f.services.WriteBackMerge(t.Context(), plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "not pending")
	require.EqualValues(t, 2, parcelCount(t, f))
}

func TestWriteBack_MetadataElectionAfterSourceCommit(t *testing.T) {
	f, plan := electionFixture(t)
	stepDown := singleMemberStepDown(t, f.services.Client)
	var steppedDown bool
	sourceClient := electionClient(t, f.sourceURI, &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		if e.CommandName == "commitTransaction" {
			stepDown()
			steppedDown = true
		}
	}})
	meta := electionClient(t, metadataDeploymentURI, nil)
	writer := merge.NewService(meta.Database(f.metadataName), f.services.WAL, f.services.Branches, f.services.Materializer, meta)
	result, err := writer.ApplyToSource(t.Context(), plan.ID, "", merge.WriteBackTarget{Database: sourceClient.Database(f.source.Name())})
	require.True(t, steppedDown)
	require.ErrorContains(t, err, "could not be marked applied")
	require.Equal(t, 2, result.Applied)
	require.EqualValues(t, 2, parcelCount(t, f))
	record := f.plan(t, plan)
	require.Equal(t, merge.StatusWritingBack, record.Status)
	require.NotNil(t, record.WriteBack)
	require.Nil(t, record.WriteBack.CommittedAt)
	_, err = f.services.WriteBackMerge(t.Context(), plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "will not be written again")

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	settled := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, settled.Status)
	require.Equal(t, record.WriteBack.Session, settled.WriteBack.Session)
	require.Nil(t, settled.WriteBack.CommittedAt)
	require.NotNil(t, settled.WriteBack.SettledAt)
	_, err = f.services.WriteBackMerge(t.Context(), plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "not pending")
	entries := f.followedEntries(t, plan.TargetHead)
	require.Len(t, entries, 2)
	for _, entry := range entries {
		require.Equal(t, "merge:"+plan.ID.Hex(), entry.TxnID)
	}
	require.EqualValues(t, 2, parcelCount(t, f))
}
