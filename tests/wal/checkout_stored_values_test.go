package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/snapshot"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func nestedCodeScope(fields bson.D) primitive.CodeWithScope {
	return primitive.CodeWithScope{Code: "nested.k", Scope: bson.D{{Key: "nested", Value: fields}}}
}

func TestSource_CheckoutStoredValues(t *testing.T) {
	short := bson.RawValue{Type: bson.TypeBinary, Value: []byte{1, 0, 0, 0, 2, 'A'}}
	standard := primitive.Binary{Subtype: 2, Data: []byte{'A'}}
	misindexed := bson.RawValue{Type: bson.TypeArray, Value: mustMarshalBSON(bson.D{{Key: "1", Value: int32(7)}})}
	regex := primitive.Regex{Pattern: "a", Options: "im"}
	unsorted := bson.RawValue{Type: bson.TypeRegex, Value: []byte("a\x00mi\x00")}
	repeated := nestedCodeScope(bson.D{{Key: "k", Value: "old"}, {Key: "k", Value: "current"}})
	current := nestedCodeScope(bson.D{{Key: "k", Value: "current"}})
	doc := func(x any) bson.D { return bson.D{{Key: "_id", Value: "q"}, {Key: "x", Value: x}} }

	for _, tc := range []struct {
		name     string
		put      bson.M
		want     bson.D
		match    bson.M
		miss     bson.M
		oldFrame bool
	}{
		{name: "binary value", put: bson.M{"_id": "q", "x": short}, want: doc(standard), match: bson.M{"x": standard}},
		{name: "binary key", put: bson.M{"_id": short, "!": true},
			want: bson.D{{Key: "_id", Value: standard}, {Key: "!", Value: true}}, match: bson.M{"_id": standard}},
		{name: "array index", put: bson.M{"_id": "q", "x": misindexed}, want: doc(bson.A{int32(7)}),
			match: bson.M{"x.0": 7}, miss: bson.M{"x.1": 7}},
		{name: "regex options", put: bson.M{"_id": "q", "x": unsorted}, want: doc(regex),
			match: bson.M{"x": bson.M{"$eq": regex}}, oldFrame: true},
		{name: "code scope", put: bson.M{"_id": "q", "x": repeated}, want: doc(current),
			match: bson.M{"x": current}, miss: bson.M{"x": repeated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			const collection = "docs"
			h := newHistoryFixture(t, "stored-values")
			at, err := h.writer.Put(ctx, collection, tc.put)
			require.NoError(t, err)
			main := h.reload()
			if tc.oldFrame {
				// The selected frame holds "im"; the retained WAL holds "mi".
				insertReleasedSnapshot(t, h.db, main, collection, at, "q", tc.want)
			}
			check := func(t *testing.T, name string) {
				t.Helper()
				child, err := h.branches.CreateBranch(h.project, name, main.ID)
				require.NoError(t, err)
				dropPhysical(t, h.db.Client(), child.ID)
				co := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
				info, err := co.Checkout(ctx, child.ID)
				require.NoError(t, err)
				coll := h.db.Client().Database(info.PhysicalDB).Collection(collection)
				got, err := coll.FindOne(ctx, tc.match).Raw()
				require.NoError(t, err, "expected predicate to match")
				require.Equal(t, mustMarshalBSON(tc.want), got)
				if tc.miss != nil {
					n, err := coll.CountDocuments(ctx, tc.miss)
					require.NoError(t, err)
					require.Zero(t, n)
				}
			}
			t.Run("checkout", func(t *testing.T) { check(t, "before-snapshot") })

			manifests, err := h.snapshots.CreateSnapshot(ctx, main.ID, at)
			require.NoError(t, err)
			require.Len(t, manifests, 1)
			t.Run("snapshot bytes", func(t *testing.T) {
				require.Len(t, manifests[0].ChunkIDs, 1)
				chunk, err := snapshot.NewMongoChunkStore(h.db).Get(ctx, manifests[0].ChunkIDs[0])
				require.NoError(t, err)
				compressor, err := wal.NewCompressor(nil)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, compressor.Close()) })
				frame, err := compressor.Decompress(chunk)
				require.NoError(t, err)
				// Snapshot fields are in name order. MongoDB moves _id to
				// the front on insertion even when "!" precedes it on disk.
				fields := tc.want
				if fields[1].Key < fields[0].Key {
					fields = bson.D{fields[1], fields[0]}
				}
				require.Equal(t, mustMarshalBSON(fields), bson.Raw(frame).Lookup("d").Document())
			})
			require.NoError(t, h.wal.RecordReclaimFloor(ctx, main.ID, collection, at))
			n, err := h.wal.DeleteDataEntriesUpTo(main.ID, collection, at)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			t.Run("checkout after reclaim", func(t *testing.T) { check(t, "after-snapshot") })
		})
	}
}

