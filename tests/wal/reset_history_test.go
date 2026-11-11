package wal_test

// Reset, snapshot and GC interactions: what a reset may and may not change
// for views captured before it, and what reclamation must leave readable.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/pin"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// historyFixture is a branch with a writer and a few helpers for laying
// down runs of documents and reading them back as sorted ID lists.
type historyFixture struct {
	*snapshotFixture
	t       *testing.T
	db      *mongo.Database
	project string
	main    *wal.Branch
	writer  *walwriter.Writer
}

func newHistoryFixture(t *testing.T, project string) *historyFixture {
	t.Helper()
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	return &historyFixture{
		snapshotFixture: f, t: t, db: db, project: project, main: main,
		writer: walwriter.New(f.wal, f.branches, f.mat, main),
	}
}

// put writes n documents named <prefix>0..<prefix>n-1 and returns the head.
func (h *historyFixture) put(prefix string, n int) int64 {
	h.t.Helper()
	for i := 0; i < n; i++ {
		_, err := h.writer.Put(context.Background(), "docs", bson.M{"_id": fmt.Sprintf("%s%d", prefix, i)})
		require.NoError(h.t, err)
	}
	return h.reload().HeadLSN
}

func (h *historyFixture) reload() *wal.Branch {
	h.t.Helper()
	branch, err := h.branches.GetBranchByID(h.main.ID)
	require.NoError(h.t, err)
	h.main = branch
	return branch
}

func (h *historyFixture) reset(target int64) {
	h.t.Helper()
	_, err := h.restore.ResetBranchToLSN(h.main.ID, target)
	require.NoError(h.t, err)
	h.reload()
}

