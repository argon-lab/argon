package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_SavedCompoundConflict(t *testing.T) {
	t.Run("write-back", savedCompoundConflictOnSource)
	t.Run("ordinary apply", savedCompoundConflictOnLiveTarget)
	t.Run("ordinary WAL apply", savedCompoundConflictInWAL)
}

func savedCompoundConflictOnSource(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, metadataDeploymentURI)
	ctx := t.Context()
	id := bson.D{{Key: "zone", Value: "east"}, {Key: "bay", Value: int32(12)}}
	catalog := f.source.Collection("catalog")
	_, err := catalog.InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "stock", Value: int32(1)}})
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	f.follow(t, f.services, walcli.FollowSourceOptions{})
	info, sandbox := f.agentSandbox(t, "compound-key")
	_, err = sandbox.Collection("catalog").UpdateByID(ctx, id, bson.M{"$set": bson.M{"stock": int32(2)}})
	require.NoError(t, err)
	_, err = catalog.UpdateByID(ctx, id, bson.M{"$set": bson.M{"stock": int32(3)}})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	plan := f.preview(t, info.BranchID)
	require.Len(t, plan.Conflicts, 1)

	writer := f.restart(t)
	result, err := writer.WriteBackMerge(ctx, plan.ID, merge.StrategyTheirs, f.sourceURI)
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	checkCompoundReplacement(t, catalog, id)
}

func savedCompoundConflictOnLiveTarget(t *testing.T) {
	f := newIngestFixture(t, "merge-compound-key")
	ctx := t.Context()
	stop := f.startIngester(t)
	defer stop()
	id := bson.D{{Key: "zone", Value: "east"}, {Key: "bay", Value: int32(12)}}
	catalog := f.physical.Collection("catalog")
	_, err := catalog.InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "stock", Value: int32(1)}})
	require.NoError(t, err)
	f.waitForEntries(t, "catalog", 1)
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	child, err := f.branches.CreateBranch(main.ProjectID, "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, child)
	ordered := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": id, "stock": int32(2), "nested": ordered})
	require.NoError(t, err)
	_, err = catalog.UpdateByID(ctx, id, bson.M{"$set": bson.M{"stock": int32(3)}})
	require.NoError(t, err)
	f.waitForEntries(t, "catalog", 2)
	preview := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := preview.Preview(ctx, child.ID)
	require.NoError(t, err)
	require.Len(t, plan.Conflicts, 1)
	fresh := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	result, err := fresh.Apply(ctx, plan.ID, merge.StrategyTheirs)
	require.NoError(t, err)
	require.Equal(t, 1, result.Applied)
	checkCompoundReplacement(t, catalog, id)
	doc, err := catalog.FindOne(ctx, bson.M{"_id": id}).Raw()
	require.NoError(t, err)
	expected, err := bson.Marshal(ordered)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expected), doc.Lookup("nested").Document())
}

func savedCompoundConflictInWAL(t *testing.T) {
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "compound-wal")
	ctx := t.Context()
	id := bson.D{{Key: "zone", Value: "east"}, {Key: "bay", Value: int32(12)}}
	key := wal.DocumentIDString(id)
	// An existing WAL image can have any field order, including one a
	// programmatic map produced before its fields were sorted.
	nested := bson.D{{Key: "q", Value: int32(1)}, {Key: "a", Value: int32(2)}, {Key: "r", Value: int32(3)}}
	raw, err := bson.Marshal(bson.D{{Key: "stock", Value: int32(2)}, {Key: "nested", Value: nested}, {Key: "_id", Value: id}})
	require.NoError(t, err)
	lsn, err := f.wal.Append(&wal.Entry{
		ProjectID: f.feature.ProjectID, BranchID: f.feature.ID, Operation: wal.OpPut,
		DocumentID: key, Collection: "catalog", PostImage: raw,
	})
	require.NoError(t, err)
	require.NoError(t, f.branches.UpdateBranchHead(f.feature.ID, lsn))
	_, err = f.mainWriter.Put(ctx, "catalog", bson.M{"_id": id, "stock": int32(3)})
	require.NoError(t, err)

	plan, err := f.merge.Preview(ctx, f.feature.ID)
	require.NoError(t, err)
	require.Len(t, plan.Conflicts, 1)
	fresh := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	_, err = fresh.Apply(ctx, plan.ID, merge.StrategyTheirs)
	require.NoError(t, err)
	f.refresh(t)
	entries, err := f.wal.GetDocumentHistory(f.main.ID, "catalog", key, plan.TargetHead+1, f.main.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, bson.Raw(raw), entries[0].PostImage)
	previous, err := f.wal.GetEntry(f.feature.ProjectID, lsn)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(raw), previous.PostImage)
}

