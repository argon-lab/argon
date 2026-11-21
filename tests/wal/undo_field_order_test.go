package wal_test

import (
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_LiveUndoUsesCleanNativePreImage(t *testing.T) {
	f := newIngestFixture(t, "undo-native-pre-image")
	ctx := captureContext(t)
	const id = "t"
	users := f.physical.Collection("users")
	_, err := users.InsertOne(ctx, bson.D{
		{Key: "_id", Value: id}, {Key: "z", Value: "new"}, {Key: "b", Value: int32(10)},
		{Key: "pad", Value: strings.Repeat("x", 80)}, {Key: "n", Value: bson.D{{Key: "m", Value: true}}},
	})
	require.NoError(t, err)
	original := decodeEditImage(t, readSourceCase(t, users, id))
	session, err := f.client.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		return users.UpdateByID(sc, id, mongo.Pipeline{
			{{Key: "$unset", Value: "z"}},
			{{Key: "$set", Value: bson.D{{Key: "z", Value: "back"}, {Key: "first", Value: "$b"}}}},
		})
	})
	require.NoError(t, err)
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetDocumentHistory(branch.ID, "users", id, 0, branch.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	last := entries[1]
	require.Equal(t, "attached_unverified", last.Metadata["capture_images"])
	fields, err := last.PostImage.Elements()
	require.NoError(t, err)
	var z []string
	for _, field := range fields {
		if field.Key() == "z" {
			z = append(z, field.Value().StringValue())
		}
	}
	require.Equal(t, []string{"new", "back"}, z)
	require.Equal(t, original, decodeEditImage(t, last.PreImage))
	svc := undo.NewService(f.wal, f.branches, f.client, f.mat)
	plan, err := svc.BuildPlan(branch, last.LSN, last.LSN, "")
	require.NoError(t, err)
	restored, deleted, err := svc.Apply(ctx, branch, plan)
	require.NoError(t, err)
	require.Equal(t, 1, restored)
	require.Zero(t, deleted)
	require.Equal(t, original, decodeEditImage(t, readSourceCase(t, users, id)))
}

