package wal_test

// A model-based test of branch history: random sequences of writes, deletes,
// resets, forks (from the head and from history), pins, snapshots, garbage
// collection and branch deletion run against the engine and against a model
// that knows nothing about LSN ranges, snapshots or replay.
//
// The model is plain bookkeeping. Every write records the full state it
// produced; a reset copies an earlier recorded state back; a fork or a pin
// copies the state it captures and keeps the copy. Whatever the engine does
// internally, it must read back exactly those copies:
//
//   - a branch head reads the branch's current state;
//   - a fork keeps the state it was forked from, plus its own writes;
//   - a pin keeps the state it pinned;
//   - a historical read at an LSN returns the state that write produced,
//     including after a reset has discarded it;
//   - the snapshot-accelerated path and full replay agree;
//   - garbage collection changes none of the above for heads, forks, pins
//     and every LSN inside its retention window;
//   - an older position that garbage collection has reclaimed past is
//     either still read exactly or refused with wal.ErrHistoryReclaimed —
//     for reads, resets and forks alike. It is never answered with a state
//     that silently lacks the reclaimed writes.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/pin"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

var modelCollections = []string{"a", "b"}

// modelState maps "collection/id" to the document's version.
type modelState map[string]int32

func (s modelState) clone() modelState {
	c := make(modelState, len(s))
	for k, v := range s {
		c[k] = v
	}
	return c
}

func (s modelState) collection(name string) modelState {
	out := make(modelState)
	for k, v := range s {
		if strings.HasPrefix(k, name+"/") {
			out[strings.TrimPrefix(k, name+"/")] = v
		}
	}
	return out
}

type modelBranch struct {
	engine  *wal.Branch // identity only: ID and name
	parent  *modelBranch
	writer  *walwriter.Writer
	baseLSN int64
	base    modelState // the state the branch was created from
	head    int64
	current modelState
	// states[lsn] is the state right after the branch's own write at lsn.
	// It is never removed: a reset only changes which LSNs are on the
	// timeline, not what was true when they were written.
	states   map[int64]modelState
	timeline []int64       // own LSNs the head can still be reset to, ascending
	written  map[int64]int // own LSN -> sequence number of the op that wrote it
	children int
	pins     int
	deleted  bool
}

func (b *modelBranch) stateAt(lsn int64) modelState {
	if lsn == b.baseLSN {
		return b.base
	}
	return b.states[lsn]
}

type modelPin struct {
	owner *modelBranch
	lsn   int64
	state modelState
}

// modelMark is an instant between two operations, usable as the start of a
// GC retention window: everything written by operations after seq is inside
// the window, everything before is outside.
type modelMark struct {
	seq int
	at  time.Time
}

type historyModel struct {
	t       *testing.T
	f       *snapshotFixture
	pinsSvc *pin.Service
	gc      *gc.Service
	rng     *rand.Rand
	project string
	log     []string

	branches []*modelBranch
	pins     []*modelPin
	marks    []modelMark
	seq      int // operations performed so far
	nextVal  int32
	names    int

	// After a GC run, historical reads are guaranteed only for writes made
	// after the run's retention boundary. retainedAfter is the newest such
	// boundary so far, as an operation sequence number; -1 before any GC.
	retainedAfter int
	// Full replay stops being a valid oracle once entries are reclaimed.
	reclaimed bool
}

func (m *historyModel) logf(format string, args ...interface{}) {
	m.log = append(m.log, fmt.Sprintf("%3d  ", m.seq)+fmt.Sprintf(format, args...))
}

func (m *historyModel) fail(format string, args ...interface{}) {
	m.t.Helper()
	m.t.Fatalf("%s\n\noperations:\n%s", fmt.Sprintf(format, args...), strings.Join(m.log, "\n"))
}

func (m *historyModel) must(err error, what string) {
	m.t.Helper()
	if err != nil {
		m.fail("%s: %v", what, err)
	}
}

func (m *historyModel) live() []*modelBranch {
	var out []*modelBranch
	for _, b := range m.branches {
		if !b.deleted {
			out = append(out, b)
		}
	}
	return out
}

func (m *historyModel) pick() *modelBranch {
	live := m.live()
	return live[m.rng.Intn(len(live))]
}

