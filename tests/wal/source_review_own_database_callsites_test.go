package wal_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wireproxy"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Each non-drop path that goes through Branch.OwnDatabase is run against a
// branch record that was tampered with to name the source database. The
// source sits on the metadata deployment, the only place where the service
// client can reach it by name.
func TestSource_TamperedRecordIsRefusedByEveryWritePath(t *testing.T) {
	type env struct {
		f      *followFixture
		audit  *sourceAudit
		claim  func(branchID string)
		intact func(t *testing.T, label string)
	}
	setup := func(t *testing.T) *env {
		ctx := context.Background()
		f := newFollowFixture(t, metadataDeploymentURI)
		f.seedSmall(t)
		f.importOnline(t, walcli.OnlineImport{})
		expected := f.sourceState(t, nil)
		audit := f.audit(t)
		branches := f.services.Client.Database(f.metadataName).Collection("wal_branches")
		return &env{
			f: f, audit: audit,
			claim: func(branchID string) {
				_, err := branches.UpdateOne(ctx, bson.M{"_id": branchID},
					bson.M{"$set": bson.M{"physical_db": f.source.Name(), "state": "live"}})
				require.NoError(t, err)
			},
			intact: func(t *testing.T, label string) {
				t.Helper()
				names, err := f.source.ListCollectionNames(ctx, bson.D{})
				require.NoError(t, err)
				assert.Equal(t, []string{"catalog"}, names, "%s: the source's collections", label)
				assert.Equal(t, expected, f.sourceState(t, nil), "%s: the source's documents", label)
				assert.Empty(t, audit.operations(t), "%s: changes seen in the source", label)
			},
		}
	}

	t.Run("merge apply into a live target", func(t *testing.T) {
		ctx := context.Background()
		e := setup(t)
		services := e.f.services
		tampered, _ := e.f.agentSandbox(t, "tampered")
		main := e.f.main(t)
		child, err := services.Sandbox.Create(ctx, main.ProjectID, tampered.BranchID, "child", time.Hour)
		require.NoError(t, err)
		require.NoError(t, services.StartCapture(ctx, child.BranchID, "agent:child"))
		agentEdits(t, services.Client.Database(child.PhysicalDB))
		plan := e.f.preview(t, child.BranchID)
		require.Equal(t, tampered.BranchID, plan.TargetBranchID)
		require.Len(t, plan.Changes, 4)

		e.claim(tampered.BranchID)
		_, err = services.Merge.Apply(ctx, plan.ID, "")
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "refusing to use it")
		}
		e.intact(t, "merge apply")
	})

	t.Run("undo apply on a live branch", func(t *testing.T) {
		ctx := context.Background()
		e := setup(t)
		services := e.f.services
		victim, db := e.f.agentSandbox(t, "tampered")
		agentEdits(t, db)
		require.NoError(t, services.SyncBranch(ctx, victim.BranchID))
		branch, err := services.Branches.GetBranchByID(victim.BranchID)
		require.NoError(t, err)
		plan, err := services.Undo.BuildPlan(branch, branch.BaseLSN+1, 0, "")
		require.NoError(t, err)
		require.NotEmpty(t, plan.Compensations)

		e.claim(victim.BranchID)
		branch, err = services.Branches.GetBranchByID(victim.BranchID)
		require.NoError(t, err)
		_, _, err = services.Undo.Apply(ctx, branch, plan)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "refusing to use it")
		}
		e.intact(t, "undo apply")
	})

	t.Run("capture start and sync", func(t *testing.T) {
		ctx := context.Background()
		e := setup(t)
		services := e.f.services
		main := e.f.main(t)
		victim, err := services.Sandbox.Create(ctx, main.ProjectID, main.ID, "tampered", time.Hour)
		require.NoError(t, err)
		e.claim(victim.BranchID)

		err = services.StartCapture(ctx, victim.BranchID, "agent:tampered")
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "refusing to use it")
		}
		for _, status := range services.Ingest.Statuses() {
			if status.BranchID == victim.BranchID {
				assert.NotEqual(t, "running", status.State, "no capture is left watching the source")
			}
		}
		err = services.SyncBranch(ctx, victim.BranchID)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "refusing to use it")
		}
		e.intact(t, "capture")
	})

	t.Run("collections prepare", func(t *testing.T) {
		ctx := context.Background()
		e := setup(t)
		services := e.f.services
		victim, _ := e.f.agentSandbox(t, "tampered")
		e.claim(victim.BranchID)
		err := services.PrepareCollection(ctx, victim.BranchID, "x")
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "refusing to use it")
		}
		e.intact(t, "collections prepare")
	})

	t.Run("wire proxy alias", func(t *testing.T) {
		ctx := context.Background()
		e := setup(t)
		services := e.f.services
		victim, _ := e.f.agentSandbox(t, "tampered")
		e.claim(victim.BranchID)

		proxy := wireproxy.New("localhost:27017", services.Projects, services.Branches)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		proxyCtx, cancel := context.WithCancel(ctx)
		go func() { _ = proxy.Serve(proxyCtx, listener) }()
		t.Cleanup(cancel)

		alias := e.f.project + "~tampered"
		uri := fmt.Sprintf("mongodb://%s/%s?directConnection=true", listener.Addr().String(), alias)
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
		require.NoError(t, err)
		defer func() { _ = client.Disconnect(context.Background()) }()
		writeCtx, cancelWrite := context.WithTimeout(ctx, 20*time.Second)
		defer cancelWrite()
		_, err = client.Database(alias).Collection("catalog").InsertOne(writeCtx, bson.M{"_id": "via-proxy"})
		if assert.Error(t, err, "a write through the alias of a tampered branch is refused") {
			assert.Contains(t, err.Error(), "refusing to use it")
		}
		e.intact(t, "wire proxy")
	})

	t.Run("checkout of a live branch", func(t *testing.T) {
		ctx := context.Background()
		e := setup(t)
		victim, _ := e.f.agentSandbox(t, "tampered")
		e.claim(victim.BranchID)
		info, err := e.f.services.Checkout.Checkout(ctx, victim.BranchID)
		require.Nil(t, info)
		require.ErrorContains(t, err, "refusing to use it")
		e.intact(t, "checkout")
	})
}
