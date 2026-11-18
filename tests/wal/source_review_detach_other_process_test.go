package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A follower runs in one process; `argon source detach` runs in another.
func TestSource_DetachStopsAFollowerInAnotherProcess(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	catalog := f.source.Collection("catalog")

	// Process A follows.
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	_, err := catalog.InsertOne(ctx, bson.M{"_id": "before-detach"})
	require.NoError(t, err)
	f.waitCaughtUp(t)

	// Process B detaches.
	other := f.restart(t)
	require.NoError(t, other.DetachSource(ctx, f.project))
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	branch := f.main(t)
	require.Empty(t, branch.Follows, "detach made main an ordinary branch")
	headAtDetach := branch.HeadLSN
	n, err := state.CountDocuments(ctx, bson.M{"_id": branch.ID})
	require.NoError(t, err)
	t.Logf("after detach: follows=%q head=%d wal_ingest_state docs=%d", branch.Follows, headAtDetach, n)

	// The source keeps changing.
	for i := 0; i < 5; i++ {
		_, err := catalog.InsertOne(ctx, bson.M{"_id": "after-detach-" + string(rune('a'+i))})
		require.NoError(t, err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		f.tick(t)
		if f.main(t).HeadLSN > headAtDetach {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	branch = f.main(t)
	entries, err := f.services.WAL.GetEntries(bson.M{"branch_id": branch.ID, "lsn": bson.M{"$gt": headAtDetach}})
	require.NoError(t, err)
	var doc bson.M
	stateErr := state.FindOne(ctx, bson.M{"_id": branch.ID}).Decode(&doc)
	t.Logf("later: follows=%q head=%d (was %d) new entries=%d state doc err=%v", branch.Follows, branch.HeadLSN, headAtDetach, len(entries), stateErr)
	for _, e := range entries {
		t.Logf("  lsn=%d actor=%s op=%v doc=%v", e.LSN, e.Actor, e.Operation, e.DocumentID)
	}
	if stateErr == nil {
		t.Logf("  recreated state: actor=%v has resume_token=%v", doc["actor"], doc["resume_token"] != nil)
	}
	stopErr := stop()
	t.Logf("follower in process A ended with: %v", stopErr)

	require.Equal(t, headAtDetach, branch.HeadLSN, "a detached main received source entries from the follower in another process")
	require.Error(t, stateErr, "the follower re-created wal_ingest_state after detach deleted it")
}
