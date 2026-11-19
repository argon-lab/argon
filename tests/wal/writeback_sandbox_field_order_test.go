package wal_test

import (
	"bytes"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_SandboxFieldOrder(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		f := newFollowFixture(t, sourceURI)
		ctx := t.Context()
		source := f.source.Collection("catalog")
		direct := f.services.Client.Database(f.metadataName).Collection("direct_updates")

		fields := bson.D{
			{Key: "zone", Value: "A"}, {Key: "price", Value: int32(5)}, {Key: "aisle", Value: "B"},
			{Key: "stock", Value: int32(9)}, {Key: "bin", Value: int32(1)},
		}
		cases := []struct {
			id     string
			fields bson.D
			update bson.M
		}{
			// Replacing an existing field keeps its position.
			{"top", bson.D{{Key: "price", Value: int32(5)}, {Key: "name", Value: "lamp"}},
				bson.M{"$set": bson.M{"price": int32(7)}}},
			// Dotted $set replaces the nested value in its existing position.
			{"nested", bson.D{{Key: "shelf", Value: fields}, {Key: "name", Value: "table"}},
				bson.M{"$set": bson.M{"shelf.price": int32(12)}}},
			// $push appends one document and preserves the existing array elements.
			{"array", bson.D{{Key: "shelves", Value: bson.A{fields, fields}}, {Key: "name", Value: "book"}},
				bson.M{"$push": bson.M{"shelves": bson.D{{Key: "zone", Value: "C"}, {Key: "aisle", Value: "D"}}}}},
		}
		for _, c := range cases {
			doc := append(bson.D{{Key: "_id", Value: c.id}}, c.fields...)
			_, err := source.InsertOne(ctx, doc)
			require.NoError(t, err)
			_, err = direct.InsertOne(ctx, readSourceCase(t, source, c.id))
			require.NoError(t, err)
		}
		f.importOnline(t, walcli.OnlineImport{})
		main := f.main(t)
		info, sandbox := f.agentSandbox(t, "field-order")
		checkedOut := sandbox.Collection("catalog")
		for _, c := range cases {
			entries, err := f.services.WAL.GetDocumentHistory(main.ID, "catalog", c.id, 0, main.HeadLSN)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			stored := entries[0].PostImage
			require.Equal(t, readSourceCase(t, source, c.id), stored)
			got := readSourceCase(t, checkedOut, c.id)
			if !bytes.Equal(got, stored) {
				t.Errorf("checkout of %s: got %s, stored %s", c.id, got, stored)
			}
			for _, coll := range []*mongo.Collection{direct, checkedOut} {
				result, err := coll.UpdateByID(ctx, c.id, c.update)
				require.NoError(t, err)
				require.EqualValues(t, 1, result.ModifiedCount)
			}
		}
		plan := f.preview(t, info.BranchID)
		require.Empty(t, plan.Conflicts)
		require.Len(t, plan.Changes, len(cases))
		result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
		require.NoError(t, err)
		require.Equal(t, len(cases), result.Applied)
		for _, change := range plan.Changes {
			id := change.DocumentID
			got := readSourceCase(t, source, id)
			require.Equal(t, change.DocumentRaw, got, "written bytes must match the preview for %s", id)
			want := readSourceCase(t, direct, id)
			if !bytes.Equal(got, want) {
				t.Errorf("source after update of %s: got %s, direct MongoDB update produced %s", id, got, want)
			}
		}
	})
}
