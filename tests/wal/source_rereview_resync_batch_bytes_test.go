package wal_test

import (
	"context"
	"crypto/rand"
	"os"
	"testing"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestResync_BatchesAreBoundedByBytes(t *testing.T) {
	ctx := context.Background()
	sourceURI := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if sourceURI == "" {
		sourceURI = metadataDeploymentURI
	}
	f := newFollowFixture(t, sourceURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	before := f.status(t)

	// The collection is small by document count but too large for one batch.
	const docs = 40
	payload := make([]byte, 512*1024)
	var documentBytes int64
	for i := range docs {
		_, err := rand.Read(payload)
		require.NoError(t, err)
		raw, err := bson.Marshal(bson.D{{Key: "_id", Value: int32(i)}, {Key: "body", Value: payload}})
		require.NoError(t, err)
		documentBytes = int64(len(raw))
		_, err = f.source.Collection("blobs").InsertOne(ctx, bson.Raw(raw))
		require.NoError(t, err)
	}

	var batches []int64
	var puts int64
	result := f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true, Progress: func(p ingest.ResyncProgress) {
		if p.Phase == "compare" && p.Collection == "blobs" {
			batches = append(batches, p.Puts-puts)
		}
		puts = p.Puts
	}})
	require.EqualValues(t, docs, result.Puts)
	require.Greater(t, len(batches), 1, "large documents must be split before hitting the default count limit")
	for _, count := range batches {
		require.LessOrEqual(t, count*documentBytes, int64(16*1024*1024))
	}
	f.requireResynced(t, before, result)
}