func idsOf(docs map[string]bson.M) []string {
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ids reads a branch's "docs" collection through full replay (no snapshots).
func (h *historyFixture) ids(branch *wal.Branch) []string {
	h.t.Helper()
	fresh, err := h.branches.GetBranchByID(branch.ID)
	require.NoError(h.t, err)
	docs, err := h.matFull.MaterializeCollection(fresh, "docs")
	require.NoError(h.t, err)
	return idsOf(docs)
}

// idsAt reads the main branch at an LSN through both paths and requires
// them to agree before returning the result.
func (h *historyFixture) idsAt(lsn int64) []string {
	h.t.Helper()
	branch := h.reload()
	full, err := h.matFull.MaterializeCollectionAtLSN(branch, "docs", lsn)
	require.NoError(h.t, err)
	accelerated, err := h.mat.MaterializeCollectionAtLSN(branch, "docs", lsn)
	require.NoError(h.t, err)
	require.Equal(h.t, idsOf(full), idsOf(accelerated), "snapshot path and full replay disagree at LSN %d", lsn)
	return idsOf(full)
}

func run(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return ids
}

func concat(runs ...[]string) []string {
	var all []string
	for _, r := range runs {
		all = append(all, r...)
	}
	sort.Strings(all)
	return all
}

// Two resets in a row ("that was not far enough back") must not change what
// a backup branch or a pin taken before the first reset reads. Releases up
// to 2.1.5 ended the second discarded range at the already rewound head, so
// those views lost everything between the two reset targets.
func TestReset_ConsecutiveResetsLeaveEarlierViewsIntact(t *testing.T) {
	h := newHistoryFixture(t, "reset-stacked")
	pins, err := pin.NewService(h.db, h.branches)
	require.NoError(t, err)

	afterK := h.put("k", 3)
	afterM := h.put("m", 3)
	afterX := h.put("x", 3)
	everything := concat(run("k", 3), run("m", 3), run("x", 3))

	// What `argon restore reset --backup` creates, and a pin on the same state.
	backup, err := h.restore.CreateBranchAtLSN(h.project, h.main.ID, "pre-incident", afterX)
	require.NoError(t, err)
	p, err := pins.Create(h.project, h.main.ID, "eval-v1", 0, "")
	require.NoError(t, err)

	h.reset(afterM)
	// A branch forked between the two resets captures the state in between.
	between, err := h.branches.CreateBranch(h.project, "between", h.main.ID)
	require.NoError(t, err)
	require.Equal(t, everything, h.ids(backup))

	h.reset(afterK) // no write in between: the head is already below the first window
	assert.Equal(t, everything, h.ids(backup), "the backup still reads the state it was forked from")
	fromPin, err := h.restore.CreateBranchFromPin(h.project, p.BranchID, "pinned-run", p.LSN)
	require.NoError(t, err)
	assert.Equal(t, everything, h.ids(fromPin), "the pin still reads the state it pinned")
	assert.Equal(t, concat(run("k", 3), run("m", 3)), h.ids(between), "a fork between the resets keeps its own view")
	assert.Equal(t, run("k", 3), h.ids(h.main))

	// Both windows end at the highest LSN the branch had reached.
	assert.Equal(t, []wal.LSNRange{{From: afterM + 1, To: afterX}, {From: afterK + 1, To: afterX}}, h.reload().DiscardedRanges)

	// New history lands above both windows and none of them resurfaces.
	h.put("n", 2)
	assert.Equal(t, concat(run("k", 3), run("n", 2)), h.ids(h.main))
	assert.Equal(t, everything, h.ids(backup))
	assert.Equal(t, everything, h.ids(fromPin))
}

// Branch records written by releases up to 2.1.5 keep the too-short range.
// They are read with the corrected end, so views that an old release was
// showing incompletely recover on upgrade without any migration.
func TestReset_RangesFromOlderReleasesAreReadWithTheirTrueEnd(t *testing.T) {
	h := newHistoryFixture(t, "reset-legacy")
	afterK := h.put("k", 3)
	afterM := h.put("m", 3)
	afterX := h.put("x", 3)
	backup, err := h.restore.CreateBranchAtLSN(h.project, h.main.ID, "pre-incident", afterX)
	require.NoError(t, err)

	// The metadata 2.1.5 leaves behind after reset(afterM), reset(afterK).
	_, err = h.db.Collection("wal_branches").UpdateOne(context.Background(), bson.M{"_id": h.main.ID}, bson.M{"$set": bson.M{
		"head_lsn": afterK,
		"discarded_ranges": []wal.LSNRange{
			{From: afterM + 1, To: afterX},
			{From: afterK + 1, To: afterM},
		},
	}})
	require.NoError(t, err)

	main := h.reload()
	assert.Equal(t, []wal.LSNRange{{From: afterM + 1, To: afterX}, {From: afterK + 1, To: afterX}}, main.EffectiveDiscardedRanges())
	assert.Equal(t, afterX, main.HistoryHighWater())
	assert.Equal(t, concat(run("k", 3), run("m", 3), run("x", 3)), h.ids(backup))
	assert.Equal(t, run("k", 3), h.ids(main))

	h.put("n", 1)
	assert.Equal(t, concat(run("k", 3), run("n", 1)), h.ids(h.main))
}

// A reset cannot land inside history an earlier reset discarded: that
// position is not on the branch's timeline any more. The history itself is
// still there, and forking it is how it is recovered.
func TestReset_IntoDiscardedHistoryIsRefusedButForkable(t *testing.T) {
	h := newHistoryFixture(t, "reset-into-discarded")
	afterD := h.put("d", 10)
	afterE := h.put("e", 10)
	h.reset(afterD)

	// Straight after the reset the discarded stretch is above the head. A
	// reset there is refused as discarded history, not as "the future".
	_, err := h.restore.ResetBranchToLSN(h.main.ID, afterD+5)
	require.ErrorContains(t, err, "earlier reset discarded")
	require.ErrorContains(t, h.restore.ValidateRestore(h.main.ID, afterD+5), "earlier reset discarded")
	_, err = h.restore.GetRestorePreview(h.main.ID, afterD+5)
	require.ErrorContains(t, err, "earlier reset discarded")
	_, err = h.restore.ResetBranchToLSN(h.main.ID, afterE+1)
	require.ErrorContains(t, err, "beyond branch HEAD")

	// It can be read and forked without waiting for a later write.
	assert.Equal(t, concat(run("d", 10), run("e", 5)), h.idsAt(afterD+5))
	recovered, err := h.restore.CreateBranchAtLSN(h.project, h.main.ID, "recovered-early", afterD+5)
	require.NoError(t, err)
	assert.Equal(t, concat(run("d", 10), run("e", 5)), h.ids(recovered))
	_, err = h.restore.CreateBranchAtLSN(h.project, h.main.ID, "beyond-history", afterE+1)
	require.Error(t, err, "nothing was ever written above the high-water mark")
	_, err = h.timeTravel.MaterializeAtLSN(h.main, "docs", afterE+1)
	require.Error(t, err)

	h.put("post", 1)
	state := concat(run("d", 10), run("post", 1))
	require.Equal(t, state, h.ids(h.main))

	inside := afterD + 5
	_, err = h.restore.ResetBranchToLSN(h.main.ID, inside)
	require.ErrorContains(t, err, "earlier reset discarded")
	require.ErrorContains(t, err, fmt.Sprintf("LSN %d or earlier", afterD), "the error names the nearest valid target")
	_, err = h.restore.GetRestorePreview(h.main.ID, inside)
	require.ErrorContains(t, err, "earlier reset discarded", "a preview fails the way the reset would")
	require.ErrorContains(t, h.restore.ValidateRestore(h.main.ID, inside), "earlier reset discarded")
	assert.Equal(t, state, h.ids(h.main), "a refused reset changes nothing")
	assert.Len(t, h.reload().DiscardedRanges, 1)

	// The same position remains forkable after the head has moved on.
	later, err := h.restore.CreateBranchAtLSN(h.project, h.main.ID, "recovered-late", inside)
	require.NoError(t, err)
	assert.Equal(t, concat(run("d", 10), run("e", 5)), h.ids(later))

	// Resetting to the edge of the window is an ordinary reset.
	h.reset(afterD)
	assert.Equal(t, run("d", 10), h.ids(h.main))

	// A preview counts only what the reset would newly discard.
	h.put("q", 2)
	preview, err := h.restore.GetRestorePreview(h.main.ID, afterD)
	require.NoError(t, err)
	assert.Equal(t, 2, preview.OperationsToDiscard)
}

// A timestamp resolves to where the branch stood at that moment, before and
// after the first post-reset write alike. Forking there works; resetting
// there is refused when a reset has since discarded it.
func TestReset_TimeResolvesToHistoryAsItWas(t *testing.T) {
	h := newHistoryFixture(t, "reset-time")
	good := h.put("good", 3)
	time.Sleep(20 * time.Millisecond)
	bad := h.put("bad", 3)
	time.Sleep(20 * time.Millisecond)
	duringIncident := time.Now()
	time.Sleep(20 * time.Millisecond)

	h.reset(good)
	lsn, err := h.timeTravel.FindLSNAtTime(h.main, duringIncident)
	require.NoError(t, err)
	assert.Equal(t, bad, lsn)

	h.put("fixed", 1)
	lsn, err = h.timeTravel.FindLSNAtTime(h.main, duringIncident)
	require.NoError(t, err)
	assert.Equal(t, bad, lsn, "the answer does not depend on whether a write followed the reset")

	_, err = h.restore.ResetBranchToTime(h.main.ID, duringIncident)
	require.ErrorContains(t, err, "earlier reset discarded")
	inspect, err := h.restore.CreateBranchAtTime(h.project, h.main.ID, "what-happened", duringIncident)
	require.NoError(t, err)
	assert.Equal(t, concat(run("good", 3), run("bad", 3)), h.ids(inspect))
	assert.Equal(t, concat(run("good", 3), run("fixed", 1)), h.ids(h.main))
}

// A snapshot taken at an LSN inside a discarded window holds the pre-reset
// history. It may serve readers that keep that view, and nobody else.
func TestSnapshot_InsideDiscardedWindowServesOnlyPreResetReaders(t *testing.T) {
	h := newHistoryFixture(t, "snap-in-window")
	ctx := context.Background()
	afterD := h.put("d", 10)
	h.put("e", 10)
	h.reset(afterD)
	h.put("post", 1)

	inside := afterD + 5
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, inside)
	require.NoError(t, err)

	// Readers on the current timeline never see the discarded writes.
	assert.Equal(t, concat(run("d", 10), run("post", 1)), h.idsAt(h.reload().HeadLSN))
	h.put("later", 1)
	assert.Equal(t, concat(run("d", 10), run("post", 1), run("later", 1)), h.idsAt(h.reload().HeadLSN))

	// Readers of the pre-reset view do, and may start from that snapshot.
	assert.Equal(t, concat(run("d", 10), run("e", 5)), h.idsAt(inside))
	assert.Equal(t, concat(run("d", 10), run("e", 7)), h.idsAt(inside+2))
	branch := h.reload()
	_, snapLSN, ok, err := h.snapshots.FindUsable(branch, "docs", 1, inside+2, inside+2)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, inside, snapLSN)
	_, _, ok, err = h.snapshots.FindUsable(branch, "docs", 1, branch.HeadLSN, branch.HeadLSN)
	require.NoError(t, err)
	assert.False(t, ok, "the head must not start from a snapshot of discarded history")

	// GC must not treat it as coverage for the branch's future readers.
	report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved)
}

