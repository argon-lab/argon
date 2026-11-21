package wal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func (m *suiteModel) branchOp(op suiteOp, b *suiteBranch) error {
	switch op.Kind {
	case "fork", "fork-past":
		if len(m.branchesModel) >= 5 {
			return nil
		}
		name := fmt.Sprintf("fork-%d", len(m.branchesModel))
		lsn := b.head()
		var child *wal.Branch
		var err error
		if op.Kind == "fork-past" {
			// Exclude the head whenever we have an earlier version.
			lsn = b.versions[op.Pick%max(1, len(b.versions)-1)].lsn
			child, err = m.restore.CreateBranchAtLSN(m.project, b.engine.ID, name, lsn)
			if errors.Is(err, wal.ErrHistoryReclaimed) && !m.guaranteed(b, lsn) {
				return nil
			}
		} else {
			child, err = m.branches.CreateBranch(m.project, name, b.engine.ID)
		}
		if err != nil {
			return err
		}
		if child.ParentID != b.engine.ID || child.BaseLSN != lsn || child.HeadLSN != lsn {
			return suiteErrorf("fork %s: expected parent %s, base and head %d; got parent %s, base %d, head %d",
				name, b.engine.Name, lsn, child.ParentID, child.BaseLSN, child.HeadLSN)
		}
		m.addBranch(child, b, lsn, b.at(lsn))
		return nil
	case "capture":
		return m.captureOp(op, b)
	case "diff", "merge":
		if b.parent == nil {
			if len(m.branchesModel) == 1 {
				return nil
			}
			b = m.branchesModel[1+op.Pick%(len(m.branchesModel)-1)]
		}
		return m.mergeOp(op, b)
	default:
		return suiteErrorf("unknown branch operation %q", op.Kind)
	}
}

// A new head fork inherits its parent's head. Its own create_branch row
// already exists at a later LSN, before the first capture or merge into it.
func (b *suiteBranch) lastRecordedLSN() int64 {
	return max(b.head(), b.engine.CreatedLSN)
}

// MongoDB moves _id to the front on insertion. Undo can choose a different
// position for _id in its WAL post-image. Preserve all other encoded bytes.
func suiteMongoImage(raw bson.Raw) (bson.Raw, error) {
	if raw == nil {
		return nil, nil
	}
	fields, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	if len(fields) > 0 && fields[0].Key() == "_id" {
		return raw, nil
	}
	doc := bson.D{{Key: "_id", Value: raw.Lookup("_id")}}
	for _, field := range fields {
		if field.Key() != "_id" {
			doc = append(doc, bson.E{Key: field.Key(), Value: field.Value()})
		}
	}
	return bson.Marshal(doc)
}

// Check every document against the expected bytes before a direct MongoDB write.
func suiteCheckPhysical(ctx context.Context, coll *mongo.Collection, want suiteState) error {
	cursor, err := coll.Find(ctx, bson.D{})
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(ctx) }()
	seen := make(map[int]bool)
	for cursor.Next(ctx) {
		raw := cursor.Current
		var id any
		if err := raw.Lookup("_id").Unmarshal(&id); err != nil {
			return err
		}
		key := suiteKey(id)
		expected, err := suiteMongoImage(want[key])
		if err != nil {
			return err
		}
		if seen[key] || !bytes.Equal(expected, raw) {
			return suiteErrorf("checkout key %d: expected %s once, got %s (duplicate=%t)", key, expected, raw, seen[key])
		}
		seen[key] = true
	}
	if err := cursor.Err(); err != nil {
		return err
	}
	if len(seen) != len(want) {
		return suiteErrorf("checkout expected %d documents, got %d", len(want), len(seen))
	}
	return nil
}

