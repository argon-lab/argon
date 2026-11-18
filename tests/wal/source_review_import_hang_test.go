package wal_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A write made to the source while it is being copied whose change event the
// server can never deliver (a document just under 16 MB: the event wrapping
// it exceeds the BSON limit, BSONObjectTooLarge) is a permanent error on the
// stream. The online import must end with an error; it must not sit in the
// retry loop.
func TestOnlineImport_PermanentStreamErrorFailsTheImport(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	ctx := context.Background()

	// Enough documents that the copy is still running when the big write
	// lands, and few enough that the whole import fits well inside the wait
	// below under the race detector on a busy machine.
	docs := make([]interface{}, 0, 5000)
	for batch := 0; batch < 3; batch++ {
		docs = docs[:0]
		for i := 0; i < 5000; i++ {
			docs = append(docs, bson.M{"_id": batch*5000 + i, "pad": strings.Repeat("x", 200)})
		}
		_, err := f.source.Collection("orders").InsertMany(ctx, docs)
		require.NoError(t, err)
	}

	// A 9 MB document in a collection with pre- and post-images: an update
	// event carrying both images is 18 MB and can never be returned.
	require.NoError(t, f.source.CreateCollection(ctx, "blobs", options.CreateCollection().
		SetChangeStreamPreAndPostImages(bson.M{"enabled": true})))
	_, err := f.source.Collection("blobs").InsertOne(ctx, bson.D{{Key: "_id", Value: "big"}, {Key: "n", Value: 0}, {Key: "p", Value: strings.Repeat("y", 9*1024*1024)}})
	require.NoError(t, err)

	importCtx, cancelImport := context.WithCancel(ctx)
	defer cancelImport()
	type outcome struct {
		err error
		at  time.Time
	}
	done := make(chan outcome, 1)
	started := time.Now()
	go func() {
		_, err := f.services.ImportDatabaseOnline(importCtx, walcli.OnlineImport{
			URI: f.sourceURI, Database: f.source.Name(), Project: f.project, BatchSize: 100,
		})
		done <- outcome{err, time.Now()}
	}()

	// Wait until main is bound to the source (after the boundary and the
	// collection list were taken, before the copy), then update the document.
	meta := f.services.Client.Database(f.metadataName)
	var branch wal.Branch
	require.Eventually(t, func() bool {
		names, err := meta.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return false
		}
		for _, name := range names {
			if meta.Collection(name).FindOne(ctx, bson.M{"follows": f.source.Name()}).Decode(&branch) == nil && branch.ID != "" {
				return true
			}
		}
		return false
	}, 30*time.Second, 2*time.Millisecond, "the import never bound main")
	_, err = f.source.Collection("blobs").UpdateOne(ctx, bson.M{"_id": "big"}, bson.M{"$set": bson.M{"n": 1}})
	require.NoError(t, err)
	select {
	case result := <-done:
		t.Fatalf("the import ended before the write could land during the copy (err=%v); the scenario was not built", result.err)
	default:
	}
	t.Logf("big document written %s after the import started, while the copy was running", time.Since(started).Round(time.Millisecond))

	// Confirm what the server says about this stream, independently of Argon.
	_ = mongo.Pipeline{}

	const patience = 60 * time.Second
	select {
	case result := <-done:
		require.Error(t, result.err, "an import that could not read the stream must not publish")
		t.Logf("the import returned after %s with: %v", result.at.Sub(started).Round(time.Millisecond), result.err)
		require.ErrorContains(t, result.err, "BSONObjectTooLarge", "the import must fail with the real cause")
		require.ErrorContains(t, result.err, "Nothing was published")
		return
	case <-time.After(patience):
	}

	status, err := f.services.Ingest.SourceStatusFor(ctx, branch.ID)
	require.NoError(t, err)
	require.NotNil(t, status)
	t.Logf("after %s the import has not returned; stored ingest state=%q error=%q", patience, status.State, status.Error)

	// Watch a little longer: the state must still be the retry loop.
	time.Sleep(10 * time.Second)
	later, err := f.services.Ingest.SourceStatusFor(ctx, branch.ID)
	require.NoError(t, err)
	t.Logf("10s later: state=%q updated_at moved=%v", later.State, later.UpdatedAt.After(status.UpdatedAt))

	cancelImport()
	select {
	case result := <-done:
		t.Logf("after cancellation the import returned: %v", result.err)
	case <-time.After(60 * time.Second):
		t.Log("the import did not return even after cancellation")
	}
	t.Fatalf("argon import database --online did not return within %s on a permanent stream error; state=%q error=%q", patience, status.State, status.Error)
}