func (m *historyModel) addBranch(engine *wal.Branch, parent *modelBranch, base modelState) *modelBranch {
	b := &modelBranch{
		engine: engine, parent: parent,
		writer:  walwriter.New(m.f.wal, m.f.branches, m.f.mat, engine),
		baseLSN: engine.BaseLSN, base: base.clone(),
		head: engine.HeadLSN, current: base.clone(),
		states: make(map[int64]modelState), written: make(map[int64]int),
	}
	if parent != nil {
		parent.children++
	}
	m.branches = append(m.branches, b)
	return b
}

func (m *historyModel) name(prefix string) string {
	m.names++
	return fmt.Sprintf("%s-%d", prefix, m.names)
}

func (m *historyModel) recordWrite(b *modelBranch, lsn int64) {
	if lsn <= b.head {
		m.fail("write on %s got LSN %d, not above head %d", b.engine.Name, lsn, b.head)
	}
	b.head = lsn
	b.states[lsn] = b.current.clone()
	b.timeline = append(b.timeline, lsn)
	b.written[lsn] = m.seq
}

// guaranteed reports whether GC must keep a position of a branch readable:
// its head and fork point, anything written inside the newest retention
// window, and any position a pin or a live fork was captured at.
func (m *historyModel) guaranteed(b *modelBranch, lsn int64) bool {
	if lsn == b.head || lsn == b.baseLSN || b.written[lsn] > m.retainedAfter {
		return true
	}
	for _, p := range m.pins {
		if p.owner == b && p.lsn == lsn {
			return true
		}
	}
	for _, child := range m.branches {
		if child.parent == b && !child.deleted && child.baseLSN == lsn {
			return true
		}
	}
	return false
}

// refused reports whether err is the engine declining a position because
// garbage collection reclaimed past it — acceptable only for a position
// nothing guarantees. Any other error, and a refusal of a guaranteed
// position, fails the test.
func (m *historyModel) refused(err error, b *modelBranch, lsn int64, what string) bool {
	m.t.Helper()
	if err == nil {
		return false
	}
	if !errors.Is(err, wal.ErrHistoryReclaimed) {
		m.fail("%s: %v", what, err)
	}
	if m.guaranteed(b, lsn) {
		m.fail("%s: LSN %d of %s must stay readable, but was refused: %v", what, lsn, b.engine.Name, err)
	}
	m.logf("       ... refused: reclaimed")
	return true
}

// --- operations ---

func (m *historyModel) write() {
	b := m.pick()
	coll := modelCollections[m.rng.Intn(len(modelCollections))]
	id := fmt.Sprintf("d%d", m.rng.Intn(5))
	m.nextVal++
	m.logf("put    %-10s %s/%s = %d", b.engine.Name, coll, id, m.nextVal)
	lsn, err := b.writer.Put(context.Background(), coll, bson.M{"_id": id, "v": m.nextVal})
	m.must(err, "put")
	b.current[coll+"/"+id] = m.nextVal
	m.recordWrite(b, lsn)
}

func (m *historyModel) remove() {
	b := m.pick()
	coll := modelCollections[m.rng.Intn(len(modelCollections))]
	id := fmt.Sprintf("d%d", m.rng.Intn(5))
	m.logf("delete %-10s %s/%s", b.engine.Name, coll, id)
	lsn, existed, err := b.writer.Delete(context.Background(), coll, id)
	m.must(err, "delete")
	_, known := b.current[coll+"/"+id]
	if existed != known {
		m.fail("delete %s/%s on %s: engine says existed=%v, model says %v", coll, id, b.engine.Name, existed, known)
	}
	if existed {
		delete(b.current, coll+"/"+id)
		m.recordWrite(b, lsn)
	}
}

func (m *historyModel) reset() {
	b := m.pick()
	targets := append([]int64{b.baseLSN}, b.timeline...)
	target := targets[m.rng.Intn(len(targets))]
	m.logf("reset  %-10s to %d (head %d)", b.engine.Name, target, b.head)
	_, err := m.f.restore.ResetBranchToLSN(b.engine.ID, target)
	if m.refused(err, b, target, "reset") {
		return
	}
	b.current = b.stateAt(target).clone()
	b.head = target
	keep := b.timeline[:0]
	for _, lsn := range b.timeline {
		if lsn <= target {
			keep = append(keep, lsn)
		}
	}
	b.timeline = keep
}