func (m *suiteModel) captureOp(op suiteOp, b *suiteBranch) (result error) {
	ctx, cancel := context.WithTimeout(m.t.Context(), 20*time.Second)
	defer cancel()
	co := checkout.NewService(m.db.Client(), m.db, m.branches, m.mat)
	info, err := co.Checkout(ctx, b.engine.ID)
	if err != nil {
		return err
	}
	// Release the physical database on a comparison failure as well,
	// since the shrinker can replay this checkout many times.
	capture := ingest.NewService(m.db.Client(), m.db, m.wal, m.branches)
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		result = errors.Join(result, capture.Cancel(cleanup, b.engine.ID), co.Release(cleanup, b.engine.ID))
	}()
	physical := m.db.Client().Database(info.PhysicalDB)
	coll := physical.Collection(suiteCollection)
	if err := suiteCheckPhysical(ctx, coll, b.current()); err != nil {
		return err
	}
	if err := checkout.EnablePrePostImages(ctx, physical, suiteCollection); err != nil {
		return err
	}
	if err := capture.Start(ctx, b.engine.ID); err != nil {
		return err
	}

	key := op.Key
	before, err := suiteMongoImage(b.current()[key])
	if err != nil {
		return err
	}
	after, err := suiteNativeWrite(ctx, coll, op, before)
	if err != nil {
		return err
	}
	if err := capture.Drain(ctx, b.engine.ID); err != nil {
		return err
	}
	fresh, err := m.branches.GetBranchByID(b.engine.ID)
	if err != nil {
		return err
	}
	entries, err := m.wal.GetBranchEntries(b.engine.ID, "", b.lastRecordedLSN()+1, fresh.HeadLSN)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		if len(entries) != 0 || fresh.HeadLSN != b.head() {
			return suiteErrorf("no-op native write recorded %d entries, head %d -> %d", len(entries), b.head(), fresh.HeadLSN)
		}
		return nil
	}
	if len(entries) != 1 {
		return suiteErrorf("native write expected one WAL row, got %d", len(entries))
	}
	row := entries[0]
	wantOp := wal.OpPut
	if after == nil {
		wantOp = wal.OpDelete
	}
	if row.Operation != wantOp || row.DocumentID != wal.DocumentIDString(suiteID(key)) ||
		row.Collection != suiteCollection || row.LSN != fresh.HeadLSN ||
		!bytes.Equal(row.PreImage, before) || !bytes.Equal(row.PostImage, after) {
		return suiteErrorf("captured key %d at head %d: expected %s %s -> %s, got %s/%s %s at %d: %s -> %s",
			key, fresh.HeadLSN, wantOp, before, after, row.Collection, row.DocumentID, row.Operation, row.LSN, row.PreImage, row.PostImage)
	}
	m.record(b, row.LSN, key, after)
	return suiteCheckPhysical(ctx, coll, b.current())
}

func suiteNativeWrite(ctx context.Context, coll *mongo.Collection, op suiteOp, before bson.Raw) (bson.Raw, error) {
	id := suiteID(op.Key)
	filter := bson.D{{Key: "_id", Value: id}}
	var doc bson.D
	switch {
	case before == nil:
		doc = suiteNativeDoc(op)
		if _, err := coll.InsertOne(ctx, doc); err != nil {
			return nil, err
		}
	case op.Pick%3 == 0:
		res, err := coll.DeleteOne(ctx, filter)
		if err != nil {
			return nil, err
		}
		if res.DeletedCount != 1 {
			return nil, suiteErrorf("native delete expected one match for %v, got %d", id, res.DeletedCount)
		}
		return nil, nil
	case op.Pick%3 == 1:
		doc = suiteNativeDoc(op)
		res, err := coll.ReplaceOne(ctx, filter, doc)
		if err != nil {
			return nil, err
		}
		if res.MatchedCount != 1 {
			return nil, suiteErrorf("native replace expected one match for %v, got %d", id, res.MatchedCount)
		}
	default:
		// Updating a scalar in place must retain every other field, including
		// nested documents, arrays, and the ordering of the original image.
		if err := bson.Unmarshal(before, &doc); err != nil {
			return nil, err
		}
		for i, field := range doc {
			if field.Key == "v" {
				doc[i].Value = int32(op.Value)
			}
		}
		res, err := coll.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"v": int32(op.Value)}})
		if err != nil {
			return nil, err
		}
		if res.MatchedCount != 1 {
			return nil, suiteErrorf("native update expected one match for %v, got %d", id, res.MatchedCount)
		}
	}
	return bson.Marshal(doc)
}

// Real replacements can change top-level field order as well as the order
// inside a nested document or array. MongoDB keeps _id at the front.
func suiteNativeDoc(op suiteOp) bson.D {
	doc := suiteDoc(op.Key, op.Value, op.Reverse)
	if op.Reverse {
		slices.Reverse(doc[1:])
	}
	return doc
}

type suiteDifference struct {
	key                 int
	conflict            bool
	base, parent, child bson.Raw
}

// Diff compares document values (map order is immaterial), classifying
// three independently recorded states. Absent sides decode as nil.
func suiteDecoded(raw bson.Raw) (bson.M, error) {
	var value bson.M
	if len(raw) == 0 {
		return nil, nil
	}
	err := bson.Unmarshal(raw, &value)
	return value, err
}

