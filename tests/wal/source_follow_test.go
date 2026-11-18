package wal_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Online import, following and write-back are tested against real replica
// sets. The source is always a database of its own; it lives on the
// deployment that holds the Argon metadata and, when
// ARGON_TEST_SOURCE_MONGODB_URI names a second replica set, on that one too.

const metadataDeploymentURI = "mongodb://localhost:27017"

// sourceDeployments are the deployments a source database is placed on.
func sourceDeployments() map[string]string {
	deployments := map[string]string{"source on the metadata deployment": metadataDeploymentURI}
	if uri := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI"); uri != "" {
		deployments["source on a separate deployment"] = uri
	}
	return deployments
}

// forEachSourceDeployment runs a test once per available deployment.
func forEachSourceDeployment(t *testing.T, test func(t *testing.T, sourceURI string)) {
	for name, uri := range sourceDeployments() {
		t.Run(name, func(t *testing.T) { test(t, uri) })
	}
}

var followFixtureSeq atomic.Int64

// followFixture is the full service stack on a fresh metadata database plus
// a source database that stands in for production.
type followFixture struct {
	services     *walcli.Services
	sourceURI    string
	sourceClient *mongo.Client
	source       *mongo.Database
	project      string
	metadataName string
}

func newFollowFixture(t *testing.T, sourceURI string) *followFixture {
	t.Helper()
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := context.Background()
	id := fmt.Sprintf("%d_%d", time.Now().UnixNano(), followFixtureSeq.Add(1))
	f := &followFixture{sourceURI: sourceURI, project: "shop-" + id, metadataName: "argon_follow_meta_" + id}
	services, err := walcli.NewServicesAt(metadataDeploymentURI, f.metadataName)
	require.NoError(t, err)
	services.SynchronousSnapshots()
	f.services = services

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(sourceURI))
	require.NoError(t, err)
	f.sourceClient = client
	f.source = client.Database("follow_src_" + id)

	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, status := range services.Ingest.Statuses() {
			_ = services.Ingest.Cancel(cleanup, status.BranchID)
		}
		_ = services.WaitAuto(cleanup)
		services.Monitor.Stop()
		if projects, err := services.Projects.ListProjects(); err == nil {
			for _, project := range projects {
				branches, _ := services.Branches.ListBranchesAny(project.ID)
				for _, branch := range branches {
					_ = services.Client.Database("argon_br_" + branch.ID).Drop(cleanup)
				}
			}
		}
		_ = services.Client.Database(f.metadataName).Drop(cleanup)
		_ = services.Client.Disconnect(cleanup)
		// The test owns the source and removes it itself. Argon never does.
		_ = f.source.Drop(cleanup)
		_ = client.Database("follow_tick").Drop(cleanup)
		_ = client.Disconnect(cleanup)
	})
	return f
}

// importOnline imports the source without pausing whatever is writing to it.
func (f *followFixture) importOnline(t *testing.T, opts walcli.OnlineImport) *importer.ImportResult {
	t.Helper()
	opts.URI, opts.Database, opts.Project = f.sourceURI, f.source.Name(), f.project
	result, err := f.services.ImportDatabaseOnline(context.Background(), opts)
	require.NoError(t, err)
	return result.(*importer.ImportResult)
}

func (f *followFixture) main(t *testing.T) *wal.Branch {
	t.Helper()
	project, err := f.services.Projects.GetProjectByName(f.project)
	require.NoError(t, err)
	branch, err := f.services.Branches.GetBranch(project.ID, "main")
	require.NoError(t, err)
	return branch
}