// Databases written by 2.1.5 can hold exactly that situation with the head
// rewound into the window: a snapshot at the rewound head that the old rule
// then replayed on top of for later writes, resurrecting discarded
// documents. The same manifest is now refused for those readers.
func TestSnapshot_ManifestFromOlderReleaseCannotResurrectDiscardedWrites(t *testing.T) {
	h := newHistoryFixture(t, "snap-legacy")
	ctx := context.Background()
	afterD := h.put("d", 10)
	afterE := h.put("e", 10)
	h.reset(afterD)
	afterPost := h.put("post", 1)

	// 2.1.5 accepted a reset to an LSN inside the first window.
	inside := afterD + 5
	_, err := h.db.Collection("wal_branches").UpdateOne(ctx, bson.M{"_id": h.main.ID}, bson.M{"$set": bson.M{
		"head_lsn": inside,
		"discarded_ranges": []wal.LSNRange{
			{From: afterD + 1, To: afterE},
			{From: inside + 1, To: afterPost},
		},
	}})
	require.NoError(t, err)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, inside)
	require.NoError(t, err)
	_, err = h.db.Collection("wal_snapshots").UpdateMany(ctx, bson.M{"branch_id": h.main.ID}, bson.M{"$unset": bson.M{"visibility_rules": ""}})
	require.NoError(t, err)

	h.reload()
	h.put("next", 1)
	assert.Equal(t, concat(run("d", 10), run("next", 1)), h.idsAt(h.reload().HeadLSN),
		"both read paths agree and the discarded documents stay discarded")
	// What the user briefly saw at the rewound head is still recoverable.
	assert.Equal(t, concat(run("d", 10), run("e", 5)), h.idsAt(inside))
}