func TestWriteBack_UndoRestoresValues(t *testing.T) {
	for _, mode := range []string{"live", "released", "missing pre-image"} {
		t.Run(mode, func(t *testing.T) {
			f := newIngestFixture(t, "undo-values")
			ctx := captureContext(t)
			const id = "ordered"
			coll := f.physical.Collection("users")
			nested := bson.D{{Key: "z", Value: 1}, {Key: "h", Value: 2}, {Key: "w", Value: 3}, {Key: "a", Value: 4}}
			_, err := coll.InsertOne(ctx, bson.D{
				{Key: "_id", Value: id}, {Key: "price", Value: 4}, {Key: "details", Value: nested},
				{Key: "parts", Value: bson.A{nested, nested}}, {Key: "name", Value: "lamp"},
			})
			require.NoError(t, err)
			original := decodeEditImage(t, readSourceCase(t, coll, id))
			_, err = coll.UpdateByID(ctx, id, bson.M{"$set": bson.M{"price": 7}})
			require.NoError(t, err)
			updated := decodeEditImage(t, readSourceCase(t, coll, id))
			require.NoError(t, f.ingest.Stop(ctx, f.branchID))
			branch, err := f.branches.GetBranchByID(f.branchID)
			require.NoError(t, err)
			history, err := f.wal.GetDocumentHistory(branch.ID, "users", id, 0, branch.HeadLSN)
			require.NoError(t, err)
			require.Len(t, history, 2)
			last := history[1]
			require.Equal(t, original, decodeEditImage(t, last.PreImage))
			require.Equal(t, updated, decodeEditImage(t, last.PostImage))

			if mode != "live" {
				require.NoError(t, f.checkout.Release(ctx, branch.ID))
				branch, err = f.branches.GetBranchByID(branch.ID)
				require.NoError(t, err)
			}
			if mode == "missing pre-image" {
				// Early metadata writers stored a replacement without its pre-image.
				allowOldFormatRows(t, f.metaDB)
				res, err := f.metaDB.Collection("wal_log").UpdateOne(ctx, bson.M{"_id": last.ID},
					bson.M{"$unset": bson.M{"pre": "", "metadata": ""}, "$set": bson.M{"actor": "writer"}})
				require.NoError(t, err)
				require.EqualValues(t, 1, res.ModifiedCount)
			}
			svc := undo.NewService(f.wal, f.branches, f.client, f.mat)
			plan, err := svc.BuildPlan(branch, last.LSN, last.LSN, "")
			require.NoError(t, err)
			restored, _, err := svc.Apply(ctx, branch, plan)
			require.NoError(t, err)
			require.Equal(t, 1, restored)
			if mode == "live" {
				require.Equal(t, original, decodeEditImage(t, readSourceCase(t, coll, id)))
				return
			}
			branch, err = f.branches.GetBranchByID(branch.ID)
			require.NoError(t, err)
			entries, err := f.wal.GetBranchEntries(branch.ID, "users", last.LSN+1, branch.HeadLSN)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, updated, decodeEditImage(t, entries[0].PreImage), "the compensation must record the replaced values")
			require.Equal(t, original, decodeEditImage(t, entries[0].PostImage))
		})
	}

	t.Run("2.8.0 writer delete", func(t *testing.T) {
		h := newHistoryFixture(t, "older-writer-undo")
		const key = "part"
		nested := bson.D{{Key: "zone", Value: "west"}, {Key: "aisle", Value: "east"}}
		fields := bson.D{
			{Key: "_id", Value: key}, {Key: "shelf", Value: nested}, {Key: "price", Value: int32(7)},
			{Key: "details", Value: bson.A{nested, nested}}, {Key: "name", Value: "spring"},
		}
		original := bson.Raw(mustMarshalBSON(fields))
		fields[2].Value = int32(9)
		discarded := bson.Raw(mustMarshalBSON(fields))
		// In 2.8.0 WriterFor decoded the prior row into a map and
		// marshalled it in name order when recording a delete.
		values := decodeEditImage(t, original)
		oldPre, err := wal.MarshalDocument(values)
		require.NoError(t, err)
		lsns, err := h.wal.AppendBatch([]*wal.Entry{
			{ProjectID: h.project, BranchID: h.main.ID, Collection: "parts", DocumentID: key, Operation: wal.OpPut, PostImage: original},
			{ProjectID: h.project, BranchID: h.main.ID, Collection: "parts", DocumentID: key, Operation: wal.OpPut, PostImage: discarded, PreImage: original},
		})
		require.NoError(t, err)
		require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, lsns[1]))
		h.reset(lsns[0])

		// The entry immediately before the delete is on the discarded timeline.
		deleteLSN, err := h.wal.Append(&wal.Entry{ProjectID: h.project, BranchID: h.main.ID,
			Collection: "parts", DocumentID: key, Operation: wal.OpDelete, PreImage: oldPre})
		require.NoError(t, err)
		require.Equal(t, lsns[1]+1, deleteLSN)
		require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, deleteLSN))
		branch := h.reload()
		svc := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)
		plan, err := svc.BuildPlan(branch, deleteLSN, deleteLSN, "")
		require.NoError(t, err)
		restored, _, err := svc.Apply(t.Context(), branch, plan)
		require.NoError(t, err)
		require.Equal(t, 1, restored)
		entries, err := h.wal.GetDocumentHistory(branch.ID, "parts", key, deleteLSN+1, h.reload().HeadLSN)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, values, decodeEditImage(t, entries[0].PostImage))
	})

	t.Run("edited plan restores BSON null", func(t *testing.T) {
		h := newHistoryFixture(t, "undo-edited-null")
		ctx := t.Context()
		_, err := h.writer.Put(ctx, "docs", bson.M{"_id": "item", "v": bson.M{}})
		require.NoError(t, err)
		at, err := h.writer.Put(ctx, "docs", bson.M{"_id": "item", "v": int32(1)})
		require.NoError(t, err)

		branch := h.reload()
		svc := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)
		plan, err := svc.BuildPlan(branch, at, at, "")
		require.NoError(t, err)
		require.Len(t, plan.Compensations, 1)
		require.Equal(t, bson.M{}, plan.Compensations[0].Restore["v"])
		plan.Compensations[0].Restore["v"] = bson.M(nil)
		restored, _, err := svc.Apply(ctx, branch, plan)
		require.NoError(t, err)
		require.Equal(t, 1, restored)
		entry, err := h.wal.GetEntry(h.project, h.reload().HeadLSN)
		require.NoError(t, err)
		require.Equal(t, bson.TypeNull, entry.PostImage.Lookup("v").Type)
	})
}