// follow starts following in the background. The returned function stops it
// and reports how it ended.
func (f *followFixture) follow(t *testing.T, services *walcli.Services, opts walcli.FollowSourceOptions) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	opts.URI, opts.Ready = f.sourceURI, ready
	go func() { done <- services.FollowSource(ctx, f.project, opts) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("following ended before the stream opened: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("following never opened the stream")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(60 * time.Second):
				result = fmt.Errorf("following did not stop")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// followUntilItStops follows in the foreground for a case that is expected to
// end by itself. A follower that keeps running instead fails the test.
func (f *followFixture) followUntilItStops(t *testing.T, opts walcli.FollowSourceOptions) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	opts.URI = f.sourceURI
	err := f.services.FollowSource(ctx, f.project, opts)
	if ctx.Err() != nil {
		info, _ := f.services.SourceStatus(context.Background(), f.project)
		t.Fatalf("following was expected to stop on its own but was still running after 45s (last error: %v; status: %+v)", err, info)
	}
	return err
}

// sourceTime is the source deployment's current cluster time.
func (f *followFixture) sourceTime(t *testing.T) primitive.Timestamp {
	t.Helper()
	var reply bson.Raw
	require.NoError(t, f.source.RunCommand(context.Background(), bson.D{{Key: "hello", Value: 1}},
		options.RunCmd().SetReadPreference(readpref.Primary())).Decode(&reply))
	seconds, ordinal, ok := reply.Lookup("operationTime").TimestampOK()
	require.True(t, ok)
	return primitive.Timestamp{T: seconds, I: ordinal}
}

// tick writes to an unrelated database on the source deployment. A follower
// publishes a change once the stream has moved past its cluster time; on a
// deployment nobody else writes to, that would take until MongoDB's next
// periodic no-op. Other traffic on a real deployment has the same effect.
func (f *followFixture) tick(t *testing.T) {
	t.Helper()
	_, err := f.sourceClient.Database("follow_tick").Collection("ticks").InsertOne(context.Background(), bson.M{"at": time.Now()})
	require.NoError(t, err)
}

// waitCaughtUp blocks until main has every change the source had made when
// it was called.
func (f *followFixture) waitCaughtUp(t *testing.T) *walcli.SourceInfo {
	t.Helper()
	target := f.sourceTime(t)
	deadline := time.Now().Add(90 * time.Second)
	var last *walcli.SourceInfo
	for time.Now().Before(deadline) {
		f.tick(t)
		info, err := f.services.SourceStatus(context.Background(), f.project)
		require.NoError(t, err)
		last = info
		if info.State == "degraded" || info.State == "stopped" {
			if info.Owner == nil {
				t.Fatalf("following is %s: %s", info.State, info.Error)
			}
			// A restart can still be replaying from a previously degraded position.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if info.Mode == "exact" && info.Position != nil && !(primitive.Timestamp{T: info.Position.T, I: info.Position.I}).Before(target) {
			return info
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("main did not catch up with the source; last status: %+v", last)
	return nil
}

// sourceState reads the source's ordinary user collections, keyed the way the
// materializer keys a branch. With a cluster time it reads their documents
// as of that time.
func (f *followFixture) sourceState(t *testing.T, at *primitive.Timestamp) map[string]map[string]bson.M {
	t.Helper()
	ctx := context.Background()
	names, err := source.Collections(ctx, f.source)
	require.NoError(t, err)
	state := make(map[string]map[string]bson.M)
	for _, name := range names {
		command := bson.D{{Key: "find", Value: name}, {Key: "readConcern", Value: bson.D{{Key: "level", Value: "majority"}}}}
		if at != nil {
			command = bson.D{{Key: "find", Value: name}, {Key: "readConcern", Value: bson.D{
				{Key: "level", Value: "snapshot"}, {Key: "atClusterTime", Value: *at}}}}
		}
		cursor, err := f.source.RunCommandCursor(ctx, command)
		if err != nil && at != nil {
			// The collection did not exist yet at that cluster time.
			continue
		}
		require.NoError(t, err)
		var docs []bson.M
		require.NoError(t, cursor.All(ctx, &docs))
		if len(docs) == 0 {
			continue
		}
		state[name] = make(map[string]bson.M, len(docs))
		for _, doc := range docs {
			state[name][wal.DocumentIDString(doc["_id"])] = doc
		}
	}
	return state
}

// mainState materializes main at an LSN (0 for its head).
func (f *followFixture) mainState(t *testing.T, lsn int64) map[string]map[string]bson.M {
	t.Helper()
	branch := f.main(t)
	if lsn == 0 {
		lsn = branch.HeadLSN
	}
	state, err := f.services.Materializer.MaterializeBranchAtLSN(branch, lsn)
	require.NoError(t, err)
	for name, docs := range state {
		if len(docs) == 0 {
			delete(state, name)
		}
	}
	return state
}

// requireMainEqualsSource compares main with the source document by document.
func (f *followFixture) requireMainEqualsSource(t *testing.T, lsn int64, at *primitive.Timestamp, label string) {
	t.Helper()
	want, got := f.sourceState(t, at), f.mainState(t, lsn)
	require.Equal(t, collectionNames(want), collectionNames(got), "%s: collections differ", label)
	for name, docs := range want {
		require.Len(t, got[name], len(docs), "%s: %s has a different number of documents", label, name)
		for id, doc := range docs {
			require.Equal(t, doc, got[name][id], "%s: %s/%s differs", label, name, id)
		}
	}
}

func collectionNames(state map[string]map[string]bson.M) map[string]bool {
	names := make(map[string]bool, len(state))
	for name := range state {
		names[name] = true
	}
	return names
}

// productionTraffic writes to the source the way an application would while
// nobody has told it to stop: inserts, field updates, replacements, deletes,
// multi-document transactions and a collection that did not exist before.
type productionTraffic struct {
	stop  chan struct{}
	done  sync.WaitGroup
	ops   atomic.Int64
	fails atomic.Int64
}

func (f *followFixture) startTraffic(t *testing.T, writers int, pause time.Duration) *productionTraffic {
	t.Helper()
	traffic := &productionTraffic{stop: make(chan struct{})}
	for w := 0; w < writers; w++ {
		traffic.done.Add(1)
		go func(w int) {
			defer traffic.done.Done()
			ctx := context.Background()
			rng := rand.New(rand.NewSource(int64(1000 + w)))
			users, orders, late := f.source.Collection("users"), f.source.Collection("orders"), f.source.Collection("late_arrivals")
			record := func(err error) {
				if err != nil {
					traffic.fails.Add(1)
					return
				}
				traffic.ops.Add(1)
			}
			for n := 0; ; n++ {
				select {
				case <-traffic.stop:
					return
				default:
				}
				user := fmt.Sprintf("user-%04d", rng.Intn(3000))
				order := fmt.Sprintf("order-%04d", rng.Intn(2000))
				switch rng.Intn(8) {
				case 0:
					_, err := users.InsertOne(ctx, bson.D{{Key: "_id", Value: fmt.Sprintf("new-%d-%d", w, n)}, {Key: "name", Value: "arrived during import"}, {Key: "visits", Value: int32(1)}})
					record(err)
				case 1, 2:
					_, err := users.UpdateOne(ctx, bson.M{"_id": user}, bson.M{"$inc": bson.M{"visits": 1}, "$set": bson.M{"last_writer": w}})
					record(err)
				case 3:
					_, err := orders.ReplaceOne(ctx, bson.M{"_id": order}, bson.D{{Key: "status", Value: "replaced"}, {Key: "round", Value: int32(n)}})
					record(err)
				case 4:
					_, err := orders.DeleteOne(ctx, bson.M{"_id": order})
					record(err)
				case 5:
					// Renaming moves the value to the end of the document.
					from, to := "name", "title"
					if n%2 == 1 {
						from, to = to, from
					}
					_, err := users.UpdateOne(ctx, bson.M{"_id": user, from: bson.M{"$exists": true}}, bson.M{"$rename": bson.M{from: to}})
					record(err)
				case 6:
					session, err := f.sourceClient.StartSession()
					if err != nil {
						record(err)
						continue
					}
					_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
						if _, err := users.UpdateOne(sc, bson.M{"_id": user}, bson.M{"$inc": bson.M{"orders": 1}}); err != nil {
							return nil, err
						}
						if _, err := orders.UpdateOne(sc, bson.M{"_id": order}, bson.M{"$set": bson.M{"status": "paid"}, "$inc": bson.M{"touched": 1}}, options.Update().SetUpsert(true)); err != nil {
							return nil, err
						}
						// A second change to the same document in the same
						// transaction.
						_, err := users.UpdateOne(sc, bson.M{"_id": user}, bson.M{"$set": bson.M{"last_order": order}})
						return nil, err
					})
					session.EndSession(ctx)
					record(err)
				case 7:
					_, err := late.InsertOne(ctx, bson.M{"writer": w, "n": n})
					record(err)
				}
				if pause > 0 {
					time.Sleep(pause)
				}
			}
		}(w)
	}
	return traffic
}

func (p *productionTraffic) halt() {
	close(p.stop)
	p.done.Wait()
}

func (f *followFixture) seedShop(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	users := make([]interface{}, 0, 3000)
	for i := 0; i < 3000; i++ {
		users = append(users, bson.D{{Key: "_id", Value: fmt.Sprintf("user-%04d", i)}, {Key: "name", Value: fmt.Sprintf("User %d", i)}, {Key: "visits", Value: int32(0)}})
	}
	_, err := f.source.Collection("users").InsertMany(ctx, users)
	require.NoError(t, err)
	orders := make([]interface{}, 0, 2000)
	for i := 0; i < 2000; i++ {
		orders = append(orders, bson.D{{Key: "_id", Value: fmt.Sprintf("order-%04d", i)}, {Key: "status", Value: "open"}, {Key: "total", Value: float64(i) + 0.5}})
	}
	_, err = f.source.Collection("orders").InsertMany(ctx, orders)
	require.NoError(t, err)
}

// The source is written throughout the import. The imported project must be
// the source at exactly one cluster time, and following must then bring it
// to the source's final state without losing or inventing a document.
func TestOnlineImport_SourceWrittenThroughout(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		f := newFollowFixture(t, sourceURI)
		f.seedShop(t)

		traffic := f.startTraffic(t, 3, time.Millisecond)
		// The writers must be under way before the import starts, however
		// long their first connections take.
		for started := time.Now(); traffic.ops.Load() < 20; time.Sleep(10 * time.Millisecond) {
			require.Less(t, time.Since(started), 30*time.Second, "production traffic never started (%d failed writes)", traffic.fails.Load())
		}
		before := traffic.ops.Load()
		result := f.importOnline(t, walcli.OnlineImport{BatchSize: 100})
		during := traffic.ops.Load() - before
		require.Greater(t, during, int64(50), "the source must actually be written while it is imported (%d writes failed)", traffic.fails.Load())
		require.True(t, result.Online)
		require.NotNil(t, result.ConsistentClusterTime)
		require.Greater(t, result.ConsistentLSN, result.StartLSN)

		// The published project is the source as it stood at the reported
		// cluster time. MongoDB still serves that moment, so it is read back.
		f.requireMainEqualsSource(t, result.ConsistentLSN, result.ConsistentClusterTime, "at the import's consistent point")

		// Traffic continues; following starts from where the import stopped.
		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		time.Sleep(300 * time.Millisecond)
		traffic.halt()
		require.Zero(t, traffic.fails.Load(), "production writes must not fail because of the import")
		info := f.waitCaughtUp(t)
		require.NoError(t, stop())
		f.requireMainEqualsSource(t, 0, nil, "after following caught up")

		require.Equal(t, "exact", info.Mode)
		require.Len(t, info.InexactRanges, 1, "the copy made while the source was written is the one inexact range")
		require.Equal(t, result.ConsistentLSN, info.InexactRanges[0].ToLSN)
		t.Logf("%d writes during the import, %d in total; consistent at LSN %d", during, traffic.ops.Load(), result.ConsistentLSN)
	})
}

