package wal_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestResync_BoundsSelectorsForLargeIDs(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	catalog := f.source.Collection("catalog")
	const documentCount = 184
	const idBytes = 100 * 1024
	docs := make([]interface{}, 0, documentCount)
	suffix := strings.Repeat("x", idBytes-4)
	for i := range documentCount {
		// IDs beginning with "{" require a typed WAL key, which is larger
		// than the original BSON ID. The compare pass must account for it.
		id := fmt.Sprintf("{%03d%s", i, suffix)
		docs = append(docs, bson.M{"_id": id})
	}
	_, err := catalog.InsertMany(ctx, docs)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{BatchSize: 24})

	var compareBatches, pruneBatches int
	info := f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true, Progress: func(p ingest.ResyncProgress) {
		if p.Collection != "catalog" {
			return
		}
		switch p.Phase {
		case "compare":
			compareBatches++
		case "prune":
			pruneBatches++
		}
	}})
	require.EqualValues(t, documentCount, info.Compared)
	require.Zero(t, info.Puts)
	require.Zero(t, info.Deletes)
	require.Greater(t, compareBatches, 1)
	require.Greater(t, pruneBatches, 1)
	require.False(t, f.status(t).ResyncUnfinished)
}
