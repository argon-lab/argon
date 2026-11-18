package wal_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// A source collection has pre/post-images enabled (what the degraded message
// recommends) and a document over 8MB is updated. The change event then
// carries both images and exceeds 16MB, so the server fails every getMore at
// that position. The follower is expected to report degraded; the suspicion
// is that it reports "retrying" forever.
func TestFollow_EventTooLargeToReturnIsDegraded(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if uri == "" {
		uri = metadataDeploymentURI
	}
	ctx := context.Background()
	f := newFollowFixture(t, uri)

	require.NoError(t, f.source.CreateCollection(ctx, "blobs"))
	require.NoError(t, f.source.RunCommand(ctx, bson.D{{Key: "collMod", Value: "blobs"},
		{Key: "changeStreamPreAndPostImages", Value: bson.D{{Key: "enabled", Value: true}}}}).Err())
	big := strings.Repeat("x", 9<<20)
	_, err := f.source.Collection("blobs").InsertOne(ctx, bson.M{"_id": "big", "payload": big, "n": 0})
	require.NoError(t, err)
	_, err = f.source.Collection("blobs").InsertOne(ctx, bson.M{"_id": "small", "n": 0})
	require.NoError(t, err)

	f.importOnline(t, walcli.OnlineImport{})
	f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	headBefore := f.main(t).HeadLSN

	// The update that can never be delivered, then one that could be.
	_, err = f.source.Collection("blobs").UpdateOne(ctx, bson.M{"_id": "big"}, bson.M{"$set": bson.M{"n": 1}})
	require.NoError(t, err)
	_, err = f.source.Collection("blobs").UpdateOne(ctx, bson.M{"_id": "small"}, bson.M{"$set": bson.M{"n": 1}})
	require.NoError(t, err)

	seen := map[string]int{}
	var last *walcli.SourceInfo
	lastError := ""
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		f.tick(t)
		info, err := f.services.SourceStatus(ctx, f.project)
		require.NoError(t, err)
		last = info
		seen[info.State]++
		if info.Error != "" && info.Error != lastError {
			lastError = info.Error
			t.Logf("state=%s error=%s", info.State, info.Error)
		}
		if info.State == "degraded" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("states observed over 40s (polls): %v", seen)
	t.Logf("final: state=%s mode=%s head before=%d head now=%d error=%q", last.State, last.Mode, headBefore, f.main(t).HeadLSN, last.Error)
	require.Equal(t, headBefore, f.main(t).HeadLSN, "main must not have advanced past the undeliverable event")
	// What `argon source status` does: it exits non-zero only on degraded.
	require.Equal(t, "degraded", last.State,
		"a position that fails every time must be reported as degraded; `argon source status` succeeds on %q", last.State)
}
