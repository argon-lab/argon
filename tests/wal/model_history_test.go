package wal_test

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/pin"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// All generated rows belong to suiteCollection. The model keeps the positions
// of published snapshots and the extent of actual WAL reclamation separately
// from the original state at each LSN.
type suiteHistory struct {
	gc         *gc.Service
	pins       *pin.Service
	undos      *undo.Service
	snapshots  map[*suiteBranch][]int64
	pinned     map[*suiteBranch]int64
	floors     map[*suiteBranch]int64
	oldThrough int64
	removed    int64
}

func (m *suiteModel) initHistory() error {
	pins, err := pin.NewService(m.db, m.branches)
	if err != nil {
		return err
	}
	pins.SetReadableCheck(m.mat.ValidateReadable)
	collector := gc.NewService(m.wal, m.branches, m.snapshots)
	collector.SetPinLookup(pins.LSNsForBranch)
	m.history = suiteHistory{
		gc: collector, pins: pins, undos: undo.NewService(m.wal, m.branches, m.db.Client(), m.mat),
		snapshots: make(map[*suiteBranch][]int64), pinned: make(map[*suiteBranch]int64),
		floors: make(map[*suiteBranch]int64),
	}
	return nil
}

func (m *suiteModel) historyOp(op suiteOp, b *suiteBranch) error {
	switch op.Kind {
	case "snapshot":
		return m.snapshotHead(b)
	case "gc":
		return m.collectHistory(op.Pick)
	case "pin":
		if _, exists := m.history.pinned[b]; exists {
			return nil
		}
		p, err := m.history.pins.Create(m.project, b.engine.ID, "pin-"+b.engine.Name, b.head(), "")
		if err != nil {
			return err
		}
		if p.LSN != b.head() {
			return suiteErrorf("pin LSN: want %d, got %d", b.head(), p.LSN)
		}
		m.history.pinned[b] = p.LSN
	case "undo":
		return m.undoOne(b, op.Pick)
	case "legacy":
		return m.legacyPut(b, op)
	case "read":
		at := b.versions[op.Pick%len(b.versions)].lsn
		if err := m.checkRead(b, at); err != nil {
			return err
		}
		branch, err := m.fresh(b)
		if err != nil {
			return err
		}
		actual, err := m.mat.MaterializeDocumentAtLSN(branch, suiteCollection, wal.DocumentIDString(suiteID(op.Key)), at)
		// A surviving full image can answer a point lookup even if other
		// documents at this LSN were reclaimed. Any value returned must match.
		if errors.Is(err, wal.ErrHistoryReclaimed) && !m.guaranteed(b, at) {
			return nil
		}
		if err != nil {
			return err
		}
		want, err := suiteDecode(b.at(at)[op.Key])
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, want) {
			return suiteErrorf("point read %s key %d at %d: want %v, got %v", branch.Name, op.Key, at, want, actual)
		}
	default:
		return suiteErrorf("unknown history operation %q", op.Kind)
	}
	return nil
}

// In this workload there are no resets or source-copy baselines. An exact
// snapshot answers its own position, even when newer snapshots allowed GC
// to remove the interval immediately after it. Between snapshots, all replay
// slots from the last baseline to the target must remain available.
func (m *suiteModel) guaranteed(b *suiteBranch, at int64) bool {
	for b != nil {
		if at > b.fork {
			base := b.fork
			for _, lsn := range m.history.snapshots[b] {
				if lsn <= at {
					base = max(base, lsn)
				}
			}
			if base < at && base < m.history.floors[b] {
				return false
			}
			if base > b.fork {
				return true // This frame includes the entire inherited state.
			}
		}
		at = min(at, b.fork)
		b = b.parent
	}
	return true
}

