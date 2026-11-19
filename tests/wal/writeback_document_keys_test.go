package wal_test

import (
	"encoding/json"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWriteBack_StoredDocumentKeys(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, reproSourceURI(t))
	keys := map[string]any{
		"int32": int32(0), "int64": int64(0), "double": float64(0),
		"boolean": false, "string": "", "zero_object_id": primitive.NilObjectID,
		"object_id": primitive.NewObjectID(), "null": nil,
		"empty_binary":     primitive.Binary{Data: []byte{}},
		"empty_old_binary": primitive.Binary{Subtype: 2, Data: []byte{}},
		"old_binary":       primitive.Binary{Subtype: 2, Data: []byte{0, 0, 0, 0}},
		"user_binary":      primitive.Binary{Subtype: 128, Data: []byte{3, 1, 4}},
		"uuid":             primitive.Binary{Subtype: 4, Data: make([]byte, 16)},
		"compound":         bson.D{{Key: "z", Value: int32(3)}, {Key: "a", Value: int64(7)}},
	}
	// Each collection admits just one numeric zero, regardless of BSON width.
	for name, key := range keys {
		_, err := f.source.Collection(name).InsertOne(ctx, bson.D{{Key: "_id", Value: key}, {Key: "qty", Value: int32(1)}})
		require.NoError(t, err)
	}
	f.importOnline(t, walcli.OnlineImport{})
	info, box := f.agentSandbox(t, "typed-keys")
	for name, key := range keys {
		change, err := box.Collection(name).UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$set": bson.M{"qty": int32(2)}})
		require.NoError(t, err, name)
		require.EqualValues(t, 1, change.ModifiedCount, name)
	}
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, len(keys))
	loaded := f.plan(t, plan)
	digest := approvalFor(t, plan)
	require.NoError(t, loaded.CheckApproval(digest))
	for _, c := range loaded.Changes {
		key, err := wal.MarshalDocument(bson.D{{Key: "_id", Value: keys[c.Collection]}})
		require.NoError(t, err)
		for _, image := range []bson.Raw{c.BeforeRaw, c.DocumentRaw} {
			require.Equal(t, key.Lookup("_id"), image.Lookup("_id"), c.Collection)
		}
	}
	result, err := f.services.WriteBackMerge(ctx, loaded.ID, "", f.sourceURI, digest)
	require.NoError(t, err)
	require.Equal(t, len(keys), result.Applied)
	for _, c := range loaded.Changes {
		stored, err := f.source.Collection(c.Collection).FindOne(ctx, bson.M{"_id": keys[c.Collection]}).Raw()
		require.NoError(t, err)
		require.Equal(t, c.DocumentRaw, stored, c.Collection)
	}
}

func TestWriteBack_ExplicitNullDocumentKey(t *testing.T) {
	ctx := t.Context()
	f, branch := planFixture(t)
	w, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = w.Put(ctx, "catalog", bson.M{"_id": nil, "qty": int32(3)})
	require.NoError(t, err)
	p, err := f.services.Merge.PreparePlan(ctx, branch)
	require.NoError(t, err)
	require.Len(t, p.Changes, 1)
	p.ID = primitive.NewObjectID()
	absent := approvalFor(t, p)
	documentKey := func(p *merge.Plan) json.RawMessage {
		body, err := p.CLIJSON(true)
		require.NoError(t, err)
		var out struct{ Changes []map[string]json.RawMessage }
		require.NoError(t, json.Unmarshal(body, &out))
		return out.Changes[0]["document_key"]
	}
	require.Nil(t, documentKey(p))
	p.Changes[0].ID = primitive.Null{}
	want := approvalFor(t, p)
	require.NotEqual(t, absent, want)
	require.NoError(t, f.services.Merge.SavePlan(ctx, p))
	for _, plan := range []*merge.Plan{p, f.plan(t, p)} {
		require.Equal(t, want, approvalFor(t, plan))
		require.Equal(t, "null", string(documentKey(plan)))
		raw, err := bson.Marshal(plan.Changes[0])
		require.NoError(t, err)
		require.Equal(t, bson.TypeNull, bson.Raw(raw).Lookup("document_key").Type)
	}
}
