package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Inside a user database only system.* is MongoDB's. A collection called
// config.flags or admin.users is the application's data, and a project that
// says it equals its source has to hold it and follow it.
func TestOnlineImport_DottedCollectionNamesAreData(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	names := []string{"config.flags", "admin.users", "local.cache", "argon_wal.notes"}
	for _, name := range names {
		_, err := f.source.Collection(name).InsertOne(ctx, bson.M{"_id": "one", "v": int32(1)})
		require.NoError(t, err)
	}
	result := f.importOnline(t, walcli.OnlineImport{})
	require.Subset(t, result.Collections, names)
	f.requireMainEqualsSource(t, 0, nil, "after the import")

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	for _, name := range names {
		_, err := f.source.Collection(name).UpdateOne(ctx, bson.M{"_id": "one"}, bson.M{"$set": bson.M{"v": int32(2)}})
		require.NoError(t, err)
	}
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	f.requireMainEqualsSource(t, 0, nil, "after following")
	require.EqualValues(t, 2, f.mainState(t, 0)["config.flags"][wal.DocumentIDString("one")]["v"])
}

// An inexact catch-up is allowed by the run that is given --allow-inexact,
// not for good. A follower stopped in the middle of one and restarted
// without the flag stops at the next update it cannot read exactly, instead
// of quietly storing a later state.
func TestFollow_InexactCatchUpIsAllowedPerRun(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	// A capped collection: MongoDB does not serve it at a past cluster time.
	f.cappedCollection(t, "events")
	var err error
	f.importOnline(t, walcli.OnlineImport{})
	branch := f.main(t)

	// What a run with --allow-inexact leaves behind when it is stopped
	// before it has converged.
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	_, err = state.UpdateOne(ctx, bson.M{"_id": branch.ID}, bson.M{"$set": bson.M{
		"capture_status.source.mode": "catch_up", "capture_status.source.inexact_from_lsn": branch.HeadLSN + 1,
		"capture_status.source.inexact_reason": "an earlier run"}})
	require.NoError(t, err)
	_, err = f.source.Collection("events").UpdateOne(ctx, bson.M{"_id": "e1"}, bson.M{"$set": bson.M{"n": int32(1)}})
	require.NoError(t, err)

	followErr := f.followUntilItStops(t, walcli.FollowSourceOptions{})
	require.ErrorContains(t, followErr, "exact post-image unavailable")
	require.ErrorContains(t, followErr, "not a matter of keeping up")
	require.Equal(t, branch.HeadLSN, f.main(t).HeadLSN)

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{AllowInexact: true})
	info := f.waitCaughtUp(t)
	require.NoError(t, stop())
	require.Equal(t, "exact", info.Mode)
	f.requireMainEqualsSource(t, 0, nil, "after the allowed catch-up")
}

// Something other than the follower writes to a followed main: a process
// from before following existed does not know the branch is read-only. The
// branch then no longer equals the source at its saved position. The
// follower says so and stops; it does not carry on from the foreign head.
func TestFollow_ForeignWriteToMainIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	f.waitCaughtUp(t)
	branchID := f.main(t).ID

	// What an older binary's merge into main does: append, then move the head.
	foreign := func() {
		branch := f.main(t)
		post, err := bson.Marshal(bson.M{"_id": "item-00", "stock": int32(-1)})
		require.NoError(t, err)
		lsn, err := f.services.WAL.Append(&wal.Entry{ProjectID: branch.ProjectID, BranchID: branch.ID, Operation: wal.OpPut,
			Collection: "catalog", DocumentID: wal.DocumentIDString("item-00"), PostImage: post, Actor: "merge:old-binary"})
		require.NoError(t, err)
		require.NoError(t, f.services.Branches.UpdateBranchHead(branch.ID, lsn))
	}
	foreign()
	_, err := f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-01"}, bson.M{"$set": bson.M{"stock": int32(5)}})
	require.NoError(t, err)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		f.tick(t)
		// Wait for the follower to report its own failure. Source status
		// can already see a mismatched head before the follower checks it.
		if f.readIngestState(t, branchID).Lookup("capture_status", "state").StringValue() == "degraded" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.ErrorContains(t, stop(), "moved without this follower")
	info, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", info.State)

	// Starting again does not pass over it either: the head on record is
	// not the branch's head.
	foreign()
	require.ErrorContains(t, f.followUntilItStops(t, walcli.FollowSourceOptions{}), "without the follower")

	// A write-back is refused while the branch is known not to be the source.
	sandbox, sandboxDB := f.agentSandbox(t, "agent-run")
	_, err = sandboxDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-05"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	plan := f.preview(t, sandbox.BranchID)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.ErrorContains(t, err, "degraded")
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)

	// A resync is the way back.
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI})
	require.NoError(t, err)
	f.requireMainEqualsSource(t, 0, nil, "after the resync")

	// A saved position that another process replaced (an older checkout
	// deletes it and writes its own) is not resumed from.
	branch := f.main(t)
	state := f.services.Client.Database(f.metadataName).Collection("wal_ingest_state")
	_, err = state.DeleteOne(ctx, bson.M{"_id": branch.ID})
	require.NoError(t, err)
	_, err = state.InsertOne(ctx, bson.M{"_id": branch.ID, "start_at": f.sourceTime(t)})
	require.NoError(t, err)
	require.ErrorContains(t, f.followUntilItStops(t, walcli.FollowSourceOptions{}), "replaced by another process")
}