func suiteDifferences(b *suiteBranch) ([]suiteDifference, error) {
	base, parent, child := b.versions[0].state, b.parent.current(), b.current()
	var out []suiteDifference
	for key := 0; key < suiteKeyCount; key++ {
		d := suiteDifference{key: key, base: base[key], parent: parent[key], child: child[key]}
		from, err := suiteDecoded(d.base)
		if err != nil {
			return nil, err
		}
		ours, err := suiteDecoded(d.parent)
		if err != nil {
			return nil, err
		}
		theirs, err := suiteDecoded(d.child)
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(from, theirs) || reflect.DeepEqual(ours, theirs) {
			continue
		}
		d.conflict = !reflect.DeepEqual(ours, from)
		out = append(out, d)
	}
	return out, nil
}

// Normalize map/D containers in engine map views. The plan's typed key
// still distinguishes the two ordered compound _ids with identical fields.
func suiteCheckPlanValue(want bson.Raw, got bson.M) error {
	expected, err := suiteDecoded(want)
	if err != nil {
		return err
	}
	var actual bson.M
	if got != nil {
		raw, err := bson.Marshal(got)
		if err != nil {
			return err
		}
		actual, err = suiteDecoded(raw)
		if err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(expected, actual) {
		return suiteErrorf("expected %v, got %v", expected, actual)
	}
	return nil
}

func suiteCheckPlan(b *suiteBranch, plan *merge.Plan, differences []suiteDifference) error {
	if plan.SourceBranchID != b.engine.ID || plan.TargetBranchID != b.parent.engine.ID ||
		plan.SourceHead != b.head() || plan.TargetHead != b.parent.head() || plan.BaseLSN != b.fork {
		return suiteErrorf("expected %s@%d into %s@%d from fork %d, got %s@%d into %s@%d from %d",
			b.engine.Name, b.head(), b.parent.engine.Name, b.parent.head(), b.fork,
			plan.SourceBranch, plan.SourceHead, plan.TargetBranch, plan.TargetHead, plan.BaseLSN)
	}
	pending := make(map[string]suiteDifference, len(differences))
	for _, diff := range differences {
		pending[wal.DocumentIDString(suiteID(diff.key))] = diff
	}
	for _, change := range plan.Changes {
		d, ok := pending[change.DocumentID]
		if !ok || d.conflict || change.Collection != suiteCollection || change.Delete != (d.child == nil) {
			return suiteErrorf("unexpected change %s/%s, delete=%t (expected difference=%t, conflict=%t, child=%s)",
				change.Collection, change.DocumentID, change.Delete, ok, d.conflict, d.child)
		}
		if err := suiteCheckPlanValue(d.parent, change.Before); err != nil {
			return suiteErrorf("change %d before: %w", d.key, err)
		}
		if err := suiteCheckPlanValue(d.child, change.Document); err != nil {
			return suiteErrorf("change %d document: %w", d.key, err)
		}
		delete(pending, change.DocumentID)
	}
	for _, conflict := range plan.Conflicts {
		d, ok := pending[conflict.DocumentID]
		if !ok || !d.conflict || conflict.Collection != suiteCollection {
			return suiteErrorf("unexpected conflict %s/%s", conflict.Collection, conflict.DocumentID)
		}
		if err := suiteCheckPlanValue(d.base, conflict.Base); err != nil {
			return suiteErrorf("conflict %d base: %w", d.key, err)
		}
		if err := suiteCheckPlanValue(d.parent, conflict.Ours); err != nil {
			return suiteErrorf("conflict %d ours: %w", d.key, err)
		}
		if err := suiteCheckPlanValue(d.child, conflict.Theirs); err != nil {
			return suiteErrorf("conflict %d theirs: %w", d.key, err)
		}
		delete(pending, conflict.DocumentID)
	}
	if len(pending) > 0 {
		return suiteErrorf("plan omitted differences %v", pending)
	}
	return nil
}

// For an ordinary merge the saved plan normally keeps the source WAL
// bytes. If an image no longer has WAL evidence, its marked fallback is
// the model's value in field-name order, with its ordered _id preserved.
func suiteMergePost(model bson.Raw, saved bson.Raw, sorted bool) (bson.Raw, error) {
	expected := model
	if sorted && model != nil {
		doc, err := suiteDecode(model)
		if err != nil {
			return nil, err
		}
		expected, err = wal.MarshalDocument(doc)
		if err != nil {
			return nil, err
		}
	}
	if !bytes.Equal(expected, saved) {
		return nil, suiteErrorf("expected saved merge image %s, got %s (sorted=%t)", expected, saved, sorted)
	}
	return expected, nil
}

func (m *suiteModel) mergeOp(op suiteOp, b *suiteBranch) error {
	svc := merge.NewService(m.db, m.wal, m.branches, m.mat, m.db.Client())
	differences, err := suiteDifferences(b)
	if err != nil {
		return err
	}
	// Compute is the read-only diff endpoint. Preview also reconstructs
	// ordered images and persists the plan that Apply will actually load.
	if op.Kind == "diff" {
		plan, err := svc.Compute(b.engine.ID)
		if err != nil {
			return err
		}
		return suiteCheckPlan(b, plan, differences)
	}
	ctx := m.t.Context()
	plan, err := svc.Preview(ctx, b.engine.ID)
	if err != nil {
		return err
	}
	if err := suiteCheckPlan(b, plan, differences); err != nil {
		return err
	}
	strategy := merge.StrategyOurs
	if op.Reverse {
		strategy = merge.StrategyTheirs
	}

	// Resolve each model difference using the selected strategy. Use the
	// plan's evidence marker only to choose between its two documented
	// encodings of the source value; the model supplies the value itself.
	type resolution struct {
		key  int
		post bson.Raw
	}
	resolved := make(map[string]resolution)
	conflictCount := 0
	for _, d := range differences {
		if d.conflict {
			conflictCount++
			if strategy == merge.StrategyOurs {
				continue
			}
		}
		resolved[wal.DocumentIDString(suiteID(d.key))] = resolution{d.key, d.child}
	}
	checkSaved := func(id string, saved bson.Raw, sorted bool) error {
		r := resolved[id]
		post, err := suiteMergePost(r.post, saved, sorted)
		if err != nil {
			return suiteErrorf("key %s: %w", id, err)
		}
		r.post = post
		resolved[id] = r
		return nil
	}
	for _, c := range plan.Changes {
		if err := checkSaved(c.DocumentID, c.DocumentRaw, slices.Contains(c.SortedImages, "document")); err != nil {
			return err
		}
	}
	if strategy == merge.StrategyTheirs {
		for _, c := range plan.Conflicts {
			if err := checkSaved(c.DocumentID, c.TheirsRaw, slices.Contains(c.SortedImages, "theirs")); err != nil {
				return err
			}
		}
	}
	result, err := svc.Apply(ctx, plan.ID, strategy)
	if err != nil {
		return err
	}
	if result.Applied != len(resolved) || result.ConflictsResolved != conflictCount {
		return suiteErrorf("merge expected %d writes, %d conflicts resolved, got %+v", len(resolved), conflictCount, result)
	}
	target := b.parent
	fresh, err := m.branches.GetBranchByID(target.engine.ID)
	if err != nil {
		return err
	}
	rows, err := m.wal.GetBranchEntries(target.engine.ID, "", target.lastRecordedLSN()+1, fresh.HeadLSN)
	if err != nil {
		return err
	}
	if len(rows) != len(resolved)+1 {
		return suiteErrorf("merge expected %d data rows and one control row, got %d rows", len(resolved), len(rows))
	}
	for _, row := range rows[:len(rows)-1] {
		r, ok := resolved[row.DocumentID]
		wantOp := wal.OpPut
		if r.post == nil {
			wantOp = wal.OpDelete
		}
		if !ok || row.Collection != suiteCollection || row.Operation != wantOp || !bytes.Equal(row.PostImage, r.post) {
			return suiteErrorf("merge WAL expected key present=%t, %s %s; got %s/%s %s %s",
				ok, wantOp, r.post, row.Collection, row.DocumentID, row.Operation, row.PostImage)
		}
		before, err := suiteDecoded(row.PreImage)
		if err != nil {
			return err
		}
		if err := suiteCheckPlanValue(target.current()[r.key], before); err != nil {
			return suiteErrorf("merge pre-image key %d: %w", r.key, err)
		}
		m.record(target, row.LSN, r.key, r.post)
		delete(resolved, row.DocumentID)
	}
	control := rows[len(rows)-1]
	if control.Operation != wal.OpMerge || control.LSN != fresh.HeadLSN {
		return suiteErrorf("merge expected final merge record at %d, got %s at %d", fresh.HeadLSN, control.Operation, control.LSN)
	}
	m.record(target, control.LSN, -1, nil)
	return nil
}
