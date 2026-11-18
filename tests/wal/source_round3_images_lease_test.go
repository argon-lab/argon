package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestFollow_StopsEnablingImagesWhenTheLeaseExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	_, err := f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})

	interval := ingest.SourceHeartbeatInterval
	ingest.SourceHeartbeatInterval = time.Minute
	t.Cleanup(func() { ingest.SourceHeartbeatInterval = interval })
	branch := f.main(t)
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	var checked int
	err = f.services.Ingest.Follow(ctx, branch.ID, f.source, ingest.FollowOptions{
		OnReady: cancel,
		PrepareSource: func(ctx context.Context, checkLease func(context.Context) error) error {
			changed, err := source.EnableImages(ctx, f.source, []string{"catalog", "scratch"}, func(ctx context.Context) error {
				checked++
				if checked == 2 {
					// Age the heartbeat after the first collMod succeeds and before
					// the next one. Leave the owner ID unchanged.
					if _, err := state.UpdateByID(ctx, branch.ID, bson.M{"$set": bson.M{
						"capture_status.owner.heartbeat_at": time.Unix(1, 0),
					}}); err != nil {
						return err
					}
				}
				return checkLease(ctx)
			})
			return source.ImagesLeftEnabled(err, changed)
		},
	})
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	require.ErrorContains(t, err, "lease for branch main expired")
	require.ErrorContains(t, err, "was enabled on source collection(s) catalog and is still enabled")
	require.Equal(t, 2, checked)
	require.True(t, reproImagesEnabledOn(t, f, "catalog"))
	require.False(t, reproImagesEnabledOn(t, f, "scratch"))
}