func TestWriteBack_SandboxCodeScope(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	ctx := t.Context()
	source := f.source.Collection("docs")
	doc := func(keys bson.D) bson.D {
		return bson.D{{Key: "_id", Value: "q"}, {Key: "x", Value: nestedCodeScope(keys)}, {Key: "z", Value: int32(1)}}
	}
	_, err := source.InsertOne(ctx, doc(bson.D{{Key: "k", Value: "old"}, {Key: "k", Value: "current"}}))
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	info, physical := f.agentSandbox(t, "scope")
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	clean := doc(bson.D{{Key: "k", Value: "current"}})
	replaced, err := source.ReplaceOne(ctx, bson.M{"_id": "q"}, clean)
	require.NoError(t, err)
	require.EqualValues(t, 1, replaced.ModifiedCount)
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	updated, err := physical.Collection("docs").UpdateByID(ctx, "q", bson.M{"$set": bson.M{"z": int32(9)}})
	require.NoError(t, err)
	require.EqualValues(t, 1, updated.ModifiedCount)
	plan := f.preview(t, info.BranchID)
	require.Empty(t, plan.Conflicts)
	require.Len(t, plan.Changes, 1)
	require.Equal(t, mustMarshalBSON(clean), plan.Changes[0].BeforeRaw)
	require.Empty(t, plan.Changes[0].SortedImages)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	got, err := source.FindOne(ctx, bson.M{"_id": "q", "z": int32(9), "x": clean[1].Value}).Raw()
	require.NoError(t, err)
	require.Equal(t, plan.Changes[0].DocumentRaw, got)
}

func TestWriteBack_NestedCodeScopeDocuments(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, metadataDeploymentURI)
	source := f.source.Collection("docs")
	var fields bson.D
	for i, key := range "jihgfedcba" {
		fields = append(fields, bson.E{Key: string(key), Value: int32(i)})
	}
	script := primitive.CodeWithScope{Code: "item.a", Scope: bson.D{
		{Key: "item", Value: fields}, {Key: "copies", Value: bson.A{fields, fields, fields}},
	}}
	doc := func(z int32) bson.D {
		return bson.D{{Key: "_id", Value: "q"}, {Key: "z", Value: z}, {Key: "script", Value: script}, {Key: "a", Value: int32(0)}}
	}
	_, err := source.InsertOne(ctx, doc(1))
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	info, sandbox := f.agentSandbox(t, "nested-scope")
	updated, err := sandbox.Collection("docs").UpdateByID(ctx, "q", bson.M{"$set": bson.M{"z": int32(9)}})
	require.NoError(t, err)
	require.EqualValues(t, 1, updated.ModifiedCount)

	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Changes, 1)
	require.Empty(t, plan.Conflicts)
	require.Empty(t, plan.Changes[0].SortedImages)
	result, err := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	require.Equal(t, mustMarshalBSON(doc(9)), readSourceCase(t, source, "q"))
	require.NoError(t, stop())
}