// restart builds a second service stack on the same metadata database, as a
// new process would.
func (f *followFixture) restart(t *testing.T) *walcli.Services {
	t.Helper()
	services, err := walcli.NewServicesAt(metadataDeploymentURI, f.metadataName)
	require.NoError(t, err)
	services.SynchronousSnapshots()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, status := range services.Ingest.Statuses() {
			_ = services.Ingest.Cancel(cleanup, status.BranchID)
		}
		_ = services.WaitAuto(cleanup)
		services.Monitor.Stop()
		_ = services.Client.Disconnect(cleanup)
	})
	return services
}

// followedEntries returns main's own data entries above an LSN.
func (f *followFixture) followedEntries(t *testing.T, after int64) []*wal.Entry {
	t.Helper()
	branch := f.main(t)
	entries, err := f.services.WAL.GetEntries(bson.M{"branch_id": branch.ID, "lsn": bson.M{"$gt": after, "$lte": branch.HeadLSN}},
		options.Find().SetSort(bson.M{"lsn": 1}))
	require.NoError(t, err)
	data := entries[:0]
	for _, entry := range entries {
		if entry.IsData() {
			data = append(data, entry)
		}
	}
	return data
}

func (f *followFixture) seedSmall(t *testing.T) {
	t.Helper()
	docs := make([]interface{}, 0, 20)
	for i := 0; i < 20; i++ {
		docs = append(docs, bson.D{{Key: "_id", Value: fmt.Sprintf("item-%02d", i)}, {Key: "name", Value: fmt.Sprintf("Item %d", i)}, {Key: "stock", Value: int32(10)}})
	}
	_, err := f.source.Collection("catalog").InsertMany(context.Background(), docs)
	require.NoError(t, err)
}

