package wal_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Resync repairs, in place, a project whose follower has ended as degraded.
// Every test here runs against real replica sets and checks the result the
// same way the import's tests do: main at the reported LSN is compared,
// document by document, with the source read back at the reported cluster
// time.

func (f *followFixture) resync(t *testing.T, services *walcli.Services, opts walcli.ResyncSourceOptions) *walcli.ResyncInfo {
	t.Helper()
	opts.URI = f.sourceURI
	info, err := services.ResyncSource(context.Background(), f.project, opts)
	require.NoError(t, err)
	return info
}

func (f *followFixture) status(t *testing.T) *walcli.SourceInfo {
	t.Helper()
	info, err := f.services.SourceStatus(context.Background(), f.project)
	require.NoError(t, err)
	return info
}

// degrade follows until the follower ends by itself and checks that it ended
// as degraded for the expected reason.
func (f *followFixture) degrade(t *testing.T, reason string) *walcli.SourceInfo {
	t.Helper()
	require.Error(t, f.followUntilItStops(t, walcli.FollowSourceOptions{}))
	info := f.status(t)
	require.Equal(t, "degraded", info.State)
	require.Contains(t, info.Error, reason)
	return info
}

// requireResynced checks what every finished resync must leave behind: main
// at the reported LSN is the source at the reported cluster time, the status
// is exact and followable, and the LSNs the resync wrote are one recorded
// inexact range that starts right after the last consistent point.
func (f *followFixture) requireResynced(t *testing.T, before *walcli.SourceInfo, result *walcli.ResyncInfo) *walcli.SourceInfo {
	t.Helper()
	at := primitive.Timestamp{T: result.ConsistentTime.T, I: result.ConsistentTime.I}
	f.requireMainEqualsSource(t, result.ConsistentLSN, &at, "at the resync's consistent point")

	after := f.status(t)
	require.Equal(t, "stopped", after.State)
	require.Empty(t, after.Error)
	require.Equal(t, "exact", after.Mode)
	require.False(t, after.ResyncUnfinished)
	require.Zero(t, after.InexactOpenFromLSN)
	require.Equal(t, result.ConsistentLSN, after.ConsistentLSN)
	require.Equal(t, result.ConsistentTime, *after.ConsistentTime)

	require.Equal(t, before.HeadLSN+1, result.InexactFromLSN, "the inexact range starts with the first entry after the last consistent state")
	require.Len(t, after.InexactRanges, len(before.InexactRanges)+1)
	recorded := after.InexactRanges[len(after.InexactRanges)-1]
	require.Equal(t, result.InexactFromLSN, recorded.FromLSN)
	require.Equal(t, result.ConsistentLSN, recorded.ToLSN)
	require.Contains(t, recorded.Reason, "resync")
	require.Equal(t, result.ConsistentTime, recorded.ConvergedAt)
	return after
}

// A collection is dropped on a source that is written throughout, before,
// during and after the resync. The follower ends at the drop. Resync brings
// main to the source at one cluster time, with the dropped collection gone,
// and following continues from there.
func TestResync_AfterDroppedCollection(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedShop(t)
		archive := make([]interface{}, 0, 300)
		for i := 0; i < 300; i++ {
			archive = append(archive, bson.D{{Key: "_id", Value: int32(i)}, {Key: "note", Value: fmt.Sprintf("archived %d", i)}})
		}
		_, err := f.source.Collection("archive").InsertMany(ctx, archive)
		require.NoError(t, err)
		imported := f.importOnline(t, walcli.OnlineImport{})

		traffic := f.startTraffic(t, 3, time.Millisecond)
		time.Sleep(200 * time.Millisecond)
		require.NoError(t, f.source.Collection("archive").Drop(ctx))
		time.Sleep(200 * time.Millisecond)
		before := f.degrade(t, "drop of archive")
		require.Contains(t, before.Error, "argon source resync")
		require.Contains(t, f.mainState(t, 0), "archive", "the follower stopped before the drop")

		writesBefore := traffic.ops.Load()
		result := f.resync(t, f.services, walcli.ResyncSourceOptions{BatchSize: 100})
		require.Greater(t, traffic.ops.Load()-writesBefore, int64(50), "the source must actually be written while it is resynced")
		f.requireResynced(t, before, result)

		require.NotContains(t, f.mainState(t, result.ConsistentLSN), "archive")
		require.Equal(t, []string{"archive"}, result.RemovedCollections)
		require.GreaterOrEqual(t, result.Deletes, int64(300))
		require.Greater(t, result.Unchanged, int64(1000), "documents main already held are not stored again")
		// History before the resync is still there to be read.
		require.Len(t, f.mainState(t, imported.ConsistentLSN)["archive"], 300)

		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		time.Sleep(300 * time.Millisecond)
		traffic.halt()
		require.Zero(t, traffic.fails.Load(), "production writes must not fail because of the resync")
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		f.requireMainEqualsSource(t, 0, nil, "after following again")
		t.Logf("resync compared %d documents, %d unchanged, %d put, %d deleted; inexact LSN %d-%d",
			result.Compared, result.Unchanged, result.Puts, result.Deletes, result.InexactFromLSN, result.ConsistentLSN)
	})
}

