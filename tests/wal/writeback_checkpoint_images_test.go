package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestResync_RecapturesUnverifiedBeforeImage(t *testing.T) {
	for _, provenance := range []string{"attached unverified", "legacy snapshot"} {
		t.Run(provenance, func(t *testing.T) {
			t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
			ctx := t.Context()
			f := newFollowFixture(t, reproSourceURI(t))
			catalog := f.source.Collection("catalog")
			item := bson.D{{Key: "_id", Value: "parcel"}, {Key: "stock", Value: int32(3)}}
			_, err := catalog.InsertOne(ctx, item)
			require.NoError(t, err)
			f.importOnline(t, walcli.OnlineImport{})
			main := f.main(t)
			db := f.services.Client.Database(f.metadataName)
			if provenance == "attached unverified" {
				// Model a checkpoint whose attached image only establishes decoded values.
				marked, err := db.Collection("wal_log").UpdateOne(ctx,
					bson.M{"branch_id": main.ID, "collection": "catalog", "document_id": "parcel"},
					bson.M{"$set": bson.M{"metadata.capture_images": "attached_unverified"}})
				require.NoError(t, err)
				require.EqualValues(t, 1, marked.MatchedCount)
			} else {
				// A surviving WAL row covered by a legacy manifest could predate a
				// reclaimed change, even if both images happen to have the same bytes.
				_, err := f.services.Snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
				require.NoError(t, err)
				marked, err := db.Collection("wal_snapshots").UpdateOne(ctx,
					bson.M{"branch_id": main.ID, "collection": "catalog", "lsn": main.HeadLSN},
					bson.M{"$unset": bson.M{"visibility_rules": ""}})
				require.NoError(t, err)
				require.EqualValues(t, 1, marked.ModifiedCount)
			}
			child, err := f.services.Branches.CreateBranch(main.ProjectID, "stock-edits", main.ID)
			require.NoError(t, err)
			writer, err := f.services.WriterFor(f.project, child.Name)
			require.NoError(t, err)
			_, err = writer.Put(ctx, "catalog", bson.M{"_id": "parcel", "stock": int32(4)})
			require.NoError(t, err)
			plan := f.preview(t, child.ID)
			require.Contains(t, plan.Changes[0].SortedImages, "before")
			original, err := catalog.FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
			require.NoError(t, err)
			checkRefused := func() {
				t.Helper()
				_, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
				require.ErrorContains(t, err, "unverified before-image")
				stored := f.plan(t, plan)
				require.Equal(t, merge.StatusPending, stored.Status)
				require.Nil(t, stored.WriteBack)
				require.Empty(t, stored.WriteBackLastError)
				got, err := catalog.FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
				require.NoError(t, err)
				require.Equal(t, original, got)
			}
			checkRefused()
			result := f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true})
			require.EqualValues(t, 1, result.Puts)
			checkRefused()
			fresh := f.preview(t, child.ID)
			require.Len(t, fresh.Changes, 1)
			require.Empty(t, fresh.Changes[0].SortedImages)
			require.Equal(t, original, fresh.Changes[0].BeforeRaw)
			_, err = f.services.WriteBackMerge(ctx, fresh.ID, "", f.sourceURI)
			require.NoError(t, err)
			got, err := catalog.FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
			require.NoError(t, err)
			require.Equal(t, fresh.Changes[0].DocumentRaw, got)
		})
	}
}

func TestWriteBack_BeforeImageAcrossResync(t *testing.T) {
	f, child := planFixture(t)
	ctx := t.Context()
	catalog := f.source.Collection("catalog")
	const id = "item-00"
	key := bson.M{"_id": id}
	original, err := catalog.FindOne(ctx, key).Raw()
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.PutMany(ctx, "catalog", []bson.M{
		{"_id": "added", "stock": int32(7)}, {"_id": id, "stock": int32(4)},
	})
	require.NoError(t, err)
	// Preview on a stopped checkpoint that already lags a production edit.
	_, err = catalog.UpdateByID(ctx, id, bson.M{"$set": bson.M{"stock": int32(33)}})
	require.NoError(t, err)
	plan := f.preview(t, child)
	require.Len(t, plan.Changes, 2)
	// A transient production insert restores the absent key before apply.
	_, err = catalog.InsertOne(ctx, bson.M{"_id": "added", "stock": int32(88)})
	require.NoError(t, err)
	_, err = catalog.DeleteOne(ctx, bson.M{"_id": "added"})
	require.NoError(t, err)
	// Resync records new target images. The old plan still checks its own.
	f.resync(t, f.services, walcli.ResyncSourceOptions{Force: true})
	require.Greater(t, f.main(t).HeadLSN, plan.TargetHead)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	var conflict *merge.ConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, id, conflict.DocumentID)
	require.ErrorIs(t, catalog.FindOne(ctx, bson.M{"_id": "added"}).Err(), mongo.ErrNoDocuments)
	changed, err := catalog.FindOne(ctx, key).Raw()
	require.NoError(t, err)
	require.Equal(t, int32(33), changed.Lookup("stock").Int32())
	_, err = catalog.ReplaceOne(ctx, key, original)
	require.NoError(t, err)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Equal(t, 2, result.Applied)
}

func TestWriteBack_DuplicateBeforeImage(t *testing.T) {
	ctx := t.Context()
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, reproSourceURI(t))
	catalog := f.source.Collection("catalog")
	before := bson.D{{Key: "_id", Value: "parcel"}, {Key: "stock", Value: int32(7)}, {Key: "stock", Value: int32(3)}}
	_, err := catalog.InsertOne(ctx, before)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	child, err := f.services.Branches.CreateBranch(main.ProjectID, "stock-edits", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, child.Name)
	require.NoError(t, err)
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": "parcel", "stock": int32(4)})
	require.NoError(t, err)
	plan := f.preview(t, child.ID)
	require.Empty(t, plan.Changes[0].SortedImages)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "duplicate fields")
	source, err := catalog.FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
	require.NoError(t, err)
	expected, err := bson.Marshal(before)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expected), source)
	require.Equal(t, source, plan.Changes[0].BeforeRaw)
	stored := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, stored.Status)
	require.Nil(t, stored.WriteBack)
}