func checkCompoundReplacement(t *testing.T, catalog *mongo.Collection, id bson.D) {
	t.Helper()
	count, err := catalog.CountDocuments(t.Context(), bson.D{})
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	doc, err := catalog.FindOne(t.Context(), bson.M{"_id": id}).Raw()
	require.NoError(t, err)
	expected, err := bson.Marshal(id)
	require.NoError(t, err)
	require.Equal(t, bson.Raw(expected), doc.Lookup("_id").Document())
	require.Equal(t, int32(2), doc.Lookup("stock").Int32())
}

func TestWriteBack_ProgrammaticNestedMapOrder(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	branch, err := f.services.Branches.CreateBranch(main.ProjectID, "field-order", main.ID)
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, branch.Name)
	require.NoError(t, err)
	docs := make([]bson.M, 4)
	for i := range docs {
		docs[i] = bson.M{"_id": int32(180 + i), "fields": bson.M{"j": 1, "b": 1, "g": 1, "a": 1, "d": 1, "f": 1, "e": 1, "i": 1, "c": 1, "h": 1}}
	}
	_, err = writer.PutMany(t.Context(), "catalog", docs)
	require.NoError(t, err)
	plan := f.preview(t, branch.ID)
	result, err := f.services.WriteBackMerge(t.Context(), plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Equal(t, len(docs), result.Applied)
	for _, doc := range docs {
		raw, err := f.source.Collection("catalog").FindOne(t.Context(), bson.M{"_id": doc["_id"]}).Raw()
		require.NoError(t, err)
		elements, err := raw.Lookup("fields").Document().Elements()
		require.NoError(t, err)
		var keys string
		for _, element := range elements {
			keys += element.Key()
		}
		require.Equal(t, "abcdefghij", keys)
	}
}

func TestWriteBack_RequiresVerifiedSandboxImage(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	ctx := t.Context()
	for _, mode := range []string{"attached_unverified", "duplicate", "reclaimed"} {
		t.Run(mode, func(t *testing.T) {
			branch, err := f.services.Branches.CreateBranch(main.ProjectID, mode, main.ID)
			require.NoError(t, err)
			doc := bson.D{{Key: "_id", Value: "new-item"}, {Key: "stock", Value: int32(2)}}
			if mode == "attached_unverified" {
				doc = append(doc, bson.E{Key: "stock", Value: int32(3)})
			}
			if mode == "duplicate" {
				duplicate := bson.D{{Key: "z", Value: int32(1)}, {Key: "z", Value: int32(2)}}
				doc = append(doc, bson.E{Key: "nested", Value: bson.A{bson.A{duplicate}}})
			}
			image, err := bson.Marshal(doc)
			require.NoError(t, err)
			entry := &wal.Entry{
				ProjectID: main.ProjectID, BranchID: branch.ID, Operation: wal.OpPut,
				Collection: "catalog", DocumentID: "new-item", PostImage: image,
			}
			if mode == "attached_unverified" {
				entry.Metadata = map[string]any{"capture_images": mode}
			}
			lsn, err := f.services.WAL.Append(entry)
			require.NoError(t, err)
			require.NoError(t, f.services.Branches.UpdateBranchHead(branch.ID, lsn))
			plan := f.preview(t, branch.ID)
			if mode == "reclaimed" {
				require.NoError(t, f.services.WAL.RecordReclaimFloor(ctx, branch.ID, "catalog", lsn))
				_, err = f.services.WAL.DeleteDataEntriesUpTo(branch.ID, "catalog", lsn)
				require.NoError(t, err)
			}
			_, err = f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI)
			require.ErrorContains(t, err, "catalog/new-item")
			require.ErrorContains(t, err, "preview")
			require.ErrorContains(t, err, "sandbox history")
			require.ErrorIs(t, f.source.Collection("catalog").FindOne(ctx, bson.M{"_id": "new-item"}).Err(), mongo.ErrNoDocuments)
			stored := f.plan(t, plan)
			require.Equal(t, merge.StatusPending, stored.Status)
			require.Nil(t, stored.WriteBack)
			require.Empty(t, stored.WriteBackLastError)
		})
	}
}
