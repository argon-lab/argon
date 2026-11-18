package wal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSource_UnexpectedHeadChangeDegradesStatusAndAdmitsResync(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	branch := f.main(t)
	savedHead := branch.HeadLSN
	sandboxInfo, sandboxDB := f.agentSandbox(t, "catalog-edit")
	_, err := sandboxDB.Collection("catalog").UpdateByID(ctx, "item-01", bson.M{"$set": bson.M{"stock": int32(120)}})
	require.NoError(t, err)
	plan := f.preview(t, sandboxInfo.BranchID)
	require.Len(t, plan.Changes, 1)

	// Simulate the append and head advance of a process that can write to main.
	post, err := bson.Marshal(bson.M{"_id": "item-00", "stock": int32(-1)})
	require.NoError(t, err)
	head, err := f.services.WAL.Append(&wal.Entry{
		ProjectID: branch.ProjectID, BranchID: branch.ID, Operation: wal.OpPut,
		Collection: "catalog", DocumentID: "item-00", PostImage: post, Actor: "merge:old-binary",
	})
	require.NoError(t, err)
	require.NoError(t, f.services.Branches.UpdateBranchHead(branch.ID, head))
	status, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", status.State)
	require.ErrorContains(t, status.HealthError(), "without the follower")
	require.Equal(t, savedHead, status.ConsistentLSN)
	require.Equal(t, head, status.HeadLSN)
	require.Equal(t, int32(-1), f.mainState(t, 0)["catalog"]["item-00"]["stock"])

	// An invalid source address must never be reached after a head mismatch.
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", "invalid-source-uri")
	require.ErrorContains(t, err, "without the follower")
	_, err = f.services.Merge.ApplyToSource(ctx, plan.ID, "", merge.WriteBackTarget{Database: f.source})
	require.ErrorContains(t, err, "without the follower")
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)

	// A no-op on the source after comparison lets the replay reach its
	// boundary without waiting for the source's periodic no-op.
	resync, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: f.sourceURI,
		Progress: func(walcli.ResyncProgress) { f.tick(t) },
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, resync.Puts)
	require.Equal(t, f.sourceState(t, nil), f.mainState(t, 0))
	require.NoError(t, f.status(t).HealthError())
}

func TestSource_StatusKeepsHeadAndPositionTogetherDuringFollowing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newFollowFixture(t, reproSourceURI(t))
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	baseline := f.main(t).HeadLSN
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	const changes = 30
	done := make(chan error, 1)
	go func() {
		for i := range changes {
			_, err := f.source.Collection("catalog").InsertOne(ctx, bson.M{"_id": fmt.Sprintf("new-%d", i)})
			if err != nil {
				done <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, err := f.sourceClient.Database("follow_tick").Collection("ticks").InsertOne(ctx, bson.M{"at": time.Now()})
		done <- err
	}()
	defer func() {
		cancel()
		if done != nil {
			<-done
		}
	}()
	for {
		select {
		case err := <-done:
			done = nil
			require.NoError(t, err)
		default:
		}
		status, err := f.services.SourceStatus(ctx, f.project)
		require.NoError(t, err)
		require.Equal(t, "running", status.State, status.Error)
		require.NoError(t, status.HealthError())
		require.Equal(t, status.HeadLSN, status.ConsistentLSN)
		if status.HeadLSN == baseline+changes {
			break
		}
	}
	require.NoError(t, stop())
}
