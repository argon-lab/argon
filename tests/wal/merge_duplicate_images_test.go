package wal_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func duplicateMergeDocument(id, shape string, units ...int32) bson.D {
	fields := bson.D{}
	for _, n := range units {
		fields = append(fields, bson.E{Key: "units", Value: n})
	}
	doc := bson.D{{Key: "_id", Value: id}}
	switch shape {
	case "top":
		return append(doc, fields...)
	case "nested":
		return append(doc, bson.E{Key: "value", Value: fields})
	case "array":
		return append(doc, bson.E{Key: "value", Value: bson.A{bson.A{fields}}})
	case "scope":
		return append(doc, bson.E{Key: "value", Value: primitive.CodeWithScope{Code: "units", Scope: fields}})
	default:
		panic(shape)
	}
}

func mergeUnits(t *testing.T, raw bson.Raw, shape string) []int32 {
	t.Helper()
	switch shape {
	case "nested":
		raw = raw.Lookup("value").Document()
	case "array":
		raw = raw.Lookup("value", "0", "0").Document()
	case "scope":
		_, raw = raw.Lookup("value").CodeWithScope()
	}
	var values []int32
	fields, err := raw.Elements()
	require.NoError(t, err)
	for _, field := range fields {
		if field.Key() == "units" {
			values = append(values, field.Value().Int32())
		}
	}
	return values
}

// 2.7.0 kept the last field in top-level maps, nested maps and arrays. Its
// Go BSON decoder kept both fields in a code scope's ordered document. The
// single-value rule also applies to scope fields in an ordinary merge.
func TestWriteBack_OrdinaryMergeDuplicateImages(t *testing.T) {
	for _, target := range []string{"wal", "checkout"} {
		for _, strategy := range []string{merge.StrategyOurs, merge.StrategyTheirs} {
			t.Run(target+"/"+strategy, func(t *testing.T) {
				ordinaryDuplicateImages(t, target == "checkout", strategy, false)
			})
		}
		t.Run(target+"/previously-saved", func(t *testing.T) {
			ordinaryDuplicateImages(t, target == "checkout", merge.StrategyTheirs, true)
		})
	}
}