// Following is stopped and started again, the second time by a new process.
// Changes made while nothing was following are picked up, and each change
// ends up in main exactly once.
func TestFollow_ResumesAfterRestartWithoutLossOrRepeat(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		result := f.importOnline(t, walcli.OnlineImport{})
		catalog := f.source.Collection("catalog")

		changes := 0
		write := func(round int) {
			for i := 0; i < 20; i++ {
				_, err := catalog.UpdateOne(ctx, bson.M{"_id": fmt.Sprintf("item-%02d", i)}, bson.M{"$inc": bson.M{"stock": -1}, "$set": bson.M{"round": round}})
				require.NoError(t, err)
				changes++
			}
			_, err := catalog.InsertOne(ctx, bson.M{"_id": fmt.Sprintf("added-%d", round), "round": round})
			require.NoError(t, err)
			_, err = catalog.DeleteOne(ctx, bson.M{"_id": fmt.Sprintf("added-%d", round-1)})
			require.NoError(t, err)
			changes++
			if round > 1 {
				changes++ // the delete found a document
			}
			session, err := f.sourceClient.StartSession()
			require.NoError(t, err)
			_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
				for _, id := range []string{"item-00", "item-01", "item-02"} {
					if _, err := catalog.UpdateOne(sc, bson.M{"_id": id}, bson.M{"$set": bson.M{"reserved_in_round": round}}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			session.EndSession(ctx)
			require.NoError(t, err)
			changes += 3
		}

		stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
		write(1)
		f.waitCaughtUp(t)
		require.NoError(t, stop())
		headWhenStopped := f.main(t).HeadLSN

		// Nothing follows the source now.
		write(2)
		write(3)
		require.Equal(t, headWhenStopped, f.main(t).HeadLSN, "main only moves while something follows the source")

		restarted := f.restart(t)
		stopRestarted := f.follow(t, restarted, walcli.FollowSourceOptions{})
		write(4)
		f.waitCaughtUp(t)
		require.NoError(t, stopRestarted())

		f.requireMainEqualsSource(t, 0, nil, "after the restart")
		entries := f.followedEntries(t, result.ConsistentLSN)
		require.Len(t, entries, changes, "every source change is in main exactly once")
		var last primitive.Timestamp
		for _, entry := range entries {
			at, ok := entry.Metadata["source_time"].(primitive.Timestamp)
			require.True(t, ok, "followed entries record their source cluster time")
			require.False(t, at.Before(last), "entries are in the source's order")
			last = at
			require.Equal(t, "source", entry.Actor)
		}
	})
}

