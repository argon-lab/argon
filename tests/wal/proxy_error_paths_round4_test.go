package wal_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/wireproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// round4Lookups sits between the proxy and the real project and branch
// services. It counts the metadata reads the proxy makes and, when told to,
// fails them the way an unreachable metadata store does. With nothing set it
// hands every answer and every error through unchanged.
type round4Lookups struct {
	projects wireproxy.ProjectLookup
	branches wireproxy.BranchLookup

	down     atomic.Bool
	failNext atomic.Int64

	byName   atomic.Int64 // GetProjectIDByName
	byBranch atomic.Int64 // GetBranch
	byID     atomic.Int64 // GetBranchByID
	failed   atomic.Int64
}

var errRound4StoreDown = fmt.Errorf("server selection error: %w", context.DeadlineExceeded)

func (l *round4Lookups) unreachable() bool {
	if l.down.Load() {
		l.failed.Add(1)
		return true
	}
	for {
		n := l.failNext.Load()
		if n <= 0 {
			return false
		}
		if l.failNext.CompareAndSwap(n, n-1) {
			l.failed.Add(1)
			return true
		}
	}
}

func (l *round4Lookups) reads() int64 { return l.byName.Load() + l.byBranch.Load() + l.byID.Load() }

func (l *round4Lookups) GetProjectIDByName(name string) (string, error) {
	l.byName.Add(1)
	if l.unreachable() {
		return "", errRound4StoreDown
	}
	return l.projects.GetProjectIDByName(name)
}

func (l *round4Lookups) GetBranch(projectID, name string) (*wal.Branch, error) {
	l.byBranch.Add(1)
	if l.unreachable() {
		return nil, errRound4StoreDown
	}
	return l.branches.GetBranch(projectID, name)
}

func (l *round4Lookups) GetBranchByID(branchID string) (*wal.Branch, error) {
	l.byID.Add(1)
	if l.unreachable() {
		return nil, errRound4StoreDown
	}
	return l.branches.GetBranchByID(branchID)
}

// round4Proxy is a proxy over the real services of a fresh metadata database
// with one project that has a checked-out sandbox, "run-1".
type round4Proxy struct {
	db       *mongo.Database
	project  string
	alias    string
	physical *mongo.Database
	lookups  *round4Lookups
	addr     string
	mainID   string
	sandbox  string
	discard  func(t *testing.T)
}

func newRound4Proxy(t *testing.T, label string) *round4Proxy {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()
	project := fmt.Sprintf("round4-%s-%d", label, time.Now().UnixNano())
	f, svc, mainID := newSandboxFixture(t, db, project)
	_, err := db.Collection("wal_projects").InsertOne(ctx, bson.M{"_id": project, "name": project, "use_wal": true})
	require.NoError(t, err)

	info, err := svc.Create(ctx, project, mainID, "run-1", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(info.PhysicalDB).Drop(context.Background()) })

	// The proxy as walcli wires it: the project service and the branch
	// service, nothing in between but the counter.
	projects, err := projectwal.NewProjectService(db, f.wal, f.branches)
	require.NoError(t, err)
	lookups := &round4Lookups{projects: projects, branches: f.branches}
	return &round4Proxy{
		db:       db,
		project:  project,
		alias:    project + "~run-1",
		physical: db.Client().Database(info.PhysicalDB),
		lookups:  lookups,
		addr:     startProxy(t, wireproxy.New("localhost:27017", lookups, lookups)),
		mainID:   mainID,
		sandbox:  info.BranchID,
		discard: func(t *testing.T) {
			require.NoError(t, svc.Discard(ctx, info.BranchID))
		},
	}
}