// resetIntoDiscarded asks for a reset to an LSN an earlier reset took off
// the timeline. The engine must refuse and leave the branch alone.
func (m *historyModel) resetIntoDiscarded() {
	b := m.pick()
	onTimeline := make(map[int64]bool, len(b.timeline))
	for _, lsn := range b.timeline {
		onTimeline[lsn] = true
	}
	var discarded []int64
	for lsn := range b.states {
		if !onTimeline[lsn] {
			discarded = append(discarded, lsn)
		}
	}
	if len(discarded) == 0 {
		return
	}
	sort.Slice(discarded, func(i, j int) bool { return discarded[i] < discarded[j] })
	target := discarded[m.rng.Intn(len(discarded))]
	m.logf("reset  %-10s to discarded %d must be refused (head %d)", b.engine.Name, target, b.head)
	if _, err := m.f.restore.ResetBranchToLSN(b.engine.ID, target); err == nil {
		m.fail("reset of %s to discarded LSN %d was accepted", b.engine.Name, target)
	}
}

func (m *historyModel) fork() {
	parent := m.pick()
	name := m.name("fork")
	m.logf("fork   %-10s from %s at head %d", name, parent.engine.Name, parent.head)
	child, err := m.f.branches.CreateBranch(m.project, name, parent.engine.ID)
	m.must(err, "fork")
	if child.BaseLSN != parent.head {
		m.fail("fork %s based at %d, parent head is %d", name, child.BaseLSN, parent.head)
	}
	m.addBranch(child, parent, parent.current)
}