// Following preserves claims of any age whose transactions it has not seen.
func TestWriteBack_UnobservedClaimsStayClaimed(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	claim := func(name string, age time.Duration) *merge.Plan {
		info, sandboxDB := f.agentSandbox(t, name)
		_, err := sandboxDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-05"}, bson.M{"$set": bson.M{"stock": int32(1)}})
		require.NoError(t, err)
		plan := f.preview(t, info.BranchID)
		_, err = f.services.Client.Database(f.metadataName).Collection("wal_merge_plans").UpdateOne(ctx, bson.M{"_id": plan.ID}, bson.M{"$set": bson.M{
			"status": merge.StatusWritingBack,
			"write_back": bson.M{"session": primitive.Binary{Subtype: 4, Data: []byte(primitive.NewObjectID().Hex()[:16])},
				"database": f.source.Name(), "changes": 1, "started_at": time.Now().Add(-age)},
		}})
		require.NoError(t, err)
		return plan
	}
	old, young := claim("agent-old", 30*time.Minute), claim("agent-young", time.Minute)
	_, err := f.services.WriteBackMerge(ctx, old.ID, "", f.sourceURI, approvalFor(t, old))
	require.ErrorContains(t, err, "will not be written again")

	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-00"}, bson.M{"$set": bson.M{"stock": int32(3)}})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	for _, plan := range []*merge.Plan{old, young} {
		claimed := f.plan(t, plan)
		require.Equal(t, merge.StatusWritingBack, claimed.Status)
		require.NotNil(t, claimed.WriteBack)
		require.Empty(t, claimed.WriteBackLastError)
	}
}

// Removing an unfinished import removes the snapshots taken of it too. They
// are a copy of the imported data, kept outside the rows the cleanup deletes.
func TestImportCleanup_RemovesSnapshotsOfTheUnpublishedProject(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	project, err := f.services.Projects.BeginImport(ctx, "crashed-"+f.project)
	require.NoError(t, err)
	post, err := bson.Marshal(bson.M{"_id": "a", "secret": "imported data"})
	require.NoError(t, err)
	lsn, err := f.services.WAL.Append(&wal.Entry{ProjectID: project.ID, BranchID: project.MainBranchID, Operation: wal.OpPut,
		Collection: "docs", DocumentID: wal.DocumentIDString("a"), PostImage: post, Actor: "importer"})
	require.NoError(t, err)
	require.NoError(t, f.services.Branches.UpdateBranchHead(project.MainBranchID, lsn))
	_, err = f.services.Snapshots.CreateSnapshot(ctx, project.MainBranchID, lsn)
	require.NoError(t, err)
	metadata := f.services.Client.Database(f.metadataName)
	count := func(name string) int64 {
		n, err := metadata.Collection(name).CountDocuments(ctx, bson.D{})
		require.NoError(t, err)
		return n
	}
	require.NotZero(t, count("wal_snapshots"))
	require.NotZero(t, count("wal_snapshot_chunks"))

	require.NoError(t, f.services.Projects.CleanupImport(ctx, "crashed-"+f.project))
	require.Zero(t, count("wal_snapshots"))
	require.Zero(t, count("wal_snapshot_chunks"))
	require.Zero(t, count("wal_log"))
}
