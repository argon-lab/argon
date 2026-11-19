package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_ValueComparisonWithDuplicateSourceFields(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	ctx := t.Context()
	f := newFollowFixture(t, reproSourceURI(t))
	catalog := f.source.Collection("catalog")
	item := bson.D{{Key: "_id", Value: "parcel"}, {Key: "stock", Value: int32(3)}}
	_, err := catalog.InsertOne(ctx, item)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	// Model a checkpoint whose attached image only establishes decoded values.
	log := f.services.Client.Database(f.metadataName).Collection("wal_log")
	marked, err := log.UpdateOne(ctx, bson.M{"branch_id": main.ID, "collection": "catalog", "document_id": "parcel"},
		bson.M{"$set": bson.M{"metadata.capture_images": "attached_unverified"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, marked.MatchedCount)
	child, err := f.services.Branches.CreateBranch(main.ProjectID, "stock-edits", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, child.Name)
	require.NoError(t, err)
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": "parcel", "stock": int32(4)})
	require.NoError(t, err)
	plan := f.preview(t, child.ID)
	// The newer source BSON contains an extra value a map would discard.
	changed := bson.D{item[0], {Key: "stock", Value: int32(25)}, item[1]}
	_, err = catalog.ReplaceOne(ctx, bson.M{"_id": "parcel"}, changed)
	require.NoError(t, err)
	read := func() bson.Raw {
		t.Helper()
		current, err := catalog.FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
		require.NoError(t, err)
		return current
	}
	raw, err := bson.Marshal(changed)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(raw), read())
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorAs(t, err, new(*merge.ConflictError))
	require.Equal(t, bson.Raw(raw), read())
	require.Equal(t, merge.StatusPending, f.plan(t, plan).Status)
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
	require.Equal(t, []string{"before"}, plan.Changes[0].SortedImages)
	_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "duplicate fields")
	source, err := catalog.FindOne(ctx, bson.M{"_id": "parcel"}).Raw()
	require.NoError(t, err)
	expected, err := bson.Marshal(before)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expected), source)
	stored := f.plan(t, plan)
	require.Equal(t, merge.StatusPending, stored.Status)
	require.Nil(t, stored.WriteBack)
}