// forkHistory forks any position the parent's history ever held, whether or
// not a reset has since discarded it.
func (m *historyModel) forkHistory() {
	parent := m.pick()
	positions := []int64{parent.baseLSN}
	for lsn := range parent.states {
		positions = append(positions, lsn)
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
	at := positions[m.rng.Intn(len(positions))]
	name := m.name("past")
	m.logf("fork   %-10s from %s at LSN %d (head %d)", name, parent.engine.Name, at, parent.head)
	child, err := m.f.restore.CreateBranchAtLSN(m.project, parent.engine.ID, name, at)
	if m.refused(err, parent, at, "fork from history") {
		return
	}
	m.addBranch(child, parent, parent.stateAt(at))
}

func (m *historyModel) pin() {
	b := m.pick()
	name := m.name("pin")
	m.logf("pin    %-10s on %s at head %d", name, b.engine.Name, b.head)
	p, err := m.pinsSvc.Create(m.project, b.engine.ID, name, 0, "")
	m.must(err, "pin")
	if p.LSN != b.head {
		m.fail("pin %s at %d, head is %d", name, p.LSN, b.head)
	}
	b.pins++
	m.pins = append(m.pins, &modelPin{owner: b, lsn: p.LSN, state: b.current.clone()})
}

func (m *historyModel) snapshot() {
	b := m.pick()
	at := b.head
	// Half the time, snapshot a position from the branch's past instead —
	// possibly one that a reset has discarded.
	if m.rng.Intn(2) == 0 {
		var past []int64
		for lsn := range b.states {
			if lsn <= b.head {
				past = append(past, lsn)
			}
		}
		if len(past) > 0 {
			sort.Slice(past, func(i, j int) bool { return past[i] < past[j] })
			at = past[m.rng.Intn(len(past))]
		}
	}
	if at <= b.baseLSN {
		return
	}
	m.logf("snap   %-10s at %d (head %d)", b.engine.Name, at, b.head)
	_, err := m.f.snapshots.CreateSnapshot(context.Background(), b.engine.ID, at)
	m.refused(err, b, at, "snapshot")
}

// mark records an instant with a clear margin of quiet on both sides. WAL
// timestamps have millisecond resolution and GC reads the clock a moment
// after it is called; the margin keeps "before the mark" and "after the
// mark" unambiguous for both.
func (m *historyModel) mark() {
	const margin = 60 * time.Millisecond
	time.Sleep(margin)
	m.marks = append(m.marks, modelMark{seq: m.seq, at: time.Now()})
	m.logf("mark")
	time.Sleep(margin)
}

func (m *historyModel) collect(windowed bool) {
	boundary := m.seq // zero retention: everything written so far is out of the window
	window := time.Duration(0)
	if windowed && len(m.marks) > 0 {
		chosen := m.marks[m.rng.Intn(len(m.marks))]
		boundary, window = chosen.seq, time.Since(chosen.at)
	}
	m.logf("gc     window starts after op %d", boundary)
	report, err := m.gc.RunProject(context.Background(), m.project, gc.Config{RetentionWindow: window})
	m.must(err, "gc")
	if report.EntriesRemoved > 0 {
		m.reclaimed = true
	}
	if boundary > m.retainedAfter {
		m.retainedAfter = boundary
	}
	// Nothing written inside the window may be at or below a cutoff.
	byID := make(map[string]*modelBranch)
	for _, b := range m.branches {
		byID[b.engine.ID] = b
	}
	for _, br := range report.Branches {
		b := byID[br.BranchID]
		for coll, cutoff := range br.Cutoffs {
			for lsn, seq := range b.written {
				if seq > boundary && lsn <= cutoff {
					m.fail("gc reclaimed %s/%s up to LSN %d, but LSN %d was written inside the retention window", b.engine.Name, coll, cutoff, lsn)
				}
			}
		}
	}
}

func (m *historyModel) deleteLeaf() {
	var leaves []*modelBranch
	for _, b := range m.live() {
		if b.parent != nil && b.children == 0 && b.pins == 0 {
			leaves = append(leaves, b)
		}
	}
	if len(leaves) == 0 {
		return
	}
	b := leaves[m.rng.Intn(len(leaves))]
	m.logf("drop   %-10s", b.engine.Name)
	m.must(m.f.branches.DeleteBranch(m.project, b.engine.Name), "delete branch")
	b.deleted = true
	b.parent.children--
	m.reclaimed = true
}

// --- checks ---

func flatten(state map[string]map[string]bson.M) modelState {
	out := make(modelState)
	for coll, docs := range state {
		for id, doc := range docs {
			out[coll+"/"+id] = doc["v"].(int32)
		}
	}
	return out
}

func flattenCollection(docs map[string]bson.M) modelState {
	out := make(modelState)
	for id, doc := range docs {
		out[id] = doc["v"].(int32)
	}
	return out
}

func (m *historyModel) fresh(b *modelBranch) *wal.Branch {
	branch, err := m.f.branches.GetBranchByID(b.engine.ID)
	m.must(err, "reload "+b.engine.Name)
	if branch.HeadLSN != b.head {
		m.fail("%s head is %d, model expects %d", b.engine.Name, branch.HeadLSN, b.head)
	}
	return branch
}

func (m *historyModel) checkHead(b *modelBranch) {
	branch := m.fresh(b)
	got, err := m.f.mat.MaterializeBranch(branch)
	m.must(err, "materialize "+b.engine.Name)
	if !assertEqualState(b.current, flatten(got)) {
		m.fail("%s head (LSN %d): want %v, got %v", b.engine.Name, b.head, b.current, flatten(got))
	}
	if !m.reclaimed {
		full, err := m.f.matFull.MaterializeBranch(branch)
		m.must(err, "full replay "+b.engine.Name)
		if !assertEqualState(b.current, flatten(full)) {
			m.fail("%s head (LSN %d) by full replay: want %v, got %v", b.engine.Name, b.head, b.current, flatten(full))
		}
	}
}

// checkAt reads a branch at an LSN through the snapshot-accelerated path,
// and through full replay as well when alsoFullReplay is set and no entries
// have been reclaimed yet.
func (m *historyModel) checkAt(b *modelBranch, lsn int64, want modelState, what string, alsoFullReplay bool) {
	branch := m.fresh(b)
	for _, coll := range modelCollections {
		docs, err := m.f.mat.MaterializeCollectionAtLSN(branch, coll, lsn)
		if errors.Is(err, wal.ErrHistoryReclaimed) && !m.guaranteed(b, lsn) {
			continue // Refused, not answered: acceptable for a reclaimed position.
		}
		m.must(err, what)
		if !assertEqualState(want.collection(coll), flattenCollection(docs)) {
			m.fail("%s: %s/%s at LSN %d: want %v, got %v", what, b.engine.Name, coll, lsn, want.collection(coll), flattenCollection(docs))
		}
		if alsoFullReplay && !m.reclaimed {
			full, err := m.f.matFull.MaterializeCollectionAtLSN(branch, coll, lsn)
			m.must(err, what)
			if !assertEqualState(want.collection(coll), flattenCollection(full)) {
				m.fail("%s by full replay: %s/%s at LSN %d: want %v, got %v", what, b.engine.Name, coll, lsn, want.collection(coll), flattenCollection(full))
			}
		}
	}
}

func assertEqualState(want, got modelState) bool {
	if len(want) != len(got) {
		return false
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return false
		}
	}
	return true
}