// A collection is renamed on the source and changed under its new name. The
// source is otherwise quiet, so its own change stream can be the witness
// that the resync wrote nothing to it.
func TestResync_AfterRenamedCollection(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		_, err := f.source.Collection("settings").InsertOne(ctx, bson.M{"_id": "theme", "value": "dark"})
		require.NoError(t, err)
		f.importOnline(t, walcli.OnlineImport{})

		name := f.source.Name()
		require.NoError(t, f.sourceClient.Database("admin").RunCommand(ctx, bson.D{
			{Key: "renameCollection", Value: name + ".catalog"}, {Key: "to", Value: name + ".products"}}).Err())
		products := f.source.Collection("products")
		_, err = products.UpdateOne(ctx, bson.M{"_id": "item-03"}, bson.M{"$set": bson.M{"stock": int32(3)}})
		require.NoError(t, err)
		_, err = products.DeleteOne(ctx, bson.M{"_id": "item-04"})
		require.NoError(t, err)
		_, err = products.InsertOne(ctx, bson.M{"_id": "item-99", "name": "Added after the rename"})
		require.NoError(t, err)
		before := f.degrade(t, "rename of catalog")

		audit := f.audit(t)
		result := f.resync(t, f.services, walcli.ResyncSourceOptions{})
		require.Empty(t, audit.operations(t), "the resync must not write to the source")
		f.requireResynced(t, before, result)

		state := f.mainState(t, 0)
		require.NotContains(t, state, "catalog")
		require.Len(t, state["products"], 20)
		require.EqualValues(t, 3, state["products"][wal.DocumentIDString("item-03")]["stock"])
		require.NotContains(t, state["products"], wal.DocumentIDString("item-04"))
		require.Equal(t, []string{"catalog"}, result.RemovedCollections)
		require.EqualValues(t, 20, result.Deletes, "every document under the old name")
		require.EqualValues(t, 20, result.Puts, "every document under the new name")
		require.EqualValues(t, 1, result.Unchanged, "the collection that was not renamed is left alone")

		// The resync's own entries are attributed to the source.
		for _, entry := range f.followedEntries(t, before.HeadLSN) {
			require.Equal(t, "source", entry.Actor)
		}

		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		_, err = products.UpdateOne(ctx, bson.M{"_id": "item-05"}, bson.M{"$set": bson.M{"stock": int32(5)}})
		require.NoError(t, err)
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		f.requireMainEqualsSource(t, 0, nil, "after following again")
	})
}