func (p *round4Proxy) connect(t *testing.T, params string, more ...*options.ClientOptions) *mongo.Client {
	t.Helper()
	uri := fmt.Sprintf("mongodb://%s/%s?directConnection=true%s", p.addr, p.alias, params)
	opts := append([]*options.ClientOptions{options.Client().ApplyURI(uri)}, more...)
	client, err := mongo.Connect(context.Background(), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client
}

func round4Code(err error) (int32, string) {
	var cmd mongo.CommandError
	if errors.As(err, &cmd) {
		return cmd.Code, cmd.Name
	}
	var we mongo.WriteException
	if errors.As(err, &we) && we.WriteConcernError != nil {
		return int32(we.WriteConcernError.Code), we.WriteConcernError.Name
	}
	return 0, ""
}

// Round 4 (0e6aae9, wireproxy.lookup: "if errors.Is(err, wal.ErrProjectNotFound)
// ... project not found", anything else errMetadataUnavailable).
//
// The commit made GetProject, the lookup by ID, return wal.ErrProjectNotFound.
// The proxy does not call it. It calls GetProjectIDByName, which goes through
// GetProjectByName, and that still returns errors.New("project not found"):
// an error that matches no sentinel. The stub in internal/wireproxy's tests
// was changed to return the sentinel, so the unit tests pass; the real
// service does not return it.
//
// So through `argon proxy`, an alias whose project does not exist (a typo, a
// project deleted, a project still importing) is answered HostUnreachable
// "argon's metadata is unavailable: cannot look up project ...: project not
// found", by a proxy whose metadata store is answering every read, and the
// answer is not remembered: every command to that alias is a metadata read.
//
// On the parent commit (6aa0f7a) and on master this is NamespaceNotFound
// `project "x" not found`, cached for a second.
func TestRound4_AnAliasOfAProjectThatDoesNotExistIsNamespaceNotFound(t *testing.T) {
	p := newRound4Proxy(t, "noproject")
	ctx := context.Background()
	client := p.connect(t, "")

	// A project whose import has not finished: GetProjectByName leaves it
	// out, the same way.
	importing := p.project + "-importing"
	_, err := p.db.Collection("wal_projects").InsertOne(ctx, bson.M{"_id": importing, "name": importing, "use_wal": true, "importing": true})
	require.NoError(t, err)

	// A branch that was there and is deleted.
	_, err = p.db.Collection("wal_branches").InsertOne(ctx, bson.M{
		"_id": p.project + "-gone", "project_id": p.project, "name": "gone", "is_deleted": true,
	})
	require.NoError(t, err)

	cases := []struct {
		name    string
		alias   string
		changed bool // a row this commit changed the answer of
	}{
		{"control: no such branch in a project that exists", p.project + "~no-such-branch", false},
		{"control: a deleted branch", p.project + "~gone", false},
		{"no such project", p.project + "-typo~run-1", true},
		{"a project that is still importing", importing + "~main", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := p.lookups.byName.Load()
			var first error
			for i := 0; i < 5; i++ {
				_, err := client.Database(tc.alias).Collection("docs").InsertOne(ctx, bson.M{"n": i})
				require.Error(t, err)
				if first == nil {
					first = err
				}
			}
			reads := p.lookups.byName.Load() - before
			code, name := round4Code(first)
			t.Logf("insert through %q: code %d %s: %v", tc.alias, code, name, first)
			t.Logf("5 inserts within a second made %d lookups of the project by name", reads)

			assert.EqualValues(t, 26, code, "an alias that names nothing is NamespaceNotFound")
			assert.NotContains(t, first.Error(), "metadata is unavailable",
				"the metadata store answered: it said there is no such project")
			assert.LessOrEqual(t, reads, int64(2),
				"an alias that names nothing is remembered for a second (resolveErrorTTL); every command read the metadata store")
			assert.Zero(t, p.lookups.failed.Load(), "the control: no metadata read failed in this test")
		})
	}
}

// The same defect, as an operator meets it. HostUnreachable is a code the
// driver retries a read on (driver/errors.go retryableCodes holds 6), and a
// client with a timeout set (timeoutMS, the setting MongoDB recommends) has
// its reads retried until the timeout runs out. Nothing is cached, so one
// find through an alias with a typo in the project name becomes a metadata
// query per retry, back to back, for as long as the client's timeout.
func TestRound4_OneFindThroughAMistypedAliasIsNotARetryStormOnTheMetadataStore(t *testing.T) {
	p := newRound4Proxy(t, "storm")
	client := p.connect(t, "&timeoutMS=2000")

	before := p.lookups.byName.Load()
	start := time.Now()
	err := client.Database(p.project+"-typo~run-1").Collection("docs").FindOne(context.Background(), bson.M{}).Err()
	took := time.Since(start)
	reads := p.lookups.byName.Load() - before
	require.Error(t, err)
	code, name := round4Code(err)
	t.Logf("FindOne returned after %v: code %d %s: %v", took.Round(time.Millisecond), code, name, err)
	t.Logf("one FindOne made %d lookups of the project by name", reads)

	assert.Zero(t, p.lookups.failed.Load(), "the control: no metadata read failed")
	assert.Less(t, took, time.Second, "a project that does not exist is an answer, given at once")
	assert.LessOrEqual(t, reads, int64(2), "one find of a project that does not exist")
}