// A manifest written by an older release between the recorded and the true
// end of a back-to-back reset window left out history its readers should
// see. No reader may start from it.
func TestSnapshot_ManifestBuiltUnderTooShortRangeIsUnusable(t *testing.T) {
	h := newHistoryFixture(t, "snap-legacy-stacked")
	ctx := context.Background()
	afterK := h.put("k", 3)
	afterM := h.put("m", 3)
	afterX := h.put("x", 3)

	_, err := h.db.Collection("wal_branches").UpdateOne(ctx, bson.M{"_id": h.main.ID}, bson.M{"$set": bson.M{
		"head_lsn": afterX + 1, // let a snapshot be requested above both ranges' starts
		"discarded_ranges": []wal.LSNRange{
			{From: afterM + 1, To: afterX},
			{From: afterK + 1, To: afterM},
		},
	}})
	require.NoError(t, err)

	// Stand in for the old build: at afterM+1 it skipped (afterK, afterM].
	at := afterM + 1
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	_, err = h.db.Collection("wal_snapshots").UpdateMany(ctx, bson.M{"branch_id": h.main.ID}, bson.M{"$unset": bson.M{"visibility_rules": ""}})
	require.NoError(t, err)

	branch := h.reload()
	for _, bound := range []int64{at, afterX, afterX + 1} {
		_, _, ok, err := h.snapshots.FindUsable(branch, "docs", 1, at, bound)
		require.NoError(t, err)
		assert.False(t, ok, "bound %d", bound)
	}
	// A current build at the same position is fine for pre-reset readers.
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
	require.NoError(t, err)
	_, _, ok, err := h.snapshots.FindUsable(branch, "docs", 1, at, afterX)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, concat(run("k", 3), run("m", 3), run("x", 1)), h.idsAt(at))
}

