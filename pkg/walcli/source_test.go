package walcli

import (
	"context"
	"testing"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestSourceOperationsRequireOptIn(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "")
	want := RequireExperimentalSource()
	require.Error(t, want)
	ctx := context.Background()
	s := &Services{}

	_, err := s.ImportDatabaseOnline(ctx, OnlineImport{EnableSourceImages: true})
	require.ErrorIs(t, err, want, "online import")
	_, err = s.SourceStatus(ctx, "project")
	require.ErrorIs(t, err, want, "status")
	require.ErrorIs(t, s.FollowSource(ctx, "project", FollowSourceOptions{EnableSourceImages: true}), want, "follow")
	_, err = s.ResyncSource(ctx, "project", ResyncSourceOptions{})
	require.ErrorIs(t, err, want, "resync")
	require.ErrorIs(t, s.DetachSource(ctx, "project"), want, "detach")
	_, err = s.WriteBackMerge(ctx, primitive.NilObjectID, "", "")
	require.ErrorIs(t, err, want, "write-back")

	// The engine services are also reachable directly through Services.
	_, err = s.Importer.ImportDatabase(ctx, importer.ImportOptions{Online: true})
	require.ErrorIs(t, err, want, "importer")
	require.ErrorIs(t, s.Ingest.Follow(ctx, "branch", nil, ingest.FollowOptions{}), want, "ingest follow")
	_, err = s.Ingest.CatchUp(ctx, "branch", nil, primitive.Timestamp{}, 0, "", nil)
	require.ErrorIs(t, err, want, "ingest catch-up")
	_, err = s.Ingest.Resync(ctx, "branch", nil, ingest.ResyncOptions{})
	require.ErrorIs(t, err, want, "ingest resync")
	_, err = s.Merge.ApplyToSource(ctx, primitive.NilObjectID, "", merge.WriteBackTarget{})
	require.ErrorIs(t, err, want, "merge apply to source")
}
