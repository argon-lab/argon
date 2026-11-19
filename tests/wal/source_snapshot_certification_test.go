package wal_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/materializer"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
)

func markSnapshotsUnchecked(t *testing.T, db *mongo.Database, collection string, count int64) {
	t.Helper()
	result, err := db.Collection("wal_snapshots").UpdateMany(t.Context(), bson.M{"collection": collection},
		bson.M{"$unset": bson.M{"exact_old_binary_keys": ""}})
	require.NoError(t, err)
	require.Equal(t, count, result.ModifiedCount)
}

func TestSource_SnapshotCertificationScan(t *testing.T) {
	h := newHistoryFixture(t, "certification-scan")
	ctx := t.Context()
	const rows, snapshots = 1000, 4
	for version := range snapshots {
		docs := make([]bson.M, rows)
		for i := range docs {
			docs[i] = bson.M{"_id": int64(i), "version": int32(version)}
		}
		_, err := h.writer.PutMany(ctx, "items", docs)
		require.NoError(t, err)
		_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, h.reload().HeadLSN)
		require.NoError(t, err)
	}
	markSnapshotsUnchecked(t, h.db, "items", snapshots)
	var scans []bson.Raw
	var chunks int
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" {
			switch e.Command.Lookup("find").StringValue() {
			case "wal_log":
				scans = append(scans, bytes.Clone(e.Command))
			case "wal_snapshot_chunks":
				chunks++
			}
		}
	}}
	db := electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name())
	_ = newSnapshotFixture(t, db)
	require.Len(t, scans, 1)
	require.Equal(t, snapshots, chunks)
	certified, err := h.db.Collection("wal_snapshots").CountDocuments(ctx, bson.M{"old_key_check": "trusted"})
	require.NoError(t, err)
	require.EqualValues(t, snapshots, certified)

	// Explain the actual find, including index hint, bounds, projection and order.
	query := bson.D{}
	for _, key := range []string{"find", "filter", "hint", "projection", "sort"} {
		if value := scans[0].Lookup(key); !value.IsZero() {
			query = append(query, bson.E{Key: key, Value: value})
		}
	}
	plan, err := db.RunCommand(ctx, bson.D{{Key: "explain", Value: query}, {Key: "verbosity", Value: "executionStats"}}).Raw()
	require.NoError(t, err)
	stats := plan.Lookup("executionStats").Document()
	keys, documents := stats.Lookup("totalKeysExamined").AsInt64(), stats.Lookup("totalDocsExamined").AsInt64()
	t.Logf("four manifests over %d numeric puts: 1 WAL query examined %d keys and %d documents, %d chunks fetched", rows*snapshots, keys, documents, chunks)
	require.LessOrEqual(t, keys, int64(rows*snapshots+2))
	require.Zero(t, stats.Lookup("nReturned").AsInt64())
	require.Zero(t, documents)

	scans, chunks = nil, 0
	_ = newSnapshotFixture(t, db)
	require.Empty(t, scans)
	require.Zero(t, chunks)
}

func TestSource_SnapshotCertificationSharesAncestorScan(t *testing.T) {
	h := newHistoryFixture(t, "certification-ancestry")
	ctx := t.Context()
	h.put("parent", 1)
	for _, name := range []string{"one", "two"} {
		child, err := h.branches.CreateBranch(h.project, name, h.main.ID)
		require.NoError(t, err)
		writer := walwriter.New(h.wal, h.branches, h.mat, child)
		at, err := writer.Put(ctx, "docs", bson.M{"_id": name})
		require.NoError(t, err)
		_, err = h.snapshots.CreateSnapshot(ctx, child.ID, at)
		require.NoError(t, err)
	}
	markSnapshotsUnchecked(t, h.db, "docs", 2)
	calls := make(map[string]int)
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" && e.Command.Lookup("find").StringValue() == "wal_log" {
			calls[e.Command.Lookup("filter", "branch_id").StringValue()]++
		}
	}}
	db := electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name())
	_ = newSnapshotFixture(t, db)
	require.Len(t, calls, 3)
	for branch, count := range calls {
		require.Equal(t, 1, count, "scan of branch %s", branch)
	}
	certified, err := h.db.Collection("wal_snapshots").CountDocuments(ctx, bson.M{"old_key_check": "trusted"})
	require.NoError(t, err)
	require.EqualValues(t, 2, certified)
}

type certificationStore struct {
	snapshot.ChunkStore
	read func(context.Context, string) ([]byte, error)
}

func (s certificationStore) Get(ctx context.Context, id string) ([]byte, error) {
	return s.read(ctx, id)
}