func ordinaryDuplicateImages(t *testing.T, live bool, strategy string, previouslySaved bool) {
	ctx := t.Context()
	db := setupTestDB(t)
	const project = "merge-duplicate-fields"
	var f *snapshotFixture
	var main *wal.Branch
	var physical *ingestFixture
	var err error
	if live {
		physical = newIngestFixtureAt(t, project, db)
		stop := physical.startIngester(t)
		defer stop()
		f = physical.snapshotFixture
		main, err = f.branches.GetBranchByID(physical.branchID)
	} else {
		f = newSnapshotFixture(t, db)
		main, err = f.branches.CreateBranch(project, "main", "")
	}
	require.NoError(t, err)
	service := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	const collection = "catalog"
	physicalWrites := 0
	put := func(branch *wal.Branch, id, shape string, units ...int32) {
		t.Helper()
		doc := duplicateMergeDocument(id, shape, units...)
		if live && branch.ID == main.ID {
			_, err := physical.physical.Collection(collection).ReplaceOne(ctx, bson.M{"_id": id}, doc, options.Replace().SetUpsert(true))
			require.NoError(t, err)
			physicalWrites++
			return
		}
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)
		lsn, err := f.wal.Append(&wal.Entry{ProjectID: project, BranchID: branch.ID, Operation: wal.OpPut, Collection: collection, DocumentID: id, PostImage: raw})
		require.NoError(t, err)
		require.NoError(t, f.branches.UpdateBranchHead(branch.ID, lsn))
	}
	shapes := []string{"top", "nested", "array", "scope"}
	for _, shape := range shapes {
		put(main, "delete-"+shape, shape, 1, 2)
		put(main, "replace-"+shape, shape, 1, 2)
		put(main, "conflict-"+shape, shape, 1, 2)
	}
	if live {
		physical.waitForEntries(t, collection, physicalWrites)
	}
	child, err := f.branches.CreateBranch(project, "edits", main.ID)
	require.NoError(t, err)
	for _, shape := range shapes {
		id := "delete-" + shape
		before, err := bson.Marshal(duplicateMergeDocument(id, shape, 1, 2))
		require.NoError(t, err)
		lsn, err := f.wal.Append(&wal.Entry{ProjectID: project, BranchID: child.ID, Operation: wal.OpDelete, Collection: collection, DocumentID: id, PreImage: before})
		require.NoError(t, err)
		require.NoError(t, f.branches.UpdateBranchHead(child.ID, lsn))
		put(child, "insert-"+shape, shape, 5, 6)
		put(child, "replace-"+shape, shape, 5, 6)
		put(child, "conflict-"+shape, shape, 5, 6)
		if strategy == merge.StrategyOurs {
			put(main, "conflict-"+shape, shape, 4)
		} else {
			put(main, "conflict-"+shape, shape, 3, 4)
		}
	}
	if live {
		physical.waitForEntries(t, collection, physicalWrites)
	}
	plan, err := service.Preview(ctx, child.ID)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 3*len(shapes))
	require.Len(t, plan.Conflicts, len(shapes))
	// Previous format-3 plans could store the complete repeated fields for
	// any target. Restore that record before ordinary apply.
	record := bson.M{}
	checkSaved := func(item, id, field string, data bson.Raw, marks []string, shape string, n int32, duplicate bool) {
		t.Helper()
		assert.Equal(t, []int32{n}, mergeUnits(t, data, shape), "%s %s", id, field)
		if duplicate {
			assert.Contains(t, marks, field, "%s %s", id, field)
			record[item+"."+field] = duplicateMergeDocument(id, shape, n-1, n)
			record[item+".sorted_images"] = bson.A{}
		}
	}
	loaded, err := service.GetPlan(ctx, plan.ID)
	require.NoError(t, err)
	for i, change := range loaded.Changes {
		op, shape, _ := strings.Cut(change.DocumentID, "-")
		item := fmt.Sprintf("changes.%d", i)
		if op != "delete" {
			checkSaved(item, change.DocumentID, "document", change.DocumentRaw, change.SortedImages, shape, 6, true)
		}
		if op != "insert" {
			checkSaved(item, change.DocumentID, "before", change.BeforeRaw, change.SortedImages, shape, 2, true)
		}
	}
	for i, conflict := range loaded.Conflicts {
		_, shape, _ := strings.Cut(conflict.DocumentID, "-")
		item := fmt.Sprintf("conflicts.%d", i)
		checkSaved(item, conflict.DocumentID, "base", conflict.BaseRaw, conflict.SortedImages, shape, 2, true)
		checkSaved(item, conflict.DocumentID, "ours", conflict.OursRaw, conflict.SortedImages, shape, 4, strategy == merge.StrategyTheirs)
		checkSaved(item, conflict.DocumentID, "theirs", conflict.TheirsRaw, conflict.SortedImages, shape, 6, true)
	}
	if previouslySaved {
		_, err := db.Collection("wal_merge_plans").UpdateByID(ctx, plan.ID, bson.M{"$set": record, "$unset": bson.M{"provenance_version": ""}})
		require.NoError(t, err)
	}

	result, err := service.Apply(ctx, plan.ID, strategy)
	require.NoError(t, err)
	if live {
		physical.waitForEntries(t, collection, physicalWrites+result.Applied)
	}
	main, err = f.branches.GetBranchByID(main.ID)
	require.NoError(t, err)
	state, err := f.matFull.MaterializeCollection(main, collection)
	require.NoError(t, err)
	for _, shape := range shapes {
		id := "delete-" + shape
		assert.NotContains(t, state, id)
		if live {
			assert.ErrorIs(t, physical.physical.Collection(collection).FindOne(ctx, bson.M{"_id": id}).Err(), mongo.ErrNoDocuments)
		} else {
			rows, err := f.wal.GetDocumentHistory(main.ID, collection, id, plan.TargetHead+1, main.HeadLSN)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, []int32{2}, mergeUnits(t, rows[0].PreImage, shape))
		}
		for _, op := range []string{"insert", "replace", "conflict"} {
			id := op + "-" + shape
			expected := int32(6)
			if op == "conflict" && strategy == merge.StrategyOurs {
				expected = 4
			}
			rows, err := f.wal.GetDocumentHistory(main.ID, collection, id, 0, main.HeadLSN)
			require.NoError(t, err)
			require.NotEmpty(t, rows)
			row := rows[len(rows)-1]
			image := row.PostImage
			if live {
				image, err = physical.physical.Collection(collection).FindOne(ctx, bson.M{"_id": id}).Raw()
				require.NoError(t, err)
				assert.Equal(t, mergeUnits(t, image, shape), mergeUnits(t, row.PostImage, shape))
			} else if op == "replace" || op == "conflict" && strategy == merge.StrategyTheirs {
				before := int32(2)
				if op == "conflict" {
					before = 4
				}
				assert.Equal(t, []int32{before}, mergeUnits(t, row.PreImage, shape), "%s pre-image", id)
			}
			assert.Equal(t, []int32{expected}, mergeUnits(t, image, shape), "%s raw", id)
			var want, decoded bson.M
			raw, err := bson.Marshal(duplicateMergeDocument(id, shape, expected))
			require.NoError(t, err)
			require.NoError(t, bson.Unmarshal(raw, &want))
			require.NoError(t, bson.Unmarshal(image, &decoded))
			assert.Equal(t, want, decoded, "%s decoded", id)
			assert.Equal(t, want, state[id], "%s materialized", id)
		}
	}
}