// Without pre- and post-images on the source, every followed update still
// stores the document exactly as the source held it after that update, and
// nothing about the source is changed to get it.
func TestFollow_ExactDocumentsWithoutTouchingTheSource(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	result := f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	catalog := f.source.Collection("catalog")

	updates := []interface{}{
		bson.M{"$set": bson.M{"name": "renamed"}},
		bson.M{"$inc": bson.M{"stock": 5}},
		bson.M{"$set": bson.M{"tags": bson.A{"a", "b"}, "nested": bson.M{"z": 1, "a": 2}}},
		// Moves a value to a new field at the end of the document.
		bson.M{"$rename": bson.M{"name": "title"}},
		bson.M{"$unset": bson.M{"tags": ""}},
		bson.M{"$push": bson.M{"history": bson.M{"$each": bson.A{1, 2, 3}}}},
	}
	var stored []bson.Raw
	for _, update := range updates {
		_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-07"}, update)
		require.NoError(t, err)
		// One writer, so this read is the document as that update left it.
		raw, err := catalog.FindOne(ctx, bson.M{"_id": "item-07"}).Raw()
		require.NoError(t, err)
		stored = append(stored, raw)
	}
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	entries := f.followedEntries(t, result.ConsistentLSN)
	require.Len(t, entries, len(updates))
	for i, entry := range entries {
		require.Equal(t, []byte(stored[i]), []byte(entry.PostImage), "update %d: the stored document, byte for byte", i)
		require.Equal(t, "snapshot", entry.Metadata["capture_images"], "read back at the update's own cluster time")
	}
	info, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "exact", info.Mode)

	// The source is as its owner left it: no image recording switched on,
	// and no collection of Argon's in it.
	specs, err := f.source.ListCollectionSpecifications(ctx, bson.D{})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "catalog", specs[0].Name)
	_, lookupErr := specs[0].Options.LookupErr("changeStreamPreAndPostImages")
	require.Error(t, lookupErr, "pre- and post-images must not be enabled on a source unless asked for")
}

