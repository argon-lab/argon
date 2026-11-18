package wal_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestFollow_SourceViewDDLEventShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	stream, err := f.source.Watch(ctx, mongo.Pipeline{}, options.ChangeStream().SetShowExpandedEvents(true))
	require.NoError(t, err)
	defer func() { _ = stream.Close(ctx) }()
	require.NoError(t, f.source.CreateView(ctx, "in_stock", "catalog", bson.A{}))
	require.NoError(t, f.source.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "in_stock"}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$limit", Value: 1}}}},
	}).Err())
	name := f.source.Name()
	rename := func(from, to string, dropTarget bool) error {
		return f.sourceClient.Database("admin").RunCommand(ctx, bson.D{
			{Key: "renameCollection", Value: name + "." + from}, {Key: "to", Value: name + "." + to},
			{Key: "dropTarget", Value: dropTarget},
		}).Err()
	}
	require.ErrorContains(t, rename("in_stock", "renamed_view", false), "CommandNotSupportedOnView")
	require.ErrorContains(t, rename("catalog", "in_stock", false), "NamespaceExists")
	require.ErrorContains(t, rename("catalog", "in_stock", true), "NamespaceExists")
	require.NoError(t, f.source.Collection("in_stock").Drop(ctx))
	require.NoError(t, f.source.CreateCollection(ctx, "plain"))
	require.NoError(t, rename("plain", "renamed_plain", false))
	require.NoError(t, f.source.Collection("renamed_plain").Drop(ctx))

	type event struct {
		operation string
		name      string
		hasUUID   bool
		viewOn    string
	}
	for _, want := range []event{
		{operation: "create", name: "in_stock", viewOn: "catalog"},
		{operation: "modify", name: "in_stock", viewOn: "catalog"},
		{operation: "drop", name: "in_stock"},
		{operation: "create", name: "plain", hasUUID: true},
		{operation: "rename", name: "plain", hasUUID: true},
		{operation: "drop", name: "renamed_plain", hasUUID: true},
	} {
		require.True(t, stream.Next(ctx), "expected %s of %s: %v", want.operation, want.name, stream.Err())
		raw := stream.Current
		got := event{
			operation: raw.Lookup("operationType").StringValue(), name: raw.Lookup("ns", "coll").StringValue(),
			hasUUID: raw.Lookup("collectionUUID").Type != 0,
		}
		got.viewOn, _ = raw.Lookup("operationDescription", "viewOn").StringValueOK()
		require.Equal(t, want, got)
	}
}