func TestSource_SnapshotCertificationDeadline(t *testing.T) {
	h := newHistoryFixture(t, "certification-deadline")
	for _, name := range []string{"one", "two"} {
		at := h.put(name, 1)
		_, err := h.snapshots.CreateSnapshot(t.Context(), h.main.ID, at)
		require.NoError(t, err)
	}
	var deadline time.Time
	calls := make(map[string]bool)
	monitor := &event.CommandMonitor{Started: func(ctx context.Context, e *event.CommandStartedEvent) {
		if deadline.IsZero() || e.CommandName != "find" {
			return
		}
		name := e.Command.Lookup("find").StringValue()
		got, ok := ctx.Deadline()
		require.True(t, ok, name)
		require.True(t, got.Equal(deadline), "%s has deadline %v, want %v", name, got, deadline)
		calls[name] = true
	}}
	db := electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name())
	f := newSnapshotFixture(t, db)
	base := snapshot.NewMongoChunkStore(db)
	var chunkCalls int
	delayed := certificationStore{ChunkStore: base, read: func(ctx context.Context, _ string) ([]byte, error) {
		chunkCalls++
		got, ok := ctx.Deadline()
		require.True(t, ok)
		require.True(t, got.Equal(deadline))
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s, err := snapshot.NewServiceWithStore(db, f.branches, materializer.NewService(f.wal, f.branches), delayed)
	require.NoError(t, err)
	markSnapshotsUnchecked(t, h.db, "docs", 2)
	withLimit, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	deadline, _ = withLimit.Deadline()
	start := time.Now()
	s.CertifyOldSnapshots(withLimit)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, 1, chunkCalls)
	for _, name := range []string{"wal_snapshots", "wal_branches", "wal_log", "wal_reclaim_floors"} {
		require.True(t, calls[name], name)
	}
	pending, err := h.db.Collection("wal_snapshots").CountDocuments(t.Context(), bson.M{"old_key_check": nil})
	require.NoError(t, err)
	require.EqualValues(t, 2, pending)
}

func TestSource_SnapshotCertificationConcurrentReads(t *testing.T) {
	h := newHistoryFixture(t, "certification-concurrent")
	ctx := t.Context()
	at := h.put("book", 1)
	manifests, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	base := snapshot.NewMongoChunkStore(h.db)
	arrived := make(chan string, 2)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	hold := certificationStore{ChunkStore: base, read: func(ctx context.Context, id string) ([]byte, error) {
		arrived <- id
		select {
		case <-gate:
			return base.Get(ctx, id)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	var readers []*snapshot.Service
	for range 2 {
		s, err := snapshot.NewServiceWithStore(h.db, h.branches, materializer.NewService(h.wal, h.branches), hold)
		require.NoError(t, err)
		readers = append(readers, s)
	}
	markSnapshotsUnchecked(t, h.db, "docs", 1)
	type coverage struct {
		lsn int64
		ok  bool
		err error
	}
	done := make(chan coverage, 2)
	for _, s := range readers {
		go func() {
			lsn, ok, err := s.UsableLSN(h.main, "docs", 0, at, at)
			done <- coverage{lsn, ok, err}
		}()
	}
	for range readers {
		select {
		case id := <-arrived:
			require.Equal(t, manifests[0].ChunkIDs[0], id)
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent readers did not both reach the unchecked manifest")
		}
	}
	release()
	for range readers {
		got := <-done
		require.NoError(t, got.err)
		require.True(t, got.ok)
		require.Equal(t, at, got.lsn)
	}
	var m snapshot.Snapshot
	require.NoError(t, h.db.Collection("wal_snapshots").FindOne(ctx, bson.M{"lsn": at}).Decode(&m))
	require.Equal(t, "trusted", m.OldKeyCheck)
	require.NotNil(t, m.OldKeysTrusted)
	require.True(t, *m.OldKeysTrusted)
	reads := 0
	_, err = snapshot.NewServiceWithStore(h.db, h.branches, materializer.NewService(h.wal, h.branches),
		certificationStore{ChunkStore: base, read: func(ctx context.Context, id string) ([]byte, error) {
			reads++
			return base.Get(ctx, id)
		}})
	require.NoError(t, err)
	require.Zero(t, reads)
}

func TestSource_SnapshotCertificationConcurrentReclaim(t *testing.T) {
	h := newHistoryFixture(t, "certification-concurrent-gc")
	ctx := t.Context()
	publishing, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "update" && e.Command.Lookup("update").StringValue() == "wal_snapshots" {
			close(publishing)
			<-gate
		}
	}}
	first := newSnapshotFixture(t, electionClient(t, metadataDeploymentURI, monitor).Database(h.db.Name()))
	id := primitive.Binary{Subtype: 2, Data: []byte{9}}
	at := insertOldBinaryEntry(t, h.db, h.main, "docs", wal.DocumentIDString(id), wal.OpPut, oldBinaryDocument(id, "trace"))
	require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
	main := h.reload()
	manifest := insertReleasedSnapshot(t, h.db, main, "docs", at, "", nil)
	// GC publishes the floor before deleting its documents. Only the first
	// process sees the last subtype-2 entry still awaiting physical removal.
	require.NoError(t, h.wal.RecordReclaimFloor(ctx, main.ID, "docs", at))
	type check struct {
		trusted bool
		err     error
	}
	result := make(chan check, 1)
	go func() {
		_, ok, err := first.snapshots.UsableLSN(main, "docs", 0, at, at)
		result <- check{ok, err}
	}()
	select {
	case <-publishing:
	case <-time.After(3 * time.Second):
		t.Fatal("first check did not reach publication")
	}
	removed, err := h.wal.DeleteDataEntriesUpTo(main.ID, "docs", at)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	_, ok, err := h.snapshots.UsableLSN(main, "docs", 0, at, at)
	require.NoError(t, err)
	require.True(t, ok)
	release()
	got := <-result
	require.NoError(t, got.err)
	require.False(t, got.trusted)
	var saved snapshot.Snapshot
	require.NoError(t, h.db.Collection("wal_snapshots").FindOne(ctx, bson.M{"_id": manifest}).Decode(&saved))
	require.Equal(t, "reclaimed", saved.OldKeyCheck)
	require.NotNil(t, saved.OldKeysTrusted)
	require.False(t, *saved.OldKeysTrusted)
}