func TestWriteBack_OrdinaryMergeDuplicateCompoundID(t *testing.T) {
	f := newMergeFixture(t, setupTestDB(t), "merge-duplicate-id")
	id := bson.D{{Key: "part", Value: 1}, {Key: "part", Value: 2}}
	_, err := f.featWriter.Put(t.Context(), "docs", bson.M{"_id": id, "units": 6})
	require.NoError(t, err)
	_, err = f.merge.Preview(t.Context(), f.feature.ID)
	require.ErrorContains(t, err, "duplicate fields in compound _id")
}

func TestWriteBack_OrdinaryFormat2ScopeOrder(t *testing.T) {
	ctx := t.Context()
	db := setupTestDB(t)
	f := newMergeFixture(t, db, "merge-scope-order")
	const id = "item"
	scope := func(n int32) bson.D {
		return bson.D{{Key: "z", Value: n}, {Key: "a", Value: int32(0)}}
	}
	put := func(branch *wal.Branch, n int32) {
		raw, err := bson.Marshal(bson.D{{Key: "_id", Value: id}, {Key: "value", Value: primitive.CodeWithScope{Code: "z", Scope: scope(n)}}})
		require.NoError(t, err)
		lsn, err := f.wal.Append(&wal.Entry{ProjectID: branch.ProjectID, BranchID: branch.ID, Operation: wal.OpPut, Collection: "docs", DocumentID: id, PostImage: raw})
		require.NoError(t, err)
		require.NoError(t, f.branches.UpdateBranchHead(branch.ID, lsn))
	}
	put(f.main, 1)
	child, err := f.branches.CreateBranch(f.main.ProjectID, "edits", f.main.ID)
	require.NoError(t, err)
	put(child, 2)
	plan, err := f.merge.Preview(ctx, child.ID)
	require.NoError(t, err)
	useFormat2(t, db, plan)
	// A format-2 plan with no matching history row uses the decoded map.
	_, err = f.wal.DeleteDataEntriesUpTo(child.ID, "docs", plan.SourceHead)
	require.NoError(t, err)
	_, err = f.merge.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	f.refresh(t)
	rows, err := f.wal.GetDocumentHistory(f.main.ID, "docs", id, plan.TargetHead+1, f.main.HeadLSN)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for i, image := range []bson.Raw{rows[0].PreImage, rows[0].PostImage} {
		want, err := bson.Marshal(scope(int32(i + 1)))
		require.NoError(t, err)
		_, got := image.Lookup("value").CodeWithScope()
		require.Equal(t, bson.Raw(want), got)
	}
}

