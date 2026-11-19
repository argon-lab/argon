package wal_test

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestWriteBack_IdenticalReplacement(t *testing.T) {
	f, branch := planFixture(t)
	ctx := t.Context()
	catalog := f.source.Collection("catalog")
	before := readSourceCase(t, catalog, "item-00")
	p, err := f.services.Merge.PreparePlan(ctx, branch)
	require.NoError(t, err)
	p.Changes = []merge.Change{{Collection: "catalog", DocumentID: "extra", DocumentRaw: sourceCaseImage(t, "extra", bson.D{})}}
	p.Conflicts = []merge.Conflict{{Collection: "catalog", DocumentID: "item-00", OursRaw: before, TheirsRaw: before}}
	require.NoError(t, f.services.Merge.SavePlan(ctx, p))
	digest := approvalFor(t, p)

	result, err := f.services.WriteBackMerge(ctx, p.ID, merge.StrategyTheirs, f.sourceURI, digest)
	require.ErrorContains(t, err, "identical before and after images")
	require.ErrorContains(t, err, "catalog/item-00")
	require.Nil(t, result)
	state := f.plan(t, p)
	require.Equal(t, merge.StatusPending, state.Status)
	require.Nil(t, state.WriteBack)
	require.Empty(t, state.WriteBackLastError)
	require.Nil(t, readSourceCase(t, catalog, "extra"))

	result, err = f.services.WriteBackMerge(ctx, p.ID, merge.StrategyOurs, f.sourceURI, digest)
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	require.Equal(t, before, readSourceCase(t, catalog, "item-00"))
}

func TestWriteBack_UnmodifiedReplacementAborts(t *testing.T) {
	f, branch := approvalFixture(t)
	p := f.preview(t, branch)
	require.Len(t, p.Changes, 2)
	replacement := p.Changes[1]
	require.Equal(t, "item-01", replacement.DocumentID)
	// Send the original image for the replacement only, after admission.
	// The server should match it, report zero modifications, and abort the
	// batch before its final reads.
	reg := bson.NewRegistry()
	rawType := reflect.TypeFor[bson.Raw]()
	encoder, err := reg.LookupEncoder(rawType)
	require.NoError(t, err)
	reg.RegisterTypeEncoder(rawType, bsoncodec.ValueEncoderFunc(func(ec bsoncodec.EncodeContext, w bsonrw.ValueWriter, v reflect.Value) error {
		if bytes.Equal(v.Interface().(bson.Raw), replacement.DocumentRaw) {
			v = reflect.ValueOf(replacement.BeforeRaw)
		}
		return encoder.EncodeValue(ec, w, v)
	}))
	var updates, reads int
	monitor := &event.CommandMonitor{Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
		switch e.CommandName {
		case "update":
			updates++
			require.EqualValues(t, 1, e.Reply.Lookup("n").AsInt64())
			require.Zero(t, e.Reply.Lookup("nModified").AsInt64())
		case "find":
			reads++
		}
	}}
	client := electionClient(t, f.sourceURI, monitor)
	target := merge.WriteBackTarget{Database: client.Database(f.source.Name(), options.Database().SetRegistry(reg))}
	digest := approvalFor(t, p)
	result, err := f.services.Merge.ApplyToSource(t.Context(), p.ID, "", target, digest)
	var mismatch *merge.StoredImageError
	require.ErrorAs(t, err, &mismatch)
	require.Equal(t, "catalog", mismatch.Collection)
	require.Equal(t, replacement.DocumentID, mismatch.DocumentID)
	require.Nil(t, result)
	require.Equal(t, 1, updates)
	require.Equal(t, 2, reads)
	state := f.plan(t, p)
	require.Equal(t, merge.StatusPending, state.Status)
	require.Nil(t, state.WriteBack)
	require.Equal(t, digest, approvalFor(t, state))
	require.Equal(t, err.Error(), state.WriteBackLastError)
	require.Nil(t, readSourceCase(t, f.source.Collection("catalog"), "extra"))
}

func TestWriteBack_CappedCollection(t *testing.T) {
	for _, when := range []string{"before preflight", "after preflight"} {
		t.Run(when, func(t *testing.T) {
			f, branch := planFixture(t)
			ctx := t.Context()
			opts := options.CreateCollection()
			if when == "before preflight" {
				opts.SetCapped(true).SetSizeInBytes(65536)
			}
			require.NoError(t, f.source.CreateCollection(ctx, "stock", opts))
			p, err := f.services.Merge.PreparePlan(ctx, branch)
			require.NoError(t, err)
			for _, key := range []string{"alpha", "beta"} {
				p.Changes = append(p.Changes, merge.Change{Collection: "stock", DocumentID: key, DocumentRaw: sourceCaseImage(t, key, bson.D{})})
			}
			require.NoError(t, f.services.Merge.SavePlan(ctx, p))
			var listings, failures int
			monitor := &event.CommandMonitor{
				Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
					if e.CommandName == "listCollections" {
						listings++
						if when == "after preflight" {
							require.NoError(t, f.source.RunCommand(ctx, bson.D{
								{Key: "convertToCapped", Value: "stock"}, {Key: "size", Value: 65536},
							}).Err())
						}
					}
				},
				Failed: func(_ context.Context, e *event.CommandFailedEvent) {
					if e.CommandName == "find" {
						failures++
					}
				},
			}
			db := electionClient(t, f.sourceURI, monitor).Database(f.source.Name())
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			digest := approvalFor(t, p)
			result, err := f.services.Merge.ApplyToSource(bounded, p.ID, "", merge.WriteBackTarget{Database: db}, digest)
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, 1, listings)
			state := f.plan(t, p)
			require.Equal(t, merge.StatusPending, state.Status)
			require.Nil(t, state.WriteBack)
			require.Equal(t, digest, approvalFor(t, state))
			if when == "before preflight" {
				require.ErrorContains(t, err, "capped collection")
				require.ErrorContains(t, err, "stock/alpha")
				require.Empty(t, state.WriteBackLastError)
				require.Zero(t, failures)
			} else {
				var server mongo.CommandError
				require.ErrorAs(t, err, &server)
				require.Equal(t, "SnapshotUnavailable", server.Name)
				require.ErrorContains(t, err, "capped collections")
				require.Equal(t, err.Error(), state.WriteBackLastError)
				require.Equal(t, 1, failures)
			}
			count, err := db.Collection("stock").CountDocuments(ctx, bson.D{})
			require.NoError(t, err)
			require.Zero(t, count)
		})
	}
}
