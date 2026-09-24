package wal_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestWAL_ConcurrentStartupKeepsCurrentIndexes(t *testing.T) {
	ctx := context.Background()
	var drops atomic.Int32
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(getTestMongoURI()).SetMonitor(&event.CommandMonitor{
		Started: func(_ context.Context, event *event.CommandStartedEvent) {
			if event.CommandName == "dropIndexes" {
				drops.Add(1)
			}
		},
	}))
	require.NoError(t, err)
	db := client.Database(fmt.Sprintf("argon_index_start_%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = db.Drop(ctx); _ = client.Disconnect(ctx) })
	initial, err := wal.NewService(db)
	require.NoError(t, err)
	defer initial.Close()
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			service, err := wal.NewService(db)
			if service != nil {
				defer service.Close()
			}
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Zero(t, drops.Load(), "normal startup must not drop/rebuild the current unique index")
}

func TestWAL_StartupMigratesOnlyLegacyIndexes(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	indexes := db.Collection("wal_log").Indexes()
	_, err := indexes.CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "lsn", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "project_id", Value: 1}, {Key: "lsn", Value: 1}}},
	})
	require.NoError(t, err)
	service, err := wal.NewService(db)
	require.NoError(t, err)
	defer service.Close()
	specs, err := indexes.ListSpecifications(ctx)
	require.NoError(t, err)
	var found bool
	for _, spec := range specs {
		require.NotEqual(t, "lsn_1", spec.Name)
		if spec.Name == "project_id_1_lsn_1" {
			found = true
			require.NotNil(t, spec.Unique)
			require.True(t, *spec.Unique)
		}
	}
	require.True(t, found)
}