func TestWriteBack_OrdinarySortedBeforeChecksLiveKey(t *testing.T) {
	for _, tc := range []struct {
		name          string
		original, now any
	}{
		{"scalar", int32(7), int64(7)},
		{"compound",
			bson.D{{Key: "zone", Value: int32(7)}, {Key: "aisle", Value: "A"}},
			bson.D{{Key: "zone", Value: int64(7)}, {Key: "aisle", Value: "A"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			f := newIngestFixture(t, "merge-reinserted-id")
			catalog := f.physical.Collection("catalog")
			doc := bson.D{{Key: "_id", Value: tc.original}, {Key: "units", Value: int32(1)}, {Key: "units", Value: int32(2)}}
			func() {
				stop := f.startIngester(t)
				defer stop()
				_, err := catalog.InsertOne(ctx, doc)
				require.NoError(t, err)
				f.waitForEntries(t, "catalog", 1)
			}()
			main, err := f.branches.GetBranchByID(f.branchID)
			require.NoError(t, err)
			child, err := f.branches.CreateBranch(main.ProjectID, "edits", main.ID)
			require.NoError(t, err)
			writer := walwriter.New(f.wal, f.branches, f.mat, child)
			_, deleted, err := writer.Delete(ctx, "catalog", tc.original)
			require.NoError(t, err)
			require.True(t, deleted)
			service := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
			plan, err := service.Preview(ctx, child.ID)
			require.NoError(t, err)
			require.Len(t, plan.Changes, 1)
			require.Contains(t, plan.Changes[0].SortedImages, "before")

			_, err = catalog.DeleteOne(ctx, bson.M{"_id": tc.original})
			require.NoError(t, err)
			doc[0].Value = tc.now
			_, err = catalog.InsertOne(ctx, doc)
			require.NoError(t, err)
			key := bson.M{"_id": tc.original}
			expected, err := catalog.FindOne(ctx, key).Raw()
			require.NoError(t, err, "the old key still matches the reinserted document")
			_, err = service.Apply(ctx, plan.ID, "")
			require.ErrorContains(t, err, "physical document")
			got, err := catalog.FindOne(ctx, key).Raw()
			require.NoError(t, err)
			require.Equal(t, expected, got)
			saved, err := service.GetPlan(ctx, plan.ID)
			require.NoError(t, err)
			require.Equal(t, merge.StatusPending, saved.Status)
		})
	}
}

func TestWriteBack_ProgrammaticDuplicateImages(t *testing.T) {
	for _, target := range []string{"checkout", "followed"} {
		t.Run(target, func(t *testing.T) {
			ctx := t.Context()
			var service *merge.Service
			var childID string
			if target == "followed" {
				f, branch := planFixture(t)
				service, childID = f.services.Merge, branch
			} else {
				f := newIngestFixture(t, "merge-save-images")
				child, err := f.branches.CreateBranch("merge-save-images", "edits", f.branchID)
				require.NoError(t, err)
				service = merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
				childID = child.ID
			}
			p, err := service.PreparePlan(ctx, childID)
			require.NoError(t, err)
			const id = "parcel"
			// The raw image is authoritative even when the caller left an old
			// map in the proposal. A field sorting before _id checks key order.
			doc := append(duplicateMergeDocument(id, "scope", 1, 2), bson.E{Key: "0tag", Value: true})
			raw, err := bson.Marshal(doc)
			require.NoError(t, err)
			stale := bson.M{"_id": id, "obsolete": true}
			fromMap := bson.M{"_id": id, "value": bson.D{{Key: "units", Value: int32(3)}, {Key: "units", Value: int32(4)}}}
			p.Changes = []merge.Change{{Collection: "catalog", DocumentID: id, ID: id,
				DocumentRaw: raw, Document: stale, Before: fromMap, SortedImages: []string{"before"}}}
			p.Conflicts = []merge.Conflict{{Collection: "inventory", DocumentID: id,
				BaseRaw: raw, Base: stale, Ours: fromMap, TheirsRaw: raw, SortedImages: []string{"base"}}}
			require.NoError(t, service.SavePlan(ctx, p))
			loaded, err := service.GetPlan(ctx, p.ID)
			require.NoError(t, err)
			for _, plan := range []*merge.Plan{p, loaded} {
				c, conflict := plan.Changes[0], plan.Conflicts[0]
				rawUnits, mapUnits := []int32{1, 2}, []int32{3, 4}
				if target == "checkout" {
					rawUnits, mapUnits = []int32{2}, []int32{4}
					require.ElementsMatch(t, []string{"document", "before"}, c.SortedImages)
					require.ElementsMatch(t, []string{"base", "ours", "theirs"}, conflict.SortedImages)
					require.NotContains(t, c.Document, "obsolete")
					require.NotContains(t, conflict.Base, "obsolete")
					require.Contains(t, conflict.Theirs, "value")
				} else {
					require.Equal(t, []string{"before"}, c.SortedImages)
					require.ElementsMatch(t, []string{"base", "ours"}, conflict.SortedImages)
				}
				for _, image := range []bson.Raw{c.DocumentRaw, conflict.BaseRaw, conflict.TheirsRaw} {
					require.Equal(t, "_id", image.Index(0).Key())
					require.Equal(t, rawUnits, mergeUnits(t, image, "scope"))
					if target == "followed" {
						require.Equal(t, bson.Raw(raw), image)
					}
				}
				for _, image := range []bson.Raw{c.BeforeRaw, conflict.OursRaw} {
					require.Equal(t, mapUnits, mergeUnits(t, image, "nested"))
				}
			}
		})
	}
}