// check verifies every head and every pin, and historical positions:
// guaranteed ones must read back exactly, the rest exactly or not at all.
// Reading every position after every step is quadratic, so a step that
// cannot have changed what history reads as (a write, a fork, a pin) only
// samples a few positions per branch; resets, snapshots, collections and
// deletions, and the end of the run, check them all.
func (m *historyModel) check(everyPosition bool) {
	for _, b := range m.live() {
		m.checkHead(b)
		positions := make([]int64, 0, len(b.states))
		for lsn := range b.states {
			positions = append(positions, lsn)
		}
		sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
		if !everyPosition && len(positions) > 2 {
			m.rng.Shuffle(len(positions), func(i, j int) { positions[i], positions[j] = positions[j], positions[i] })
			positions = positions[:2]
		}
		for _, lsn := range positions {
			m.checkAt(b, lsn, b.states[lsn], "historical read", everyPosition)
		}
	}
	for _, p := range m.pins {
		m.checkAt(p.owner, p.lsn, p.state, "pinned read", true)
	}
}

// step performs one random operation and reports whether it is of a kind
// that can change how existing history reads.
func (m *historyModel) step() (rereadHistory bool) {
	m.seq++
	switch roll := m.rng.Intn(100); {
	case roll < 36:
		m.write()
	case roll < 44:
		m.remove()
	case roll < 57:
		m.reset()
		return true
	case roll < 61:
		m.resetIntoDiscarded()
	case roll < 67:
		if len(m.live()) < 5 {
			m.fork()
		}
	case roll < 73:
		if len(m.live()) < 5 {
			m.forkHistory()
		}
	case roll < 78:
		if len(m.pins) < 4 {
			m.pin()
		}
	case roll < 88:
		m.snapshot()
		return true
	case roll < 91:
		if len(m.marks) < 3 {
			m.mark()
		}
	case roll < 93:
		m.collect(false)
		return true
	case roll < 98:
		m.collect(true)
		return true
	default:
		m.deleteLeaf()
		return true
	}
	return false
}

func runHistoryModel(t *testing.T, seed int64, steps int) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	pins, err := pin.NewService(db, f.branches)
	require.NoError(t, err)
	gcService := gc.NewService(f.wal, f.branches, f.snapshots)
	gcService.SetPinLookup(pins.LSNsForBranch)
	f.branches.SetDeleteGuard(pins.RequireNoPins)
	f.branches.SetDeleteHook(func(branchID string) {
		if _, _, _, err := gcService.ReclaimDeletedBranch(context.Background(), branchID); err != nil {
			t.Errorf("reclaim deleted branch: %v", err)
		}
	})

	m := &historyModel{
		t: t, f: f, pinsSvc: pins, gc: gcService,
		rng: rand.New(rand.NewSource(seed)), project: fmt.Sprintf("model-%d", seed),
		retainedAfter: -1,
	}
	main, err := f.branches.CreateBranch(m.project, "main", "")
	require.NoError(t, err)
	m.addBranch(main, nil, modelState{})

	for i := 0; i < steps; i++ {
		// Checking after every step keeps a failure next to the operation
		// that caused it.
		m.check(m.step() || i == steps-1)
	}
}

// TestModel_BranchHistoryMatchesBookkeeping runs a fixed set of seeds on
// every test run. To search further, or to replay a failure (its name
// carries the seed):
//
//	ARGON_MODEL_SEEDS=500 ARGON_MODEL_FIRST_SEED=1000 ARGON_MODEL_STEPS=80 \
//	  go test ./tests/wal -run TestModel_ -count=1
func TestModel_BranchHistoryMatchesBookkeeping(t *testing.T) {
	setting := func(name string, fallback int64) int64 {
		v := os.Getenv(name)
		if v == "" {
			return fallback
		}
		n, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err, name)
		return n
	}
	seeds := setting("ARGON_MODEL_SEEDS", 16)
	first := setting("ARGON_MODEL_FIRST_SEED", 1)
	steps := int(setting("ARGON_MODEL_STEPS", 40))
	for seed := first; seed < first+seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			// Each seed works in its own database, so they can run together.
			t.Parallel()
			runHistoryModel(t, seed, steps)
		})
	}
}
