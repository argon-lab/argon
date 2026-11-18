package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// An online import whose catch-up replays 64 or more changes over a copy of
// 1,000 or more documents, and then fails, must leave nothing behind.
func TestOnlineImport_FailedImportLeavesNoSnapshot(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		f := newFollowFixture(t, sourceURI)
		ctx := context.Background()
		docs := make([]interface{}, 0, 1500)
		for i := 0; i < 1500; i++ {
			docs = append(docs, bson.D{{Key: "_id", Value: fmt.Sprintf("user-%04d", i)}, {Key: "secret", Value: fmt.Sprintf("production-row-%d", i)}})
		}
		_, err := f.source.Collection("users").InsertMany(ctx, docs)
		require.NoError(t, err)

		meta := f.services.Client.Database(f.metadataName)
		wrote := make(chan error, 1)
		go func() {
			// The binding is written after the stream boundary is taken, so
			// everything below arrives through the change stream.
			deadline := time.Now().Add(30 * time.Second)
			for {
				n, _ := meta.Collection("wal_sources").CountDocuments(ctx, bson.M{})
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					wrote <- fmt.Errorf("the import never bound its source")
					return
				}
				time.Sleep(time.Millisecond)
			}
			late := make([]interface{}, 0, 200)
			for i := 0; i < 200; i++ {
				late = append(late, bson.M{"_id": i})
			}
			if _, err := f.source.Collection("late").InsertMany(ctx, late); err != nil {
				wrote <- err
				return
			}
			wrote <- f.source.Collection("late").Drop(ctx)
		}()

		_, err = f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
			URI: f.sourceURI, Database: f.source.Name(), Project: f.project, BatchSize: 100})
		require.NoError(t, <-wrote)
		require.Error(t, err, "the collection drop during the copy must fail the import")
		t.Logf("import error: %v", err)
		require.Contains(t, err.Error(), "Nothing was published")
		require.NotContains(t, err.Error(), "cleanup incomplete")

		// AbortImport ran: everything it knows about is gone.
		names, err := meta.ListCollectionNames(ctx, bson.M{})
		require.NoError(t, err)
		for _, name := range names {
			n, err := meta.Collection(name).CountDocuments(ctx, bson.M{})
			require.NoError(t, err)
			t.Logf("after abort: %s = %d", name, n)
		}
		logs, _ := meta.Collection("wal_log").CountDocuments(ctx, bson.M{})
		branches, _ := meta.Collection("wal_branches").CountDocuments(ctx, bson.M{})
		require.Zero(t, logs)
		require.Zero(t, branches)
		_, lookupErr := f.services.Projects.GetProjectByName(f.project)
		require.Error(t, lookupErr)

		manifests, err := meta.Collection("wal_snapshots").CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		chunks, err := meta.Collection("wal_snapshot_chunks").CountDocuments(ctx, bson.M{})
		require.NoError(t, err)
		var manifest bson.M
		_ = meta.Collection("wal_snapshots").FindOne(ctx, bson.M{}).Decode(&manifest)
		delete(manifest, "collections")
		t.Logf("after abort: wal_snapshots = %d, wal_snapshot_chunks = %d, manifest = %v", manifests, chunks, manifest)

		require.Zero(t, manifests, "a failed import left snapshot manifests of the unpublished project")
		require.Zero(t, chunks, "a failed import left snapshot chunks (a copy of the source data)")
	})
}
