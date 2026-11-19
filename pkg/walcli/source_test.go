package walcli

import (
	"context"
	"os"
	"testing"

	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/merge"
	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

func TestSourceOperationsWithoutOptIn(t *testing.T) {
	const env = "ARGON_EXPERIMENTAL_SOURCE"
	t.Setenv(env, "")
	require.NoError(t, os.Unsetenv(env))
	t.Setenv("MONGODB_URI", "invalid")
	ctx := context.Background()

	// The mock serves empty results for the project and plan lookups.
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock).ShareClient(true))
	db := mt.Client.Database("metadata")
	mt.AddMockResponses(mtest.CreateSuccessResponse()) // project index creation
	projects, err := projectwal.NewProjectService(db, nil, nil)
	require.NoError(t, err)
	s := &Services{Projects: projects, Importer: importer.NewImportService(nil, projects, nil)}
	noProject := mtest.CreateCursorResponse(0, "metadata.wal_projects", mtest.FirstBatch)
	mt.AddMockResponses(noProject, noProject, noProject, noProject)

	_, err = s.ImportDatabaseOnline(ctx, OnlineImport{})
	require.ErrorContains(t, err, "invalid import options", "online import")
	_, err = DryRunOnlineImport(ctx, OnlineImport{})
	require.ErrorContains(t, err, "failed to connect to Argon metadata", "dry run")
	_, err = s.SourceStatus(ctx, "missing")
	require.ErrorContains(t, err, "project not found", "status")
	err = s.FollowSource(ctx, "missing", FollowSourceOptions{})
	require.ErrorContains(t, err, "project not found", "follow")
	_, err = s.ResyncSource(ctx, "missing", ResyncSourceOptions{})
	require.ErrorContains(t, err, "project not found", "resync")
	err = s.DetachSource(ctx, "missing")
	require.ErrorContains(t, err, "project not found", "detach")

	s.Merge = merge.NewService(db, nil, nil, nil, mt.Client)
	noPlan := mtest.CreateCursorResponse(0, "metadata.wal_merge_plans", mtest.FirstBatch)
	mt.AddMockResponses(noPlan, noPlan)
	_, err = s.WriteBackMerge(ctx, primitive.NilObjectID, "", "", "")
	require.ErrorIs(t, err, mongo.ErrNoDocuments, "write-back plan lookup")
	_, err = s.Merge.ApplyToSource(ctx, primitive.NilObjectID, "", merge.WriteBackTarget{}, "")
	require.ErrorIs(t, err, mongo.ErrNoDocuments, "direct write-back plan lookup")
}

func TestSourceExperimentalOptionsRequireOptIn(t *testing.T) {
	const env = "ARGON_EXPERIMENTAL_SOURCE"
	t.Setenv(env, "")
	require.NoError(t, os.Unsetenv(env))
	t.Setenv("MONGODB_URI", "invalid")
	ctx := context.Background()
	s := &Services{} // any metadata or source lookup would fail

	images, autoResync := RequireExperimentalSourceImages(), RequireExperimentalAutoResync()
	require.Error(t, images)
	require.Error(t, autoResync)

	_, err := s.ImportDatabaseOnline(ctx, OnlineImport{EnableSourceImages: true})
	require.ErrorIs(t, err, images)
	_, err = DryRunOnlineImport(ctx, OnlineImport{EnableSourceImages: true})
	require.ErrorIs(t, err, images)
	_, err = importer.DryRunOnline(ctx, importer.ImportOptions{Online: true, EnableSourceImages: true}, nil)
	require.ErrorIs(t, err, images)
	require.ErrorIs(t, s.FollowSource(ctx, "project", FollowSourceOptions{EnableSourceImages: true}), images)
	require.ErrorIs(t, s.FollowSource(ctx, "project", FollowSourceOptions{AutoResync: true}), autoResync)
}
