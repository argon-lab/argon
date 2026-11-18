package wal_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const reproTxnDocs = 450 // more than two change-stream batches of 200

// reproOplogTop is the timestamp of the newest oplog entry on the source
// deployment.
func (f *followFixture) reproOplogTop(t *testing.T) primitive.Timestamp {
	t.Helper()
	raw, err := f.sourceClient.Database("local").Collection("oplog.rs").
		FindOne(context.Background(), bson.D{}, options.FindOne().SetSort(bson.D{{Key: "$natural", Value: -1}})).Raw()
	require.NoError(t, err)
	seconds, ordinal, ok := raw.Lookup("ts").TimestampOK()
	require.True(t, ok)
	return primitive.Timestamp{T: seconds, I: ordinal}
}

// reproCommitTxn inserts reproTxnDocs documents into one collection in a
// single transaction and returns its commit cluster time.
func (f *followFixture) reproCommitTxn(t *testing.T, collection string) primitive.Timestamp {
	t.Helper()
	ctx := context.Background()
	session, err := f.sourceClient.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		docs := make([]interface{}, 0, reproTxnDocs)
		for i := 0; i < reproTxnDocs; i++ {
			docs = append(docs, bson.D{{Key: "_id", Value: fmt.Sprintf("%s-%04d", collection, i)}, {Key: "n", Value: int32(i)}})
		}
		_, err := f.source.Collection(collection).InsertMany(sc, docs)
		return nil, err
	})
	require.NoError(t, err)
	at := session.OperationTime()
	require.NotNil(t, at)
	return *at
}

// reproVisible counts the entries of one collection under main's head.
func (f *followFixture) reproVisible(t *testing.T, after int64, collection string) int {
	t.Helper()
	count := 0
	for _, entry := range f.followedEntries(t, after) {
		if entry.Collection == collection {
			count++
		}
	}
	return count
}

func reproSourceURI(t *testing.T) string {
	uri := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if uri == "" {
		t.Skip("needs a separate source deployment (ARGON_TEST_SOURCE_MONGODB_URI)")
	}
	return uri
}

// A transaction's events share one cluster time. While the source's oplog
// has nothing after that time the cluster time is still open, and main's head
// must not include any of it. Once the oplog moves on, all of it appears in
// one head move.
func TestFollow_DoesNotPublishAnOpenClusterTime(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	result := f.importOnline(t, walcli.OnlineImport{})
	f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)

	quietAttempts := 0
	for attempt := 0; attempt < 6 && quietAttempts < 2; attempt++ {
		collection := fmt.Sprintf("bulk_%d", attempt)
		txnTime := f.reproCommitTxn(t, collection)

		// Observe without ticking. The head is read before the oplog, and
		// the oplog only grows: if the oplog has nothing after the
		// transaction when read second, it had nothing when the head moved.
		quietSamples, published := 0, false
		for deadline := time.Now().Add(2500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
			visible := f.reproVisible(t, result.ConsistentLSN, collection)
			top := f.reproOplogTop(t)
			open := !top.After(txnTime)
			require.Falsef(t, visible > 0 && visible < reproTxnDocs,
				"attempt %d: main's head is inside the transaction: %d of %d entries visible", attempt, visible, reproTxnDocs)
			require.Falsef(t, visible > 0 && open,
				"attempt %d: %d entries of the transaction at %v were published while the source oplog still ended at %v (cluster time still open)",
				attempt, visible, txnTime, top)
			if visible == reproTxnDocs {
				published = true // other traffic on the deployment closed the cluster time
				break
			}
			if open {
				quietSamples++
			}
		}
		if !published && quietSamples >= 40 {
			quietAttempts++
		}
		t.Logf("attempt %d: quiet samples with the cluster time open and nothing published: %d (published early by other traffic: %v)", attempt, quietSamples, published)

		// Now close the cluster time.
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(15 * time.Millisecond) {
			require.True(t, time.Now().Before(deadline), "the transaction never reached main after a tick")
			f.tick(t)
			visible := f.reproVisible(t, result.ConsistentLSN, collection)
			require.Falsef(t, visible > 0 && visible < reproTxnDocs,
				"attempt %d: main's head is inside the transaction: %d of %d entries visible", attempt, visible, reproTxnDocs)
			if visible == reproTxnDocs {
				break
			}
		}
	}
	if quietAttempts == 0 {
		t.Skip("the source deployment's oplog never stayed quiet after a transaction; nothing could be observed")
	}
	f.waitCaughtUp(t)
	f.requireMainEqualsSource(t, 0, nil, "after the transactions")
}

// Stopping the follower while a cluster time is open does not publish it;
// the next follower reads it again and stores it exactly once.
func TestFollow_StopWithOpenClusterTimeThenRestart(t *testing.T) {
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	result := f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)

	observed := false
	for attempt := 0; attempt < 6 && !observed; attempt++ {
		if attempt > 0 {
			stop = f.follow(t, f.restart(t), walcli.FollowSourceOptions{})
			f.waitCaughtUp(t)
		}
		collection := fmt.Sprintf("bulk_%d", attempt)
		txnTime := f.reproCommitTxn(t, collection)
		time.Sleep(1500 * time.Millisecond) // the follower has read the events by now
		require.NoError(t, stop())
		visible := f.reproVisible(t, result.ConsistentLSN, collection)
		top := f.reproOplogTop(t)
		open := !top.After(txnTime)
		require.Falsef(t, visible > 0 && visible < reproTxnDocs, "stopped with the head inside the transaction: %d of %d", visible, reproTxnDocs)
		require.Falsef(t, visible > 0 && open,
			"%d entries of the transaction at %v were published by the time the follower stopped, while the source oplog still ended at %v", visible, txnTime, top)
		observed = open && visible == 0
		t.Logf("attempt %d: stopped with the cluster time open=%v, visible=%d", attempt, open, visible)
	}

	stopRestarted := f.follow(t, f.restart(t), walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stopRestarted())
	counts := map[string]int{}
	for _, entry := range f.followedEntries(t, result.ConsistentLSN) {
		counts[entry.Collection]++
	}
	for collection, count := range counts {
		require.Equalf(t, reproTxnDocs, count, "every insert of %s is in main exactly once", collection)
	}
	f.requireMainEqualsSource(t, 0, nil, "after the restart")
	if !observed {
		t.Skip("the source deployment's oplog never stayed quiet after a transaction; the open state could not be observed")
	}
}