// Enabling images on the source is an explicit option. When it is given the
// source's collections record images and followed updates carry them.
func TestFollow_SourceImagesOnlyWhenAskedFor(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	result := f.importOnline(t, walcli.OnlineImport{EnableSourceImages: true})
	require.Equal(t, []string{"catalog"}, result.ImagesEnabled)

	specs, err := f.source.ListCollectionSpecifications(ctx, bson.D{})
	require.NoError(t, err)
	enabled, ok := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
	require.True(t, ok && enabled)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	catalog := f.source.Collection("catalog")
	before, err := catalog.FindOne(ctx, bson.M{"_id": "item-03"}).Raw()
	require.NoError(t, err)
	_, err = catalog.UpdateOne(ctx, bson.M{"_id": "item-03"}, bson.M{"$set": bson.M{"stock": int32(0)}})
	require.NoError(t, err)
	after, err := catalog.FindOne(ctx, bson.M{"_id": "item-03"}).Raw()
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	entries := f.followedEntries(t, result.ConsistentLSN)
	require.Len(t, entries, 1)
	require.Equal(t, "attached", entries[0].Metadata["capture_images"])
	require.Equal(t, []byte(after), []byte(entries[0].PostImage))
	require.Equal(t, []byte(before), []byte(entries[0].PreImage))

	// A collection the application creates later is not altered by Argon,
	// until following is started with the option again.
	_, err = f.source.Collection("later").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	imagesOn := func(collection string) bool {
		specs, err := f.source.ListCollectionSpecifications(ctx, bson.M{"name": collection})
		require.NoError(t, err)
		require.Len(t, specs, 1)
		enabled, _ := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
		return enabled
	}
	stop = f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	require.False(t, imagesOn("later"))
	stop = f.follow(t, f.services, walcli.FollowSourceOptions{EnableSourceImages: true})
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	require.True(t, imagesOn("later"))
	f.requireMainEqualsSource(t, 0, nil, "with a collection created after the import")
}