// The saved position is one the source's oplog no longer holds. A fresh
// replica set never truncates its oplog (and accepts a position from before
// its oplog begins), so the server's ChangeStreamHistoryLost error is
// supplied when the follower opens its stream, as the unit test of that
// error does. What happened on the source since the last applied change is
// then unknown to the stream, and the resync has to find it by comparing:
// an insert, an update, a delete and a new collection.
func TestResync_AfterLostPosition(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		f.importOnline(t, walcli.OnlineImport{})
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(9)}})
		require.NoError(t, err)
		f.waitCaughtUp(t)
		require.NoError(t, stop())

		catalog := f.source.Collection("catalog")
		_, err = catalog.InsertOne(ctx, bson.M{"_id": "item-50", "name": "Made while nobody followed"})
		require.NoError(t, err)
		_, err = catalog.UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"stock": int32(1)}})
		require.NoError(t, err)
		_, err = catalog.DeleteOne(ctx, bson.M{"_id": "item-02"})
		require.NoError(t, err)
		_, err = f.source.Collection("reviews").InsertOne(ctx, bson.M{"_id": 1, "stars": 5})
		require.NoError(t, err)

		lost := mongo.CommandError{Code: 286, Name: "ChangeStreamHistoryLost",
			Message: "Resume of change stream was not possible, as the resume point may no longer be in the oplog."}
		require.Error(t, f.followUntilItStops(t, walcli.FollowSourceOptions{
			Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				return nil, lost
			}}))
		before := f.status(t)
		require.Equal(t, "degraded", before.State)
		require.Contains(t, before.Error, "capture cannot resume safely")
		require.Contains(t, before.Error, "ChangeStreamHistoryLost")

		audit := f.audit(t)
		result := f.resync(t, f.services, walcli.ResyncSourceOptions{})
		require.Empty(t, audit.operations(t), "the resync must not write to the source")
		f.requireResynced(t, before, result)
		require.EqualValues(t, 3, result.Puts, "the inserted document, the updated one and the new collection's")
		require.EqualValues(t, 1, result.Deletes)
		require.Empty(t, result.RemovedCollections)

		stop = f.follow(t, f.services, walcli.FollowSourceOptions{})
		_, err = catalog.DeleteOne(ctx, bson.M{"_id": "item-50"})
		require.NoError(t, err)
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		f.requireMainEqualsSource(t, 0, nil, "after following again")
	})
}

// The source database itself is dropped and later written again.
func TestResync_AfterDroppedDatabase(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	// The application drops its database. Argon never does.
	require.NoError(t, f.source.Drop(ctx))
	before := f.degrade(t, "catalog")

	result := f.resync(t, f.services, walcli.ResyncSourceOptions{})
	require.Empty(t, f.mainState(t, 0))
	require.EqualValues(t, 20, result.Deletes)
	require.Equal(t, []string{"catalog"}, result.RemovedCollections)
	require.Equal(t, "exact", f.status(t).Mode)
	require.Len(t, f.mainState(t, before.HeadLSN)["catalog"], 20, "the documents are still in main's history")

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	_, err := f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "item-00", "name": "Second life"})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after the database was written again")
}

// A resync dies after it has written part of the difference. Main is then
// neither its old state nor the source's, and nothing may report it as
// healthy: status says the resync is unfinished and that main is catching
// up, the last consistent point is still the old one, and following refuses
// to start. A new process runs resync again and completes it; the inexact
// range covers both attempts.
func TestResync_InterruptedIsNeverReportedHealthy(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	scratch := make([]interface{}, 0, 60)
	for i := 0; i < 60; i++ {
		scratch = append(scratch, bson.M{"_id": int32(i)})
	}
	_, err := f.source.Collection("scratch").InsertMany(ctx, scratch)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	for i := 0; i < 20; i++ {
		_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$set": bson.M{"stock": int32(100 + i)}})
		require.NoError(t, err)
	}
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	// These the follower never sees: it ends at the drop.
	for i := 0; i < 20; i++ {
		_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$set": bson.M{"stock": int32(200 + i)}})
		require.NoError(t, err)
	}
	before := f.degrade(t, "drop of scratch")
	oldState := f.mainState(t, 0)

	// The process is killed after the first batch that wrote something.
	attempt, cancel := context.WithCancel(ctx)
	_, err = f.services.ResyncSource(attempt, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI, BatchSize: 5,
		Progress: func(p walcli.ResyncProgress) {
			if p.Puts > 0 {
				cancel()
			}
		}})
	cancel()
	require.Error(t, err)

	interrupted := f.status(t)
	require.Greater(t, interrupted.HeadLSN, before.HeadLSN, "part of the difference was written")
	half := f.mainState(t, 0)
	require.NotEqual(t, oldState, half)
	require.Contains(t, half, "scratch", "the interrupted resync had not reached the dropped collection")
	require.True(t, interrupted.ResyncUnfinished)
	require.NotNil(t, interrupted.ResyncStartedAt)
	require.Equal(t, "catch_up", interrupted.Mode)
	require.NotEqual(t, "running", interrupted.State)
	require.Equal(t, before.HeadLSN+1, interrupted.InexactOpenFromLSN)
	require.Equal(t, before.ConsistentLSN, interrupted.ConsistentLSN, "the last consistent point is still the one before the resync")
	require.Len(t, interrupted.InexactRanges, len(before.InexactRanges), "an unfinished range is not listed as finished")
	// The state before the resync is intact below the range.
	require.Equal(t, oldState, f.mainState(t, before.HeadLSN))

	// Following cannot paper over it, in this process or a new one.
	restarted := f.restart(t)
	for _, services := range []*walcli.Services{f.services, restarted} {
		err = services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: f.sourceURI, AllowInexact: true})
		require.ErrorContains(t, err, "has not finished")
		require.ErrorContains(t, err, "argon source resync")
	}
	require.True(t, f.status(t).ResyncUnfinished)

	result := f.resync(t, restarted, walcli.ResyncSourceOptions{BatchSize: 5})
	after := f.requireResynced(t, before, result)
	require.Equal(t, before.HeadLSN+1, after.InexactRanges[len(after.InexactRanges)-1].FromLSN)
	require.NotContains(t, f.mainState(t, 0), "scratch")

	stop := f.follow(t, restarted, walcli.FollowSourceOptions{})
	_, err = f.source.Collection("catalog").DeleteOne(ctx, bson.M{"_id": "item-07"})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after following again")
}

