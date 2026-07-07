package wal_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/argon-lab/argon/internal/snapshot"
	"github.com/argon-lab/argon/internal/wal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// chunkStoreBackends returns every backend available in this environment.
// MongoDB and filesystem always run; S3 runs against an S3-compatible
// endpoint (MinIO in CI and local dev) when ARGON_TEST_S3_ENDPOINT is set.
func chunkStoreBackends(t *testing.T) map[string]snapshot.ChunkStore {
	t.Helper()
	backends := make(map[string]snapshot.ChunkStore)

	backends["mongodb"] = snapshot.NewMongoChunkStore(setupTestDB(t))

	fsStore, err := snapshot.NewFilesystemChunkStore(t.TempDir())
	require.NoError(t, err)
	backends["filesystem"] = fsStore

	if endpoint := os.Getenv("ARGON_TEST_S3_ENDPOINT"); endpoint != "" {
		bucket := os.Getenv("ARGON_TEST_S3_BUCKET")
		if bucket == "" {
			bucket = "argon-test"
		}
		ensureTestBucket(t, endpoint, bucket)
		s3Store, err := snapshot.NewS3ChunkStore(context.Background(), snapshot.S3Config{
			Bucket:   bucket,
			Prefix:   fmt.Sprintf("test-%d", os.Getpid()),
			Endpoint: endpoint,
		})
		require.NoError(t, err)
		backends["s3"] = s3Store
	} else {
		t.Log("ARGON_TEST_S3_ENDPOINT not set; skipping the s3 backend")
	}

	return backends
}

// ensureTestBucket creates the test bucket on the S3-compatible endpoint if
// it does not exist, so neither CI nor local runs need extra orchestration.
func ensureTestBucket(t *testing.T, endpoint, bucket string) {
	t.Helper()
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	require.NoError(t, err)
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if !errors.As(err, &owned) && !errors.As(err, &exists) {
			t.Fatalf("failed to ensure test bucket: %v", err)
		}
	}
}

// TestChunkStore_Contract verifies every backend against the behavior the
// snapshot layer depends on: content-addressed round-trips, idempotent
// deduplicating puts, idempotent deletes, and errors for missing chunks.
func TestChunkStore_Contract(t *testing.T) {
	ctx := context.Background()

	for name, store := range chunkStoreBackends(t) {
		t.Run(name, func(t *testing.T) {
			payload := []byte("chunk-payload-" + name)

			id, err := store.Put(ctx, payload)
			require.NoError(t, err)
			require.NotEmpty(t, id)

			// Content-addressed: same content, same ID; put is idempotent.
			id2, err := store.Put(ctx, payload)
			require.NoError(t, err)
			assert.Equal(t, id, id2, "identical content must deduplicate to one address")

			got, err := store.Get(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, payload, got)

			// Different content, different ID.
			other, err := store.Put(ctx, []byte("other-"+name))
			require.NoError(t, err)
			assert.NotEqual(t, id, other)

			// Large chunk (multi-MB) round-trips intact.
			big := make([]byte, 6*1024*1024)
			for i := range big {
				big[i] = byte(i * 31)
			}
			bigID, err := store.Put(ctx, big)
			require.NoError(t, err)
			gotBig, err := store.Get(ctx, bigID)
			require.NoError(t, err)
			assert.Equal(t, big, gotBig)

			// Delete is effective and idempotent.
			require.NoError(t, store.Delete(ctx, []string{id, bigID}))
			_, err = store.Get(ctx, id)
			assert.Error(t, err, "deleted chunk must not load")
			require.NoError(t, store.Delete(ctx, []string{id}), "double delete is not an error")

			// The other chunk is untouched.
			gotOther, err := store.Get(ctx, other)
			require.NoError(t, err)
			assert.Equal(t, []byte("other-"+name), gotOther)

			_, err = store.Get(ctx, "0000000000000000000000000000000000000000000000000000000000000000")
			assert.Error(t, err, "missing chunk must error")
		})
	}
}

// TestChunkStore_SnapshotsEndToEnd runs the snapshot lifecycle (create,
// materialize, branch delete + chunk reclamation) on each backend.
func TestChunkStore_SnapshotsEndToEnd(t *testing.T) {
	ctx := context.Background()

	for name, store := range chunkStoreBackends(t) {
		t.Run(name, func(t *testing.T) {
			db := setupTestDB(t)
			walService, err := wal.NewService(db)
			require.NoError(t, err)
			branchService, err := branchwal.NewBranchService(db, walService)
			require.NoError(t, err)
			mat := materializer.NewService(walService, branchService)
			snapService, err := snapshot.NewServiceWithStore(db, branchService, mat, store)
			require.NoError(t, err)
			matFull := materializer.NewService(walService, branchService)

			project := "e2e-" + name
			main, err := branchService.CreateBranch(project, "main", "")
			require.NoError(t, err)
			writer := driverwal.NewInterceptor(walService, main, branchService, mat)
			for i := 0; i < 30; i++ {
				_, err := writer.InsertOne(ctx, "docs", bson.M{"_id": fmt.Sprintf("d%02d", i), "n": int32(i)})
				require.NoError(t, err)
			}
			main, _ = branchService.GetBranchByID(main.ID)

			_, err = snapService.CreateSnapshot(ctx, main.ID, main.HeadLSN)
			require.NoError(t, err)

			// More writes, then compare snapshot path against full replay.
			_, err = writer.InsertOne(ctx, "docs", bson.M{"_id": "extra"})
			require.NoError(t, err)
			main, _ = branchService.GetBranchByID(main.ID)

			accelerated, err := mat.MaterializeCollection(main, "docs")
			require.NoError(t, err)
			full, err := matFull.MaterializeCollection(main, "docs")
			require.NoError(t, err)
			assert.Equal(t, full, accelerated, "backend %s: snapshot path must equal full replay", name)
			assert.Len(t, accelerated, 31)

			// Reclamation goes through the same backend.
			manifests, chunks, err := snapService.CleanupBranch(ctx, main.ID)
			require.NoError(t, err)
			assert.Positive(t, manifests)
			assert.Positive(t, chunks, "backend %s: orphaned chunks must be reclaimed from the store", name)
		})
	}
}