// GC with a retention window must leave every in-window LSN readable. A
// read just above the window's start replays from the newest snapshot below
// it, so the entries between that snapshot and the window are still needed
// even though they are older than the window.
func TestGC_InWindowReadsSurviveReclamation(t *testing.T) {
	h := newHistoryFixture(t, "gc-window-reads")
	ctx := context.Background()
	gcService := gc.NewService(h.wal, h.branches, h.snapshots)

	afterA := h.put("a", 10)
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, afterA)
	require.NoError(t, err)
	afterB := h.put("b", 10)
	time.Sleep(200 * time.Millisecond)
	windowStart := time.Now()
	time.Sleep(200 * time.Millisecond)
	afterC := h.put("c", 10)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, afterC)
	require.NoError(t, err)

	inWindow := afterB + 5
	want := concat(run("a", 10), run("b", 10), run("c", 5))
	require.Equal(t, want, h.idsAt(inWindow))

	report, err := gcService.RunProject(ctx, h.project, gc.Config{RetentionWindow: time.Since(windowStart)})
	require.NoError(t, err)
	assert.EqualValues(t, 10, report.EntriesRemoved, "only what the older snapshot covers is reclaimed")
	assert.Equal(t, map[string]int64{"docs": afterA}, report.Branches[0].Cutoffs)

	branch := h.reload()
	for lsn := afterB + 1; lsn <= afterC; lsn++ {
		docs, err := h.mat.MaterializeCollectionAtLSN(branch, "docs", lsn)
		require.NoError(t, err)
		assert.Len(t, docs, int(20+lsn-afterB), "in-window read at LSN %d", lsn)
	}
	docs, err := h.mat.MaterializeCollectionAtLSN(branch, "docs", inWindow)
	require.NoError(t, err)
	assert.Equal(t, want, idsOf(docs))

	// Once the window has moved past the newer snapshot, the rest goes too.
	report, err = gcService.RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	assert.EqualValues(t, 20, report.EntriesRemoved)
	docs, err = h.mat.MaterializeCollection(h.reload(), "docs")
	require.NoError(t, err)
	assert.Len(t, docs, 30)
}

// A snapshot above the retention boundary is not grounds for reclaiming
// anything when there is none at or below it.
func TestGC_NothingReclaimedWithoutSnapshotBelowTheWindow(t *testing.T) {
	h := newHistoryFixture(t, "gc-no-floor")
	ctx := context.Background()
	h.put("a", 10)
	time.Sleep(200 * time.Millisecond)
	windowStart := time.Now()
	time.Sleep(200 * time.Millisecond)
	afterB := h.put("b", 10)
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, afterB)
	require.NoError(t, err)

	report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, gc.Config{RetentionWindow: time.Since(windowStart)})
	require.NoError(t, err)
	assert.Zero(t, report.EntriesRemoved)
	assert.Equal(t, concat(run("a", 10), run("b", 3)), h.idsAt(afterB-7))
}