// A collection is dropped on the source while the resync is replaying the
// change stream. The resync cannot finish, says so, and leaves main marked
// as unfinished; running it again completes it.
func TestResync_SourceDropsAnotherCollectionMeanwhile(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	for _, name := range []string{"scratch", "scratch2"} {
		_, err := f.source.Collection(name).InsertOne(ctx, bson.M{"_id": 1})
		require.NoError(t, err)
	}
	f.importOnline(t, walcli.OnlineImport{})
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	before := f.degrade(t, "drop of scratch")

	dropped := false
	_, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI,
		Progress: func(p walcli.ResyncProgress) {
			if !dropped {
				dropped = true
				require.NoError(t, f.source.Collection("scratch2").Drop(ctx))
			}
		}})
	require.ErrorContains(t, err, "drop of scratch2")
	failed := f.status(t)
	require.Equal(t, "degraded", failed.State)
	require.True(t, failed.ResyncUnfinished)
	require.Equal(t, "catch_up", failed.Mode)

	result := f.resync(t, f.services, walcli.ResyncSourceOptions{})
	f.requireResynced(t, before, result)
	require.Equal(t, map[string]bool{"catalog": true}, collectionNames(f.mainState(t, 0)))
}

// Resync is for a follower that ended as degraded. On a healthy project it
// refuses, because it would trade exact history for an inexact range; when
// forced it still ends at a state of the source.
func TestResync_RefusesAHealthyProjectUnlessForced(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)

	_, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.ErrorContains(t, err, "--force")
	require.Equal(t, before.HeadLSN, f.status(t).HeadLSN)
	require.False(t, f.status(t).ResyncUnfinished)

	_, err = f.source.Collection("catalog").DeleteOne(ctx, bson.M{"_id": "item-00"})
	require.NoError(t, err)
	result := f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true})
	f.requireResynced(t, before, result)
	require.EqualValues(t, 1, result.Deletes)
	require.Zero(t, result.Puts)

	// A project that follows nothing has nothing to resync.
	_, err = f.services.ResyncSource(ctx, "no-such-project", walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.Error(t, err)
}