// What a client sees for HostUnreachable on each kind of command, with the
// real driver. These are the rows of the case table; a row that fails here
// is a finding, a row that passes is the evidence for the table.
func TestRound4_WhatAClientSeesWhileTheProxyCannotReadItsMetadata(t *testing.T) {
	p := newRound4Proxy(t, "outage")
	ctx := context.Background()

	var mu sync.Mutex
	var sdam []string
	var created, closed, cleared int
	monitors := options.Client().
		SetServerMonitor(&event.ServerMonitor{
			ServerDescriptionChanged: func(e *event.ServerDescriptionChangedEvent) {
				mu.Lock()
				defer mu.Unlock()
				sdam = append(sdam, fmt.Sprintf("%s->%s", e.PreviousDescription.Kind, e.NewDescription.Kind))
			},
		}).
		SetPoolMonitor(&event.PoolMonitor{Event: func(e *event.PoolEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch e.Type {
			case event.ConnectionCreated:
				created++
			case event.ConnectionClosed:
				closed++
			case event.PoolCleared:
				cleared++
			}
		}})
	client := p.connect(t, "&retryWrites=true", monitors)
	viaProxy := client.Database(p.alias)
	docs := viaProxy.Collection("docs4")
	physical := p.physical.Collection("docs4")
	count := func() int64 {
		n, err := physical.CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		return n
	}
	snapshot := func() (int, int, int, int) {
		mu.Lock()
		defer mu.Unlock()
		return len(sdam), created, closed, cleared
	}

	_, err := docs.InsertOne(ctx, bson.M{"_id": "seed"})
	require.NoError(t, err)
	require.EqualValues(t, 1, count())
	sdamBefore, createdBefore, closedBefore, clearedBefore := snapshot()

	t.Run("insert while down: an error, nothing written, not retried", func(t *testing.T) {
		p.lookups.down.Store(true)
		defer p.lookups.down.Store(false)
		before := p.lookups.reads()
		_, err := docs.InsertOne(ctx, bson.M{"_id": "down-insert"})
		code, name := round4Code(err)
		t.Logf("insert: code %d %s: %v (metadata reads: %d)", code, name, err, p.lookups.reads()-before)
		require.Error(t, err)
		assert.EqualValues(t, 6, code)
		assert.EqualValues(t, 1, count())
	})

	t.Run("retryable insert over a blip of one read: applied once or not at all", func(t *testing.T) {
		p.lookups.failNext.Store(1)
		_, err := docs.InsertOne(ctx, bson.M{"_id": "blip-insert"})
		p.lookups.failNext.Store(0)
		n, cerr := physical.CountDocuments(ctx, bson.M{"_id": "blip-insert"})
		require.NoError(t, cerr)
		t.Logf("insert over a blip: err=%v, documents written=%d", err, n)
		if err == nil {
			assert.EqualValues(t, 1, n, "reported done")
		} else {
			assert.EqualValues(t, 0, n, "reported failed")
		}
		// Sent again by the application: the write was never forwarded, so
		// no transaction number of it is on the server to collide with.
		_, err = docs.InsertOne(ctx, bson.M{"_id": "blip-insert-again"})
		assert.NoError(t, err, "the insert after the blip")
	})

	t.Run("insert of several documents over a blip: never written twice", func(t *testing.T) {
		p.lookups.failNext.Store(1)
		_, err := docs.InsertMany(ctx, []interface{}{bson.M{"k": "many"}, bson.M{"k": "many"}, bson.M{"k": "many"}})
		p.lookups.failNext.Store(0)
		n, cerr := physical.CountDocuments(ctx, bson.M{"k": "many"})
		require.NoError(t, cerr)
		t.Logf("insertMany over a blip: err=%v, documents written=%d", err, n)
		if err == nil {
			assert.EqualValues(t, 3, n)
		} else {
			assert.EqualValues(t, 0, n)
		}
	})

	t.Run("find over a blip of one read: the driver's retry answers it", func(t *testing.T) {
		p.lookups.failNext.Store(1)
		var doc bson.M
		err := docs.FindOne(ctx, bson.M{"_id": "seed"}).Decode(&doc)
		p.lookups.failNext.Store(0)
		t.Logf("find over a blip: err=%v doc=%v", err, doc)
		assert.NoError(t, err)
	})

	t.Run("find while down: an error after one retry, not a hang", func(t *testing.T) {
		p.lookups.down.Store(true)
		defer p.lookups.down.Store(false)
		before := p.lookups.reads()
		start := time.Now()
		err := docs.FindOne(ctx, bson.M{"_id": "seed"}).Err()
		code, name := round4Code(err)
		t.Logf("find: code %d %s after %v: %v (metadata reads: %d)", code, name, time.Since(start).Round(time.Millisecond), err, p.lookups.reads()-before)
		require.Error(t, err)
		assert.EqualValues(t, 6, code)
		assert.LessOrEqual(t, p.lookups.reads()-before, int64(2))
	})

	t.Run("drop while down: an error, and the collection is still there", func(t *testing.T) {
		p.lookups.down.Store(true)
		defer p.lookups.down.Store(false)
		err := docs.Drop(ctx)
		t.Logf("drop: %v", err)
		assert.Error(t, err)
		assert.NotZero(t, count())
		err = viaProxy.Drop(ctx)
		t.Logf("dropDatabase: %v", err)
		assert.Error(t, err)
		assert.NotZero(t, count())
	})

	t.Run("update, delete, aggregate, count, createIndexes while down: errors", func(t *testing.T) {
		p.lookups.down.Store(true)
		defer p.lookups.down.Store(false)
		_, err := docs.UpdateOne(ctx, bson.M{"_id": "seed"}, bson.M{"$set": bson.M{"touched": true}})
		assert.Error(t, err, "update")
		_, err = docs.DeleteOne(ctx, bson.M{"_id": "seed"})
		assert.Error(t, err, "delete")
		_, err = docs.Aggregate(ctx, mongo.Pipeline{})
		assert.Error(t, err, "aggregate")
		_, err = docs.CountDocuments(ctx, bson.M{})
		assert.Error(t, err, "count")
		_, err = docs.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "k", Value: 1}}})
		assert.Error(t, err, "createIndexes")
		_, err = viaProxy.ListCollectionNames(ctx, bson.M{})
		assert.Error(t, err, "listCollections")
		p.lookups.down.Store(false)
		var seed bson.M
		require.NoError(t, physical.FindOne(ctx, bson.M{"_id": "seed"}).Decode(&seed))
		assert.NotContains(t, seed, "touched")
	})

	t.Run("a cursor open across an outage: getMore does not go through the alias", func(t *testing.T) {
		// The driver sends getMore (and killCursors) to the database named
		// in the find reply's cursor.ns, which is the physical database:
		// the proxy does not resolve it, so an open cursor reads on.
		many := viaProxy.Collection("many4")
		batch := make([]interface{}, 50)
		for i := range batch {
			batch[i] = bson.M{"n": i}
		}
		_, err := many.InsertMany(ctx, batch)
		require.NoError(t, err)
		cursor, err := many.Find(ctx, bson.M{}, options.Find().SetBatchSize(10))
		require.NoError(t, err)
		seen := 0
		for seen < 10 && cursor.Next(ctx) {
			seen++
		}
		readsBefore := p.lookups.reads()
		p.lookups.down.Store(true)
		for cursor.Next(ctx) {
			seen++
		}
		p.lookups.down.Store(false)
		t.Logf("getMore while down: read %d of 50, err=%v (metadata reads %d)", seen, cursor.Err(), p.lookups.reads()-readsBefore)
		assert.NoError(t, cursor.Err())
		assert.Equal(t, 50, seen)
		assert.Zero(t, p.lookups.reads()-readsBefore)
		_ = cursor.Close(ctx)
	})

	t.Run("a transaction whose first statement meets a blip: nothing half written", func(t *testing.T) {
		session, err := client.StartSession()
		require.NoError(t, err)
		defer session.EndSession(ctx)
		attempts := 0
		p.lookups.failNext.Store(1)
		_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
			attempts++
			if _, err := docs.InsertOne(sc, bson.M{"txn": "a", "attempt": attempts}); err != nil {
				return nil, err
			}
			_, err := docs.InsertOne(sc, bson.M{"txn": "b", "attempt": attempts})
			return nil, err
		})
		p.lookups.failNext.Store(0)
		n, cerr := physical.CountDocuments(ctx, bson.M{"txn": bson.M{"$exists": true}})
		require.NoError(t, cerr)
		var cmd mongo.CommandError
		labels := []string(nil)
		if errors.As(err, &cmd) {
			labels = cmd.Labels
		}
		t.Logf("transaction over a blip: err=%v labels=%v callback ran %d time(s), documents written=%d", err, labels, attempts, n)
		if err == nil {
			assert.EqualValues(t, 2, n)
		} else {
			assert.EqualValues(t, 0, n)
		}

		// The session is usable for the next transaction.
		_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
			return docs.InsertOne(sc, bson.M{"txn2": true})
		})
		assert.NoError(t, err, "the next transaction on the same session")
	})

	t.Run("a transaction whose second statement meets a blip: aborted whole", func(t *testing.T) {
		session, err := client.StartSession()
		require.NoError(t, err)
		defer session.EndSession(ctx)
		_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
			if _, err := docs.InsertOne(sc, bson.M{"txn3": "a"}); err != nil {
				return nil, err
			}
			p.lookups.failNext.Store(1)
			_, err := docs.InsertOne(sc, bson.M{"txn3": "b"})
			p.lookups.failNext.Store(0)
			return nil, err
		})
		n, cerr := physical.CountDocuments(ctx, bson.M{"txn3": bson.M{"$exists": true}})
		require.NoError(t, cerr)
		t.Logf("transaction, blip on the second statement: err=%v, documents written=%d", err, n)
		assert.Error(t, err)
		assert.EqualValues(t, 0, n, "the first statement was rolled back")
	})

	t.Run("handshake, ping and other databases while down: untouched", func(t *testing.T) {
		p.lookups.down.Store(true)
		defer p.lookups.down.Store(false)
		before := p.lookups.reads()
		assert.NoError(t, client.Ping(ctx, nil))
		plain := client.Database(fmt.Sprintf("round4_plain_%d", time.Now().UnixNano()))
		t.Cleanup(func() { _ = plain.Drop(context.Background()) })
		_, err := plain.Collection("x").InsertOne(ctx, bson.M{"_id": 1})
		assert.NoError(t, err)
		// A new client: its handshake does not go through the alias.
		fresh := p.connect(t, "")
		assert.NoError(t, fresh.Ping(ctx, nil))
		assert.Zero(t, p.lookups.reads()-before, "none of that read the metadata store")
	})

	t.Run("after the outage: the next command goes through at once", func(t *testing.T) {
		p.lookups.down.Store(true)
		_ = docs.FindOne(ctx, bson.M{"_id": "seed"}).Err()
		p.lookups.down.Store(false)
		start := time.Now()
		assert.NoError(t, docs.FindOne(ctx, bson.M{"_id": "seed"}).Err())
		_, err := docs.InsertOne(ctx, bson.M{"_id": "after"})
		assert.NoError(t, err)
		assert.Less(t, time.Since(start), time.Second)
	})

	t.Run("an alias that had run out of its five seconds when the store went down", func(t *testing.T) {
		time.Sleep(5100 * time.Millisecond)
		p.lookups.down.Store(true)
		err := docs.FindOne(ctx, bson.M{"_id": "seed"}).Err()
		p.lookups.down.Store(false)
		code, _ := round4Code(err)
		assert.EqualValues(t, 6, code, "%v", err)
		assert.NoError(t, docs.FindOne(ctx, bson.M{"_id": "seed"}).Err(), "nothing was remembered of the outage")
	})

	t.Run("the driver did not mark the proxy down or reconnect", func(t *testing.T) {
		sdamAfter, createdAfter, closedAfter, clearedAfter := snapshot()
		mu.Lock()
		t.Logf("server description changes: %v", sdam)
		mu.Unlock()
		t.Logf("connections created %d, closed %d, pool cleared %d during the outages", createdAfter-createdBefore, closedAfter-closedBefore, clearedAfter-clearedBefore)
		assert.Equal(t, sdamBefore, sdamAfter, "HostUnreachable changed the driver's view of the server")
		assert.Zero(t, clearedAfter-clearedBefore, "HostUnreachable cleared the connection pool")
		assert.LessOrEqual(t, createdAfter-createdBefore, 2)
	})

	t.Run("a sandbox discarded: its alias is NamespaceNotFound, not HostUnreachable", func(t *testing.T) {
		require.NoError(t, docs.FindOne(ctx, bson.M{"_id": "seed"}).Err())
		p.discard(t)
		_, err := docs.InsertOne(ctx, bson.M{"_id": "after-discard"})
		code, name := round4Code(err)
		t.Logf("insert after discard: code %d %s: %v", code, name, err)
		require.Error(t, err)
		assert.EqualValues(t, 26, code)
	})
}
