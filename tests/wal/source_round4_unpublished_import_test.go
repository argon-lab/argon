package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestOnlineImport_DeploymentCaptureSkipsUnpublishedProject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	uri := reproSourceURI(t)
	f := newFollowFixture(t, uri)
	f.seedSmall(t)
	metadata := f.services.Client.Database(f.metadataName)
	before, healthy, err := walcli.InspectCaptureHealth(ctx, metadata)
	require.NoError(t, err)
	require.True(t, healthy)
	require.Empty(t, before)

	copyStarted := make(chan struct{}, 1)
	copyPause, resumeCopy := context.WithCancel(ctx)
	hosts := options.Client().ApplyURI(uri).Hosts
	require.NotEmpty(t, hosts)
	address := startSourceCommandProxy(t, hosts[0], func(message []byte) ([]byte, bool) {
		body := sourceWireCommand(message)
		database, _ := body.Lookup("$db").StringValueOK()
		collection, _ := body.Lookup("find").StringValueOK()
		singleBatch, _ := body.Lookup("singleBatch").BooleanOK()
		if database == f.source.Name() && collection == "catalog" && !singleBatch {
			select {
			case copyStarted <- struct{}{}:
			default:
			}
			<-copyPause.Done()
		}
		return nil, false
	})

	done := make(chan error, 1)
	go func() {
		_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
			URI: "mongodb://" + address + "/?directConnection=true", Database: f.source.Name(), Project: f.project,
		})
		done <- err
	}()
	defer func() {
		resumeCopy()
		if done != nil {
			<-done
		}
	}()
	select {
	case <-copyStarted:
	case err := <-done:
		done = nil
		t.Fatalf("import stopped before the copy: %v", err)
	case <-ctx.Done():
		t.Fatal("import did not start copying before the deadline")
	}

	_, err = f.services.Projects.GetProjectByName(f.project)
	require.ErrorContains(t, err, "project not found")
	during, healthy, err := walcli.InspectCaptureHealth(ctx, metadata)
	require.NoError(t, err)
	require.True(t, healthy)
	require.Empty(t, during)

	resumeCopy()
	importErr := <-done
	done = nil
	require.NoError(t, importErr)
	after, healthy, err := walcli.InspectCaptureHealth(ctx, metadata)
	require.NoError(t, err)
	require.True(t, healthy)
	require.Len(t, after, 1)
	require.Equal(t, f.main(t).ID, after[0]["branch_id"])
	require.Equal(t, "stopped", after[0]["state"])
}