func (m *suiteModel) checkRead(b *suiteBranch, at int64) error {
	branch, err := m.fresh(b)
	if err != nil {
		return err
	}
	got, err := m.timeTravel.MaterializeAtLSN(branch, suiteCollection, at)
	if !m.guaranteed(b, at) {
		if !errors.Is(err, wal.ErrHistoryReclaimed) {
			return suiteErrorf("reclaimed read %s at %d: want ErrHistoryReclaimed, got %v, state %v", branch.Name, at, err, got)
		}
		err = m.mat.ValidateReadable(branch, at)
		if !errors.Is(err, wal.ErrHistoryReclaimed) {
			return suiteErrorf("advertised readability %s at %d: want ErrHistoryReclaimed, got %v", branch.Name, at, err)
		}
		return nil
	}
	if err != nil {
		return suiteErrorf("readable history %s at %d: %w", branch.Name, at, err)
	}
	if err := suiteCompare(b.at(at), got); err != nil {
		return suiteErrorf("history %s at %d: %w", branch.Name, at, err)
	}
	return nil
}

func (m *suiteModel) snapshotHead(b *suiteBranch) error {
	at := b.head()
	if at <= b.fork {
		return nil // Snapshot creation is defined only on a branch's own segment.
	}
	branch, err := m.fresh(b)
	if err != nil {
		return err
	}
	manifests, err := m.snapshots.CreateSnapshot(m.t.Context(), branch.ID, at)
	if err != nil {
		return err
	}
	want := b.current()
	if len(manifests) == 0 {
		// Collections with a retained delete or a prior empty snapshot still
		// require a manifest, even when their current state is empty.
		for owner, limit := b, at; owner != nil; owner = owner.parent {
			for _, v := range owner.versions {
				if v.key >= 0 && v.lsn <= limit {
					return suiteErrorf("missing snapshot of %s at %d", branch.Name, at)
				}
			}
			limit = min(limit, owner.fork)
		}
		return nil
	}
	if len(manifests) != 1 || manifests[0].Collection != suiteCollection || manifests[0].LSN != at || manifests[0].DocCount != int64(len(want)) {
		return suiteErrorf("snapshot manifest %s at %d: want one %s manifest with %d documents, got %+v", branch.Name, at, suiteCollection, len(want), manifests)
	}
	lsn, frames, err := m.snapshots.OrderedSnapshotFrames(branch, suiteCollection, at, at, at)
	if err != nil {
		return err
	}
	if lsn != at || frames == nil {
		return suiteErrorf("missing ordered snapshot at %d, got %d", at, lsn)
	}
	seen := make(map[int]bool)
	err = frames(func(name string, raw bson.Raw) error {
		id, err := wal.DocumentIDFromImage(raw)
		if err != nil {
			return err
		}
		key := suiteKey(id)
		if seen[key] || want[key] == nil || name != wal.DocumentIDString(id) || !bytes.Equal(want[key], raw) {
			return suiteErrorf("snapshot frame %s at %d, key %q: want %x, got %x (duplicate %t)", branch.Name, at, name, []byte(want[key]), []byte(raw), seen[key])
		}
		seen[key] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(want) {
		return suiteErrorf("snapshot frames %s at %d: want %d, got %d", branch.Name, at, len(want), len(seen))
	}
	if !slices.Contains(m.history.snapshots[b], at) {
		m.history.snapshots[b] = append(m.history.snapshots[b], at)
	}
	return nil
}

// Set timestamps on the selected prefix only. The large separation from
// current timestamps makes the one-hour retention window deterministic by
// seed, without sleeps or a test clock in the engine.
func (m *suiteModel) collectHistory(pick int) error {
	var latest int64
	for _, b := range m.branchesModel {
		latest = max(latest, b.head())
	}
	bound := max(m.history.oldThrough, latest-int64(pick%6))
	if bound > m.history.oldThrough {
		_, err := m.db.Collection("wal_log").UpdateMany(m.t.Context(), bson.M{
			"project_id": m.project, "lsn": bson.M{"$gt": m.history.oldThrough, "$lte": bound},
		}, bson.M{"$set": bson.M{"timestamp": time.Unix(1, 0).UTC()}})
		if err != nil {
			return err
		}
		m.history.oldThrough = bound
	}
	report, err := m.history.gc.RunProject(m.t.Context(), m.project, gc.Config{RetentionWindow: time.Hour})
	if err != nil {
		return err
	}
	var removed int64
	for _, b := range m.branchesModel {
		oldFloor := m.history.floors[b]
		floor, err := m.wal.ReclaimFloor(b.engine.ID, suiteCollection)
		if err != nil {
			return err
		}
		if floor < oldFloor || floor > bound {
			return suiteErrorf("GC floor %s: want [%d,%d], got %d", b.engine.Name, oldFloor, bound, floor)
		}
		var firstRemoved int64
		protected := []int64{b.head(), b.fork}
		for _, v := range b.versions {
			if v.key < 0 {
				continue
			}
			if v.lsn > oldFloor && v.lsn <= floor {
				removed++
				if firstRemoved == 0 {
					firstRemoved = v.lsn
				}
			}
			if v.lsn > bound {
				protected = append(protected, v.lsn)
			}
		}
		m.history.floors[b] = floor
		if floor > oldFloor {
			remaining, err := m.db.Collection("wal_log").CountDocuments(m.t.Context(), bson.M{
				"branch_id": b.engine.ID, "collection": suiteCollection, "lsn": bson.M{"$lte": floor},
			})
			if err != nil {
				return err
			}
			if remaining != 0 {
				return suiteErrorf("GC retained %d rows at or below floor %d on %s", remaining, floor, b.engine.Name)
			}
		}
		if lsn, ok := m.history.pinned[b]; ok {
			protected = append(protected, lsn)
		}
		for _, child := range m.branchesModel {
			if child.parent == b {
				protected = append(protected, child.fork)
			}
		}
		slices.Sort(protected)
		for _, at := range slices.Compact(protected) {
			if !m.guaranteed(b, at) {
				return suiteErrorf("GC removed history needed by %s at protected LSN %d", b.engine.Name, at)
			}
			if err := m.checkRead(b, at); err != nil {
				return err
			}
		}
		if firstRemoved != 0 {
			if err := m.checkRead(b, firstRemoved); err != nil {
				return err
			}
		}
	}
	if removed != report.EntriesRemoved {
		return suiteErrorf("GC deletion count: model counted %d, report says %d", removed, report.EntriesRemoved)
	}
	m.history.removed += removed
	return nil
}

func rewriteSuiteLegacyKey(h *historyFixture, branchID string, lsn int64, envelope string) error {
	changed, err := h.db.Collection("wal_log").UpdateOne(h.t.Context(), bson.M{"branch_id": branchID, "lsn": lsn},
		bson.M{"$unset": bson.M{"key_v": ""}, "$set": bson.M{"document_id": envelope}})
	if err == nil && changed.ModifiedCount != 1 {
		return suiteErrorf("expected one legacy WAL row at %d, changed %d", lsn, changed.ModifiedCount)
	}
	return err
}

func (m *suiteModel) legacyPut(b *suiteBranch, op suiteOp) error {
	doc := suiteDoc(op.Key, op.Value, op.Reverse)
	post, err := bson.Marshal(doc)
	if err != nil {
		return err
	}
	id := suiteID(op.Key)
	lsn, err := m.wal.Append(&wal.Entry{
		ProjectID: m.project, BranchID: b.engine.ID, Actor: "importer", Collection: suiteCollection,
		Operation: wal.OpPut, DocumentID: wal.LegacyDocumentIDString(id),
		PreImage: b.current()[op.Key], PostImage: post,
	})
	if err != nil {
		return err
	}
	// Modern append normalizes the envelope. Recreate the persisted v2.0.1
	// representation exactly as TestSource_WriterUndoLegacyDocumentKey does.
	if err := rewriteSuiteLegacyKey(m.historyFixture, b.engine.ID, lsn, wal.LegacyDocumentIDString(id)); err != nil {
		return err
	}
	if err := m.branches.UpdateBranchHead(b.engine.ID, lsn); err != nil {
		return err
	}
	m.record(b, lsn, op.Key, post)
	return nil
}

func (m *suiteModel) undoOne(b *suiteBranch, pick int) error {
	// Pick any original write or compensation, including old writes that
	// conflict with a later edit and writes whose WAL range was reclaimed.
	var writes []suiteVersion
	for _, v := range b.versions {
		if v.key >= 0 {
			writes = append(writes, v)
		}
	}
	if len(writes) == 0 {
		return nil
	}
	index := pick % len(writes)
	v := writes[index]
	branch, err := m.fresh(b)
	if err != nil {
		return err
	}
	plan, err := m.history.undos.BuildPlan(branch, v.lsn, v.lsn, "")
	if v.lsn <= m.history.floors[b] {
		if !errors.Is(err, wal.ErrHistoryReclaimed) {
			return suiteErrorf("undo reclaimed LSN %d: want ErrHistoryReclaimed, got %v", v.lsn, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	key := wal.DocumentIDString(suiteID(v.key))
	for _, later := range writes[index+1:] {
		if later.key != v.key {
			continue
		}
		if len(plan.Conflicts) != 1 || plan.Conflicts[0].AtLSN != later.lsn || plan.Conflicts[0].DocumentID != key ||
			len(plan.Compensations) != 0 || len(plan.Unrecoverable) != 0 {
			return suiteErrorf("undo conflict at %d: want only %s changed at %d, got %+v", v.lsn, key, later.lsn, plan)
		}
		restored, deleted, err := m.history.undos.Apply(m.t.Context(), branch, plan)
		if err != nil {
			return err
		}
		if restored != 0 || deleted != 0 {
			return suiteErrorf("undo conflict at %d changed %d/%d documents", v.lsn, restored, deleted)
		}
		return m.checkRead(b, b.head())
	}
	if len(plan.Compensations) != 1 || len(plan.Unrecoverable) != 0 || len(plan.Conflicts) != 0 {
		return suiteErrorf("undo plan at %d: want one compensation, got %+v", v.lsn, plan)
	}
	want, err := suiteDecode(v.pre)
	if err != nil {
		return err
	}
	before, err := suiteDecode(b.current()[v.key])
	if err != nil {
		return err
	}
	c := plan.Compensations[0]
	if c.Collection != suiteCollection || c.DocumentID != key || !reflect.DeepEqual(want, c.Restore) || !reflect.DeepEqual(before, c.Before) {
		return suiteErrorf("undo compensation at %d: want %s/%s %v -> %v, got %+v", v.lsn, suiteCollection, key, before, want, c)
	}
	restored, deleted, err := m.history.undos.Apply(m.t.Context(), branch, plan)
	if err != nil {
		return err
	}
	wantRestored := 0
	if want != nil {
		wantRestored = 1
	}
	if restored != wantRestored || deleted != 1-wantRestored {
		return suiteErrorf("undo counts at %d: want restored/deleted %d/%d, got %d/%d", v.lsn, wantRestored, 1-wantRestored, restored, deleted)
	}
	newBranch, err := m.branches.GetBranchByID(branch.ID)
	if err != nil {
		return err
	}
	entries, err := m.wal.GetBranchEntries(branch.ID, suiteCollection, b.head()+1, newBranch.HeadLSN)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].DocumentID != key || entries[0].LSN != newBranch.HeadLSN {
		return suiteErrorf("undo at %d: expected one compensating row for %s, got %d", v.lsn, key, len(entries))
	}
	entry := entries[0]
	if (want == nil && entry.Operation != wal.OpDelete) || (want != nil && entry.Operation != wal.OpPut) {
		return suiteErrorf("undo operation at %d: restored value %v, got operation %s", v.lsn, want, entry.Operation)
	}
	actualBefore, err := suiteDecode(entry.PreImage)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, actualBefore) {
		return suiteErrorf("undo pre-image at %d: want %v, got %v", v.lsn, before, actualBefore)
	}
	// Undo promises equivalent values. Once verified, its chosen BSON order
	// is the exact order that subsequent snapshots and checkouts must retain.
	actual, err := suiteDecode(entry.PostImage)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(want, actual) {
		return suiteErrorf("undo post-image at %d: want %v, got %v", v.lsn, want, actual)
	}
	m.record(b, entry.LSN, v.key, entry.PostImage)
	return m.checkRead(b, b.head())
}
