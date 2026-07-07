package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/internal/checkout"
	"github.com/argon-lab/argon/internal/gc"
	"github.com/argon-lab/argon/internal/sandbox"
	"github.com/argon-lab/argon/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func newSandboxFixture(t *testing.T, db *mongo.Database, project string) (*snapshotFixture, *sandbox.Service, string) {
	t.Helper()
	f := newSnapshotFixture(t, db)
	co := checkout.NewService(db.Client(), db, f.branches, f.mat)
	sandboxService := sandbox.NewService(f.branches, co)

	// Reclaim on delete, the way walcli wires it.
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	f.branches.SetDeleteHook(func(branchID string) {
		_, _, _, err := gcService.ReclaimDeletedBranch(context.Background(), branchID)
		require.NoError(t, err)
	})

	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, main)
	_, err = writer.Put(context.Background(), "docs", bson.M{"_id": "seed", "v": int32(1)})
	require.NoError(t, err)
	main, _ = f.branches.GetBranchByID(main.ID)
	return f, sandboxService, main.ID
}

func TestSandbox_Lifecycle(t *testing.T) {
	db := setupTestDB(t)
	f, svc, mainID := newSandboxFixture(t, db, "sbx-life")
	ctx := context.Background()

	info, err := svc.Create(ctx, "sbx-life", mainID, "", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(info.PhysicalDB).Drop(context.Background()) })

	assert.Contains(t, info.BranchName, "sandbox-")
	assert.NotEmpty(t, info.PhysicalDB)
	assert.True(t, info.ExpiresAt.After(time.Now()))

	// The physical database holds the fork state: agents query it directly.
	var doc bson.M
	require.NoError(t, db.Client().Database(info.PhysicalDB).Collection("docs").
		FindOne(ctx, bson.M{"_id": "seed"}).Decode(&doc))
	assert.EqualValues(t, 1, doc["v"])

	// Keep removes the TTL.
	require.NoError(t, svc.Keep(ctx, info.BranchID))
	branch, _ := f.branches.GetBranchByID(info.BranchID)
	assert.Nil(t, branch.ExpiresAt)

	// Discard releases and deletes it; storage reclaims.
	require.NoError(t, svc.Discard(ctx, info.BranchID))
	assert.Zero(t, countBranchEntries(t, f, info.BranchID), "sandbox entries reclaimed")
	_, err = f.branches.GetBranchByID(info.BranchID)
	assert.Error(t, err, "sandbox branch gone")

	// Main is untouched.
	main, _ := f.branches.GetBranchByID(mainID)
	state, err := f.mat.MaterializeCollection(main, "docs")
	require.NoError(t, err)
	assert.Len(t, state, 1)
}

func TestSandbox_SweepReapsOnlyExpired(t *testing.T) {
	db := setupTestDB(t)
	f, svc, mainID := newSandboxFixture(t, db, "sbx-sweep")
	ctx := context.Background()

	expired, err := svc.Create(ctx, "sbx-sweep", mainID, "old", 1*time.Millisecond)
	require.NoError(t, err)
	fresh, err := svc.Create(ctx, "sbx-sweep", mainID, "new", time.Hour)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Client().Database(expired.PhysicalDB).Drop(context.Background())
		_ = db.Client().Database(fresh.PhysicalDB).Drop(context.Background())
	})
	time.Sleep(10 * time.Millisecond)

	report, err := svc.Sweep(ctx, "sbx-sweep")
	require.NoError(t, err)
	assert.Equal(t, []string{"old"}, report.Reaped)
	assert.Empty(t, report.Skipped)

	_, err = f.branches.GetBranchByID(expired.BranchID)
	assert.Error(t, err, "expired sandbox reaped")
	still, err := f.branches.GetBranchByID(fresh.BranchID)
	require.NoError(t, err, "fresh sandbox survives")
	assert.NotNil(t, still.ExpiresAt)

	// Sweep is idempotent.
	report, err = svc.Sweep(ctx, "sbx-sweep")
	require.NoError(t, err)
	assert.Empty(t, report.Reaped)
}

func TestSandbox_ExtendPushesExpiry(t *testing.T) {
	db := setupTestDB(t)
	_, svc, mainID := newSandboxFixture(t, db, "sbx-extend")
	ctx := context.Background()

	info, err := svc.Create(ctx, "sbx-extend", mainID, "extend-me", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Client().Database(info.PhysicalDB).Drop(context.Background()) })

	newExpiry, err := svc.Extend(ctx, info.BranchID, 2*time.Hour)
	require.NoError(t, err)
	assert.True(t, newExpiry.After(info.ExpiresAt), "extension moves the expiry out")

	// Extending a non-sandbox branch fails.
	_, err = svc.Extend(ctx, mainID, time.Hour)
	assert.Error(t, err)
}
