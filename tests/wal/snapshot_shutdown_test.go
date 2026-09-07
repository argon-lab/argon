package wal_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

type pausedAutoPutStore struct {
	snapshot.ChunkStore
	entered     chan struct{}
	resume      chan struct{}
	enteredOnce sync.Once
	resumeOnce  sync.Once
}

func (s *pausedAutoPutStore) Put(ctx context.Context, data []byte) (string, error) {
	s.enteredOnce.Do(func() { close(s.entered) })
	select {
	case <-s.resume:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.ChunkStore.Put(ctx, data)
}
func (s *pausedAutoPutStore) unpause() { s.resumeOnce.Do(func() { close(s.resume) }) }

func TestSnapshot_AutoShutdownWaitsForPublication(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	main, err := f.branches.CreateBranch("auto-shutdown", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "first"})
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	store := &pausedAutoPutStore{ChunkStore: snapshot.NewMongoChunkStore(db), entered: make(chan struct{}), resume: make(chan struct{})}
	snapshots, err := snapshot.NewServiceWithStore(db, f.branches, materializer.NewService(f.wal, f.branches), store)
	require.NoError(t, err)
	defer func() { store.unpause(); require.NoError(t, snapshots.WaitAuto(ctx)) }()
	snapshots.EnableAuto(snapshot.AutoConfig{Threshold: 1, CheckEvery: 1})
	snapshots.MaybeSnapshot(main)
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("auto snapshot did not reach chunk publication")
	}
	locks, err := db.Collection("wal_snapshot_locks").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.Equal(t, int64(1), locks, "paused publication owns the durable lock")
	done := make(chan error, 1)
	go func() { done <- snapshots.WaitAuto(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before publication finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	store.unpause()
	require.NoError(t, <-done)
	locks, err = db.Collection("wal_snapshot_locks").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.Zero(t, locks, "successful shutdown waits through deferred lock release")
	manifests, err := snapshots.ListSnapshots(ctx, main.ID)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "graceful shutdown completes the admitted snapshot")

	// A service that has shut down cannot restart auto work, even if callers
	// attempt to enable it again after advancing the branch.
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "second"})
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	snapshots.EnableAuto(snapshot.AutoConfig{Threshold: 1, CheckEvery: 1, Synchronous: true})
	snapshots.MaybeSnapshot(main)
	require.NoError(t, snapshots.WaitAuto(ctx))
	after, err := snapshots.ListSnapshots(ctx, main.ID)
	require.NoError(t, err)
	require.Len(t, after, len(manifests), "shutdown permanently stops automatic task admission")
}

func TestSnapshot_AutoShutdownDeadlineCancelsPublication(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	main, err := f.branches.CreateBranch("auto-shutdown-deadline", "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "first"})
	require.NoError(t, err)
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	store := &pausedAutoPutStore{ChunkStore: snapshot.NewMongoChunkStore(db), entered: make(chan struct{}), resume: make(chan struct{})}
	snapshots, err := snapshot.NewServiceWithStore(db, f.branches, materializer.NewService(f.wal, f.branches), store)
	require.NoError(t, err)
	defer func() { store.unpause(); require.NoError(t, snapshots.WaitAuto(ctx)) }()
	snapshots.EnableAuto(snapshot.AutoConfig{Threshold: 1, CheckEvery: 1})
	snapshots.MaybeSnapshot(main)
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("auto snapshot did not reach chunk publication")
	}
	expired, expire := context.WithCancel(context.Background())
	expire()
	err = snapshots.WaitAuto(expired)
	require.ErrorIs(t, err, context.Canceled, "an incomplete bounded shutdown must not claim success")
	// Timeout canceled the cooperative Put; a second bounded wait observes
	// completion of the independent-context lock cleanup before disconnecting.
	require.NoError(t, snapshots.WaitAuto(ctx))
	locks, err := db.Collection("wal_snapshot_locks").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	require.Zero(t, locks)
	manifests, err := snapshots.ListSnapshots(ctx, main.ID)
	require.NoError(t, err)
	require.Empty(t, manifests, "canceled publication never exposes a partial manifest")
}
