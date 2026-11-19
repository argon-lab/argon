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
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_SavedCLIImages(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, reproSourceURI(t))
	catalog := f.source.Collection("catalog")
	before := bson.D{{Key: "_id", Value: "replace"}, {Key: "shelf", Value: bson.D{{Key: "aisle", Value: int64(0)}, {Key: "zone", Value: int32(0)}}}}
	_, err := catalog.InsertOne(ctx, before)
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	child, err := f.services.Branches.CreateBranch(main.ProjectID, "shelves", main.ID)
	require.NoError(t, err)
	w, err := f.services.WriterFor(f.project, child.Name)
	require.NoError(t, err)
	shelf := bson.D{{Key: "zone", Value: int32(3)}, {Key: "aisle", Value: int64(8)}}
	for _, id := range []string{"insert", "replace"} {
		_, err = w.Put(ctx, "catalog", bson.M{"_id": id, "shelf": shelf})
		require.NoError(t, err)
	}

	type output struct {
		ID      primitive.ObjectID
		Digest  string `json:"approval_digest"`
		Changes []struct {
			Document json.RawMessage
			Before   json.RawMessage
		}
	}
	cli := buildSourceCLI(t)
	var preview struct{ Plan output }
	body := runMergeCLI(t, f, cli, "merge", "preview", "-p", f.project, "-b", child.Name, "-o", "json")
	require.NoError(t, json.Unmarshal(body, &preview))
	var list struct{ Plans []output }
	require.NoError(t, json.Unmarshal(runMergeCLI(t, f, cli, "merge", "list", "-p", f.project, "-o", "json"), &list))
	require.Len(t, list.Plans, 1)
	require.Equal(t, preview.Plan.ID, list.Plans[0].ID)
	require.Len(t, preview.Plan.Changes, 2)
	require.Len(t, list.Plans[0].Changes, len(preview.Plan.Changes))

	saved, err := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans").FindOne(ctx, bson.M{"_id": preview.Plan.ID}).Raw()
	require.NoError(t, err)
	runMergeCLI(t, f, cli, "merge", "apply", preview.Plan.ID.Hex(), "--write-back", "--approve", preview.Plan.Digest, "-o", "json")
	changes, err := saved.Lookup("changes").Array().Values()
	require.NoError(t, err)
	for i, change := range preview.Plan.Changes {
		var doc bson.Raw
		require.NoError(t, bson.UnmarshalExtJSON(change.Document, true, &doc))
		id := doc.Lookup("_id").StringValue()
		t.Run(id, func(t *testing.T) {
			want, err := bson.Marshal(bson.D{{Key: "_id", Value: id}, {Key: "shelf", Value: shelf}})
			require.NoError(t, err)
			actual, err := catalog.FindOne(ctx, bson.M{"_id": id}).Raw()
			require.NoError(t, err)
			require.Equal(t, imageFromJSON(t, list.Plans[0].Changes[i].Document), doc)
			require.Equal(t, bson.Raw(want), doc)
			require.Equal(t, doc, changes[i].Document().Lookup("document").Document())
			require.Equal(t, doc, actual)
			if id == "replace" {
				var prior bson.Raw
				require.NoError(t, bson.UnmarshalExtJSON(change.Before, true, &prior))
				expected, err := bson.Marshal(before)
				require.NoError(t, err)
				require.Equal(t, imageFromJSON(t, list.Plans[0].Changes[i].Before), prior)
				require.Equal(t, bson.Raw(expected), prior)
				require.Equal(t, prior, changes[i].Document().Lookup("before").Document())
			}
		})
	}
}

// Format 2 readers ignore additional fields. Exercise the historical apply
// path with the same plans and histories used by the source-layout tests.
func useFormat2(t *testing.T, db *mongo.Database, plan *merge.Plan) {
	t.Helper()
	_, err := db.Collection("wal_merge_plans").UpdateByID(t.Context(), plan.ID, bson.M{"$set": bson.M{"format_version": 2}})
	require.NoError(t, err)
}