// With --auto-resync a running follower repairs a dropped collection itself
// and keeps following.
func TestFollow_AutoResyncAfterDroppedCollection(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	resyncs := make(chan *walcli.ResyncInfo, 4)
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{AutoResync: true, OnResync: func(info *walcli.ResyncInfo) { resyncs <- info }})
	f.waitCaughtUp(t)
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(2)}})
	require.NoError(t, err)

	var result *walcli.ResyncInfo
	deadline := time.After(60 * time.Second)
	for result == nil {
		select {
		case result = <-resyncs:
		case <-deadline:
			t.Fatalf("no automatic resync happened; status: %+v", f.status(t))
		case <-time.After(100 * time.Millisecond):
			f.tick(t)
		}
	}
	require.Equal(t, []string{"scratch"}, result.RemovedCollections)

	// Following is running again: a later change arrives.
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(3)}})
	require.NoError(t, err)
	target := f.sourceTime(t)
	require.Eventually(t, func() bool {
		f.tick(t)
		info := f.status(t)
		return info.State == "running" && info.Mode == "exact" && info.Position != nil &&
			!(primitive.Timestamp{T: info.Position.T, I: info.Position.I}).Before(target)
	}, 60*time.Second, 100*time.Millisecond)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after the automatic resync")
	info := f.status(t)
	require.Len(t, info.InexactRanges, 2, "the import's and the resync's")
	require.True(t, strings.HasPrefix(info.InexactRanges[1].Reason, "resync"))
}

// Garbage collection has reclaimed the early history of main, so the log no
// longer names every document main holds: their state lives in a snapshot.
// The resync must still find the documents the source dropped, and must not
// take a document for unchanged on the word of a log that has holes.
func TestResync_AfterHistoryWasReclaimed(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertMany(ctx, []interface{}{bson.M{"_id": 1}, bson.M{"_id": 2}})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	main := f.main(t)
	_, err = f.services.Snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	_, err = f.services.RunGC(ctx, main.ProjectID, time.Nanosecond, false)
	require.NoError(t, err)
	for _, name := range []string{"catalog", "scratch"} {
		floor, err := f.services.WAL.ReclaimFloor(main.ID, name)
		require.NoError(t, err)
		require.Positive(t, floor, "the test needs %s's history on main to be reclaimed", name)
	}

	catalog := f.source.Collection("catalog")
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	_, err = catalog.DeleteOne(ctx, bson.M{"_id": "item-02"})
	require.NoError(t, err)
	_, err = catalog.UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	before := f.degrade(t, "drop of scratch")

	result := f.resync(t, f.services, walcli.ResyncSourceOptions{BatchSize: 7})
	f.requireResynced(t, before, result)
	require.Equal(t, []string{"scratch"}, result.RemovedCollections)
	require.EqualValues(t, 3, result.Deletes)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	_, err = catalog.DeleteOne(ctx, bson.M{"_id": "item-03"})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after following again")
}

// A resync opens the source's change stream before it writes anything. The
// replay that needs the stream only starts after the whole comparison, so a
// source user without the changeStream privilege would otherwise be found out
// last, with the branch already marked as mid-resync.
func TestResync_StreamThatCannotBeOpenedFailsBeforeAnythingIsWritten(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(9)}})
	require.NoError(t, err)
	head := f.main(t).HeadLSN

	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI, Force: true,
		Watch: func(context.Context, *mongo.Database, *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			return nil, mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized on shop to execute command { aggregate: 1, pipeline: [ { $changeStream: {} } ] }"}
		}})
	require.ErrorContains(t, err, "change stream cannot be opened")
	require.ErrorContains(t, err, "Nothing was written")
	require.Equal(t, head, f.main(t).HeadLSN)
	status, err := f.services.Ingest.SourceStatusFor(ctx, f.main(t).ID)
	require.NoError(t, err)
	require.Nil(t, status.Source.Resync, "no unfinished resync is on record")
	require.Equal(t, "exact", status.Source.Mode)

	// Following is as it was: it picks up from the saved position.
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after the refused resync")
}

// Another process detaches the project just as a resync is about to record
// that it has started. The resync stops there. It does not write to the
// branch, and it does not leave a status document behind for a branch that
// follows nothing.
func TestResync_DetachedJustBeforeItStartsLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	branch := f.main(t)
	other := f.restart(t)

	_, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI, Force: true,
		// The stream check is the last step before the start is recorded.
		Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
			require.NoError(t, other.DetachSource(ctx, f.project))
			return src.Watch(ctx, mongo.Pipeline{}, opts)
		}})
	require.ErrorContains(t, err, "no longer follows")

	after := f.main(t)
	require.Empty(t, after.Follows)
	require.Equal(t, branch.HeadLSN, after.HeadLSN, "nothing was written to the detached branch")
	n, err := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state").CountDocuments(ctx, bson.M{"_id": branch.ID})
	require.NoError(t, err)
	require.Zero(t, n, "the status document detach removed was not brought back")
}