// A collection dropped on the source carries no documents in its event. The
// follower does not guess: it stops as degraded at the change before it.
func TestFollow_SourceCollectionDropIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	// The application drops one of its own collections.
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(2)}})
	require.NoError(t, err)

	followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
	require.Error(t, followErr)
	info, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", info.State)
	require.Contains(t, info.Error, "drop of scratch")

	// The change before the drop was applied, the one after it was not.
	doc, err := f.services.Materializer.MaterializeDocument(f.main(t), "catalog", wal.DocumentIDString("item-00"))
	require.NoError(t, err)
	require.EqualValues(t, 1, doc["stock"])
}

// When following falls behind MongoDB's snapshot history on a source without
// images, an update's exact document can no longer be read. By default that
// is a reported gap. With --allow-inexact following converges on the
// source's current state instead and records which history is not exact.
//
// The test shortens the snapshot history of the source deployment, so it
// only runs against the separate one, where nothing else is affected.
func TestFollow_UpdateOlderThanSnapshotHistory(t *testing.T) {
	separate := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if separate == "" {
		t.Skip("set ARGON_TEST_SOURCE_MONGODB_URI to a second replica set: this test changes a server parameter on the source deployment")
	}
	ctx := context.Background()
	f := newFollowFixture(t, separate)
	admin := f.sourceClient.Database("admin")
	setWindow := func(seconds int) {
		require.NoError(t, admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "minSnapshotHistoryWindowInSeconds", Value: seconds}}).Err())
	}
	setWindow(1)
	t.Cleanup(func() { setWindow(300) })

	f.seedSmall(t)
	result := f.importOnline(t, walcli.OnlineImport{})
	catalog := f.source.Collection("catalog")
	for round := 1; round <= 3; round++ {
		_, err := catalog.UpdateOne(ctx, bson.M{"_id": "item-05"}, bson.M{"$set": bson.M{"stock": int32(round)}})
		require.NoError(t, err)
	}
	_, err := catalog.DeleteOne(ctx, bson.M{"_id": "item-06"})
	require.NoError(t, err)
	// Let those updates fall out of the snapshot history. MongoDB advances
	// its oldest timestamp with its checkpoints, so keep it writing. The
	// probe reads at a time after the last update: once that is gone, every
	// update before it is too.
	afterUpdates := f.sourceTime(t)
	gone := false
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline) && !gone; {
		f.tick(t)
		time.Sleep(time.Second)
		probe := f.source.RunCommand(ctx, bson.D{{Key: "find", Value: "catalog"}, {Key: "limit", Value: 1}, {Key: "readConcern", Value: bson.D{
			{Key: "level", Value: "snapshot"}, {Key: "atClusterTime", Value: afterUpdates}}}})
		gone = probe.Err() != nil
	}
	require.True(t, gone, "the source must have discarded the history of the updates")

	// Default: a gap, reported, and main not advanced past it.
	head := f.main(t).HeadLSN
	followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
	require.Error(t, followErr)
	info, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", info.State)
	require.Contains(t, info.Error, "exact post-image unavailable")
	require.Equal(t, head, f.main(t).HeadLSN)

	// Allowed: converge, and say which range is not exact.
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{AllowInexact: true})
	info = f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after the inexact catch-up")
	require.Equal(t, "exact", info.Mode)
	require.Len(t, info.InexactRanges, 2, "the import and the catch-up")
	catchUp := info.InexactRanges[1]
	require.Equal(t, head+1, catchUp.FromLSN)
	require.Contains(t, catchUp.Reason, "snapshot history")
	entries := f.followedEntries(t, result.ConsistentLSN)
	require.NotEmpty(t, entries)
	require.Equal(t, "current", entries[0].Metadata["capture_images"], "the entry says its document is a later state")
	require.LessOrEqual(t, entries[len(entries)-1].LSN, catchUp.ToLSN)
}