func TestWriteBack_MixedKeyImages(t *testing.T) {
	for _, format := range []string{"current", "format 2"} {
		t.Run(format, func(t *testing.T) {
			ctx := t.Context()
			db := setupTestDB(t)
			f := newMergeFixture(t, db, "mixed-history")
			const collection = "inventory"
			_, err := f.featWriter.Put(ctx, collection, bson.M{"_id": int32(37), "era": "legacy"})
			require.NoError(t, err)
			// A legacy delete could refer to either a string or ObjectID.
			// A later typed replacement still proves its own BSON bytes.
			modernID := primitive.NewObjectID()
			_, err = f.featWriter.Put(ctx, collection, bson.M{"_id": modernID, "era": "legacy"})
			require.NoError(t, err)
			_, _, err = f.featWriter.Delete(ctx, collection, modernID)
			require.NoError(t, err)
			downgradeToLegacyKeys(t, db, f.snapshotFixture, f.feature.ID, modernID)
			shape := bson.D{{Key: "width", Value: int32(8)}, {Key: "height", Value: int64(4)}}
			raw, err := bson.Marshal(bson.D{{Key: "_id", Value: modernID}, {Key: "shape", Value: shape}})
			require.NoError(t, err)
			lsn, err := f.wal.Append(&wal.Entry{ProjectID: f.feature.ProjectID, BranchID: f.feature.ID,
				Operation: wal.OpPut, Collection: collection, DocumentID: modernID.Hex(), PostImage: raw})
			require.NoError(t, err)
			require.NoError(t, f.branches.UpdateBranchHead(f.feature.ID, lsn))
			plan, err := f.merge.Preview(ctx, f.feature.ID)
			require.NoError(t, err)
			require.Len(t, plan.Changes, 2)
			if format == "format 2" {
				useFormat2(t, db, plan)
			}
			result, err := f.merge.Apply(ctx, plan.ID, "")
			require.NoError(t, err)
			require.Equal(t, 2, result.Applied)
			f.refresh(t)
			merged, err := f.wal.GetDocumentHistory(f.main.ID, collection, modernID.Hex(), plan.TargetHead+1, f.main.HeadLSN)
			require.NoError(t, err)
			require.Len(t, merged, 1)
			require.Equal(t, bson.Raw(raw), merged[0].PostImage)
			if format == "current" {
				data, err := plan.CLIJSON(true)
				require.NoError(t, err)
				var preview struct {
					Changes []struct {
						Sorted []string `json:"sorted_images"`
					}
				}
				require.NoError(t, json.Unmarshal(data, &preview))
				for i, c := range plan.Changes {
					if c.DocumentID == modernID.Hex() {
						require.Empty(t, preview.Changes[i].Sorted)
					} else {
						require.Equal(t, []string{"document"}, preview.Changes[i].Sorted)
					}
				}
			}
		})
	}
}

func imageFromJSON(t *testing.T, data json.RawMessage) bson.Raw {
	t.Helper()
	var raw bson.Raw
	require.NoError(t, bson.UnmarshalExtJSON(data, true, &raw))
	return raw
}

func TestWriteBack_StoredConflictImages(t *testing.T) {
	for _, strategy := range []string{merge.StrategyTheirs, merge.StrategyOurs} {
		t.Run(strategy, func(t *testing.T) {
			ctx := t.Context()
			db := setupTestDB(t)
			f := newMergeFixture(t, db, "shelves")
			put := func(branch *wal.Branch, width int32) bson.Raw {
				t.Helper()
				raw, err := bson.Marshal(bson.D{{Key: "_id", Value: "parcel"}, {Key: "width", Value: width}, {Key: "aisle", Value: int64(3)}})
				require.NoError(t, err)
				lsn, err := f.wal.Append(&wal.Entry{ProjectID: branch.ProjectID, BranchID: branch.ID,
					Operation: wal.OpPut, Collection: "catalog", DocumentID: "parcel", PostImage: raw})
				require.NoError(t, err)
				require.NoError(t, f.branches.UpdateBranchHead(branch.ID, lsn))
				return raw
			}
			base := put(f.main, 2)
			child, err := f.branches.CreateBranch(f.main.ProjectID, "widths", f.main.ID)
			require.NoError(t, err)
			ours, theirs := put(f.main, 4), put(child, 9)
			plan, err := f.merge.Preview(ctx, child.ID)
			require.NoError(t, err)
			loaded, err := f.merge.GetPlan(ctx, plan.ID)
			require.NoError(t, err)
			for _, p := range []*merge.Plan{plan, loaded} {
				data, err := p.CLIJSON(true)
				require.NoError(t, err)
				var out struct {
					Conflicts []struct{ Base, Ours, Theirs json.RawMessage }
				}
				require.NoError(t, json.Unmarshal(data, &out))
				require.Len(t, out.Conflicts, 1)
				require.Equal(t, base, imageFromJSON(t, out.Conflicts[0].Base))
				require.Equal(t, ours, imageFromJSON(t, out.Conflicts[0].Ours))
				require.Equal(t, theirs, imageFromJSON(t, out.Conflicts[0].Theirs))
			}
			_, err = f.wal.DeleteDataEntriesUpTo(child.ID, "catalog", plan.SourceHead)
			require.NoError(t, err)
			result, err := f.merge.Apply(ctx, plan.ID, strategy)
			require.NoError(t, err)
			require.Equal(t, 1, result.ConflictsResolved)
			rows, err := f.wal.GetEntries(bson.M{"txn_id": "merge:" + plan.ID.Hex(), "operation": wal.OpPut})
			require.NoError(t, err)
			if strategy == merge.StrategyOurs {
				require.Empty(t, rows)
				require.Zero(t, result.Applied)
				return
			}
			require.Len(t, rows, 1)
			require.Equal(t, theirs, rows[0].PostImage)
			require.Equal(t, ours, rows[0].PreImage)
		})
	}
}
