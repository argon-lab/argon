package wal_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (f *followFixture) ingestState() *mongo.Collection {
	return f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
}

func (f *followFixture) readIngestState(t *testing.T, branchID string) bson.Raw {
	t.Helper()
	doc, err := f.ingestState().FindOne(context.Background(), bson.M{"_id": branchID}).Raw()
	require.NoError(t, err)
	return doc
}

// Removing the status during preflight must prevent the resync from
// recording its start. Test both detach and state deletion on its own,
// so the branch fence alone cannot make both cases pass.
func TestResync_StartDoesNotRecreateIngestState(t *testing.T) {
	for _, detach := range []bool{false, true} {
		name := "state deleted"
		if detach {
			name = "source detached"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFollowFixture(t, reproSourceURI(t))
			f.seedSmall(t)
			f.importOnline(t, walcli.OnlineImport{})
			branch := f.main(t)
			other := f.restart(t)

			var once sync.Once
			var removeErr error
			removed := false
			monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
				if e.CommandName != "listCollections" || e.DatabaseName != f.source.Name() {
					return
				}
				once.Do(func() {
					if detach {
						removeErr = other.DetachSource(ctx, f.project)
					} else {
						removeErr = other.Ingest.ClearResumeState(ctx, branch.ID)
					}
					removed = true
				})
			}}
			client, err := mongo.Connect(ctx, options.Client().ApplyURI(f.sourceURI).SetMonitor(monitor))
			require.NoError(t, err)
			defer func() { _ = client.Disconnect(ctx) }()

			_, err = f.services.Ingest.Resync(ctx, branch.ID, client.Database(f.source.Name()), ingest.ResyncOptions{
				State: f.services.Materializer, Force: true,
			})
			require.True(t, removed, "the source preflight did not list collections")
			require.NoError(t, removeErr)
			var configuration *ingest.ConfigurationError
			require.ErrorAs(t, err, &configuration)
			after := f.main(t)
			require.Equal(t, branch.HeadLSN, after.HeadLSN)
			if detach {
				require.Empty(t, after.Follows)
			} else {
				require.Equal(t, branch.Follows, after.Follows)
			}
			require.ErrorIs(t, f.ingestState().FindOne(ctx, bson.M{"_id": branch.ID}).Err(), mongo.ErrNoDocuments)
		})
	}
}

// A follower in another process can be opening the source stream while a
// detach and checkout replace its ingest state. Its next report must leave
// the checked-out branch's new state intact.
func TestFollow_StaleFollowerCannotReportIntoCheckoutState(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	branch := f.main(t)
	other := f.restart(t)

	opening, open := make(chan struct{}), make(chan struct{})
	done := startSourceRun(t, func(ctx context.Context) error {
		return f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{
			URI: f.sourceURI,
			Watch: func(ctx context.Context, src *mongo.Database, opts *options.ChangeStreamOptions) (*mongo.ChangeStream, error) {
				close(opening)
				select {
				case <-open:
					return src.Watch(ctx, mongo.Pipeline{}, opts)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		})
	})
	awaitSourceRun(t, opening)

	require.NoError(t, other.DetachSource(ctx, f.project))
	_, err := other.Checkout.Checkout(ctx, branch.ID)
	require.NoError(t, err)
	before := f.readIngestState(t, branch.ID)

	_, err = f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": "after-detach"})
	require.NoError(t, err)
	f.tick(t)
	close(open)
	var configuration *ingest.ConfigurationError
	require.ErrorAs(t, done(), &configuration)

	require.True(t, bytes.Equal(before, f.readIngestState(t, branch.ID)), "the old follower changed the checkout state")
	after := f.main(t)
	require.Empty(t, after.Follows)
	require.Equal(t, branch.HeadLSN, after.HeadLSN)
}

func startSourceRun(t *testing.T, run func(context.Context) error) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result error
	go func() {
		result = run(ctx)
		close(done)
	}()
	wait := func() error {
		select {
		case <-done:
			return result
		case <-time.After(20 * time.Second):
			return errors.New("the source run did not stop")
		}
	}
	t.Cleanup(func() {
		cancel()
		_ = wait()
	})
	return wait
}

func awaitSourceRun(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the source run did not reach the expected step")
	}
}