// Garbage collection may leave an old position without the entries needed
// to rebuild it. Reading, resetting to, forking, pinning or undoing through
// such a position must fail loudly; it used to replay across the hole and
// return a state that silently lacked the reclaimed writes.
func TestGC_ReclaimedPositionsAreRefusedNotMisread(t *testing.T) {
	h := newHistoryFixture(t, "gc-floors")
	ctx := context.Background()
	pins, err := pin.NewService(h.db, h.branches)
	require.NoError(t, err)
	pins.SetReadableCheck(h.mat.ValidateReadable)
	undoService := undo.NewService(h.wal, h.branches, h.db.Client(), h.mat)

	afterA := h.put("a", 5)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, afterA)
	require.NoError(t, err)
	afterB := h.put("b", 5)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, afterB)
	require.NoError(t, err)
	between := afterA + 2 // rebuilt from the first snapshot plus two entries

	report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	require.EqualValues(t, 10, report.EntriesRemoved)
	floor, err := h.wal.ReclaimFloor(h.main.ID, "docs")
	require.NoError(t, err)
	assert.Equal(t, afterB, floor)

	// The head, and positions that sit exactly on a snapshot, are intact.
	branch := h.reload()
	docs, err := h.mat.MaterializeCollection(branch, "docs")
	require.NoError(t, err)
	assert.Equal(t, concat(run("a", 5), run("b", 5)), idsOf(docs))
	docs, err = h.mat.MaterializeCollectionAtLSN(branch, "docs", afterA)
	require.NoError(t, err)
	assert.Equal(t, run("a", 5), idsOf(docs))
	require.NoError(t, h.mat.ValidateReadable(branch, afterA))
	require.NoError(t, h.mat.ValidateReadable(branch, branch.HeadLSN))

	// A position between the snapshots is gone, and says so everywhere.
	_, err = h.mat.MaterializeCollectionAtLSN(branch, "docs", between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	_, err = h.timeTravel.GetBranchStateAtLSN(branch, between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed, "a fully reclaimed collection must not simply vanish from the result")
	_, err = h.mat.MaterializeDocumentAtLSN(branch, "docs", "a0", between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	require.ErrorIs(t, h.mat.ValidateReadable(branch, between), wal.ErrHistoryReclaimed)

	_, err = h.restore.GetRestorePreview(h.main.ID, between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	require.ErrorIs(t, h.restore.ValidateRestore(h.main.ID, between), wal.ErrHistoryReclaimed)
	_, err = h.restore.ResetBranchToLSN(h.main.ID, between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	_, err = h.restore.CreateBranchAtLSN(h.project, h.main.ID, "unreadable", between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	_, err = pins.Create(h.project, h.main.ID, "unreadable", between, "")
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	_, err = undoService.BuildPlan(branch, afterA+1, 0, "")
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)
	_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, between)
	require.ErrorIs(t, err, wal.ErrHistoryReclaimed)

	after := h.reload()
	assert.Equal(t, branch.HeadLSN, after.HeadLSN, "refusals change nothing")
	assert.Empty(t, after.DiscardedRanges)
	_, err = h.branches.GetBranch(h.project, "unreadable")
	require.Error(t, err)

	// Positions that can still be rebuilt keep working: fork one, pin one,
	// reset to one, undo above the floor.
	fork, err := h.restore.CreateBranchAtLSN(h.project, h.main.ID, "at-snapshot", afterA)
	require.NoError(t, err)
	assert.Equal(t, run("a", 5), idsOfBranch(t, h, fork))
	_, err = pins.Create(h.project, h.main.ID, "at-snapshot", afterA, "")
	require.NoError(t, err)
	h.put("c", 2)
	plan, err := undoService.BuildPlan(h.reload(), afterB+1, 0, "")
	require.NoError(t, err)
	assert.Len(t, plan.Compensations, 2)
	h.reset(afterA)
	docs, err = h.mat.MaterializeCollection(h.reload(), "docs")
	require.NoError(t, err)
	assert.Equal(t, run("a", 5), idsOf(docs))

	// New history on top of that reset reads through the reclaimed stretch
	// without complaint: everything reclaimed above the reset is discarded.
	h.put("d", 1)
	docs, err = h.mat.MaterializeCollection(h.reload(), "docs")
	require.NoError(t, err)
	assert.Equal(t, concat(run("a", 5), run("d", 1)), idsOf(docs))
}

// Reclaimed entries that a reader would have skipped as discarded are not a
// hole for that reader.
func TestGC_ReclaimedDiscardedHistoryDoesNotBlockReaders(t *testing.T) {
	h := newHistoryFixture(t, "gc-floors-discarded")
	ctx := context.Background()
	afterA := h.put("a", 3)
	_, err := h.snapshots.CreateSnapshot(ctx, h.main.ID, afterA)
	require.NoError(t, err)
	report, err := gc.NewService(h.wal, h.branches, h.snapshots).RunProject(ctx, h.project, zeroRetention)
	require.NoError(t, err)
	require.EqualValues(t, 3, report.EntriesRemoved)

	// Rewind to before everything. The only snapshot now holds discarded
	// history and the entries beneath it are gone — which no longer matters.
	h.reset(h.main.BaseLSN)
	docs, err := h.mat.MaterializeCollection(h.reload(), "docs")
	require.NoError(t, err)
	assert.Empty(t, docs)
	h.put("fresh", 2)
	docs, err = h.mat.MaterializeCollection(h.reload(), "docs")
	require.NoError(t, err)
	assert.Equal(t, run("fresh", 2), idsOf(docs))

	// The discarded stretch itself is still readable from its snapshot.
	docs, err = h.mat.MaterializeCollectionAtLSN(h.reload(), "docs", afterA)
	require.NoError(t, err)
	assert.Equal(t, run("a", 3), idsOf(docs))
}

func idsOfBranch(t *testing.T, h *historyFixture, branch *wal.Branch) []string {
	t.Helper()
	fresh, err := h.branches.GetBranchByID(branch.ID)
	require.NoError(t, err)
	docs, err := h.mat.MaterializeCollection(fresh, "docs")
	require.NoError(t, err)
	return idsOf(docs)
}
