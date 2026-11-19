package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func shortCountImage(t *testing.T, id string, payload []byte) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(bson.D{
		{Key: "_id", Value: id},
		{Key: "data", Value: primitive.Binary{Subtype: 2, Data: payload}},
		{Key: "after", Value: true},
	})
	require.NoError(t, err)
	bson.Raw(raw).Lookup("data").Value[5] = 1
	return raw
}

func TestWriteBack_OrdinaryMergeMalformedOldBinary(t *testing.T) {
	const id = "contested"
	raw := shortCountImage(t, id, []byte{'a', 'b'})
	ctx := t.Context()
	for _, mode := range []string{"linear", "diverged"} {
		t.Run(mode, func(t *testing.T) {
			f := newMergeFixture(t, setupTestDB(t), "binary-length")
			head, err := f.wal.Append(&wal.Entry{
				ProjectID: f.feature.ProjectID, BranchID: f.feature.ID, Collection: "docs",
				DocumentID: id, Operation: wal.OpPut, PostImage: raw,
			})
			require.NoError(t, err)
			require.NoError(t, f.branches.UpdateBranchHead(f.feature.ID, head))
			if mode == "diverged" {
				_, err = f.mainWriter.Put(ctx, "docs", bson.M{"_id": id, "stock": 8})
				require.NoError(t, err)
			}
			f.refresh(t)
			rows, err := f.wal.GetDocumentHistory(f.feature.ID, "docs", id, head, head)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, raw, rows[0].PostImage)
			p, err := f.merge.Preview(ctx, f.feature.ID)
			require.ErrorIs(t, err, wal.ErrOldBinaryLength)
			require.ErrorContains(t, err, "docs/"+id)
			require.Nil(t, p)
			_, err = f.matFull.MaterializeCollection(f.feature, "docs")
			require.ErrorIs(t, err, wal.ErrOldBinaryLength)
		})
	}
	t.Run("persisted plan", func(t *testing.T) {
		db := setupTestDB(t)
		f := newMergeFixture(t, db, "binary-length")
		_, err := f.featWriter.Put(ctx, "docs", bson.M{"_id": id, "stock": 8})
		require.NoError(t, err)
		p, err := f.merge.Preview(ctx, f.feature.ID)
		require.NoError(t, err)
		_, err = db.Collection("wal_merge_plans").UpdateByID(ctx, p.ID, bson.M{"$set": bson.M{"changes.0.document": raw}})
		require.NoError(t, err)
		_, err = f.merge.Apply(ctx, p.ID, "")
		require.ErrorIs(t, err, wal.ErrOldBinaryLength)
		f.refresh(t)
		require.Equal(t, p.TargetHead, f.main.HeadLSN)
	})
}

func TestSource_ImportAndCaptureMalformedOldBinary(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, reproSourceURI(t))
	ctx := t.Context()
	// A decoder that consumes only the inner count can read the rest of
	// this payload as another BSON field.
	payload := []byte{'a', byte(bson.TypeBoolean), 'x', 0, 1}
	original := shortCountImage(t, "copied", payload)
	_, err := f.source.Collection("docs").InsertOne(ctx, original)
	require.NoError(t, err)
	_, err = f.services.ImportDatabase(ctx, f.sourceURI, f.source.Name(), "quiesced", false, 32, true)
	require.ErrorIs(t, err, wal.ErrOldBinaryLength)

	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	captured := shortCountImage(t, "captured", payload)
	_, err = f.source.Collection("docs").InsertOne(ctx, captured)
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	main := f.main(t)
	for id, image := range map[string]bson.Raw{"copied": original, "captured": captured} {
		rows, err := f.services.WAL.GetDocumentHistory(main.ID, "docs", id, 0, main.HeadLSN)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, image, rows[0].PostImage)
	}
	_, err = f.services.Materializer.MaterializeCollection(main, "docs")
	require.ErrorIs(t, err, wal.ErrOldBinaryLength)
}
