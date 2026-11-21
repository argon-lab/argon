package wal_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const suiteCollection = "items"
const suiteKeyCount = 8

// The model indexes documents by position in this table, independently of
// Argon's string encoding. Equal-looking IDs must coexist and stay distinct.
var suiteIDs = [suiteKeyCount]any{
	"q", int32(7), "7", primitive.ObjectID{0x12}, "120000000000000000000000",
	bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}},
	bson.D{{Key: "a", Value: int32(2)}, {Key: "z", Value: int32(1)}}, "~argon:bson:literal",
}

func suiteID(key int) any { return suiteIDs[key] }

func suiteKey(id any) int {
	for k, candidate := range suiteIDs {
		if reflect.DeepEqual(id, candidate) {
			return k
		}
	}
	return -1
}

func suiteDoc(key, value int, reversed bool) bson.D {
	nested := bson.D{{Key: "a", Value: int32(value % 7)}, {Key: "z", Value: "tail"}}
	if reversed {
		nested[0], nested[1] = nested[1], nested[0]
	}
	// PutMany sorts its top-level map. Ordered children keep their field order.
	return bson.D{{Key: "_id", Value: suiteID(key)}, {Key: "list", Value: bson.A{int32(value), nested}},
		{Key: "nested", Value: nested}, {Key: "v", Value: int32(value)}}
}

func suiteDecode(raw bson.Raw) (bson.M, error) {
	if raw == nil {
		return nil, nil
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var id any // preserve order inside a compound _id
	if err := raw.Lookup("_id").Unmarshal(&id); err != nil {
		return nil, err
	}
	doc["_id"] = id
	return doc, nil
}

type suiteState map[int]bson.Raw

type suiteVersion struct {
	lsn       int64
	state     suiteState
	key       int // -1 for a control record
	pre, post bson.Raw
}

type suiteBranch struct {
	engine   *wal.Branch // identity, never the authority on the expected head
	parent   *suiteBranch
	fork     int64
	versions []suiteVersion
}

func (b *suiteBranch) head() int64         { return b.versions[len(b.versions)-1].lsn }
func (b *suiteBranch) current() suiteState { return b.versions[len(b.versions)-1].state }

// The last state at or before an LSN describes the intervening range too.
func (b *suiteBranch) at(lsn int64) suiteState {
	i := sort.Search(len(b.versions), func(i int) bool { return b.versions[i].lsn > lsn })
	if i > 0 {
		return b.versions[i-1].state
	}
	if b.parent != nil {
		return b.parent.at(lsn)
	}
	return suiteState{}
}

type suiteModel struct {
	*historyFixture
	branchesModel []*suiteBranch
	history       suiteHistory
}

func (m *suiteModel) addBranch(engine *wal.Branch, parent *suiteBranch, fork int64, base suiteState) *suiteBranch {
	lsn := fork
	if parent == nil {
		lsn = engine.CreatedLSN
	}
	b := &suiteBranch{engine: engine, parent: parent, fork: fork, versions: []suiteVersion{{lsn: lsn, key: -1, state: base}}}
	m.branchesModel = append(m.branchesModel, b)
	return b
}

func (m *suiteModel) record(b *suiteBranch, lsn int64, key int, post bson.Raw) {
	state := maps.Clone(b.current())
	pre := state[key]
	if key >= 0 {
		if post == nil {
			delete(state, key)
		} else {
			state[key] = post
		}
	}
	b.versions = append(b.versions, suiteVersion{lsn: lsn, key: key, pre: pre, post: post, state: state})
}

func (m *suiteModel) fresh(b *suiteBranch) (*wal.Branch, error) {
	branch, err := m.branches.GetBranchByID(b.engine.ID)
	if err == nil && branch.HeadLSN != b.head() {
		return nil, suiteErrorf("branch head: %s expected %d, got %d", branch.Name, b.head(), branch.HeadLSN)
	}
	return branch, err
}

func suiteCompare(want suiteState, got map[string]bson.M) error {
	if len(got) != len(want) {
		return suiteErrorf("document count: expected %d, got %d (expected %v, got %v)", len(want), len(got), want, got)
	}
	for key := range suiteKeyCount {
		image := want[key]
		if image == nil {
			continue
		}
		expected, err := suiteDecode(image)
		if err != nil {
			return err
		}
		if actual := got[wal.DocumentIDString(suiteID(key))]; !reflect.DeepEqual(actual, expected) {
			return suiteErrorf("document value: key %d expected %v, got %v", key, expected, actual)
		}
	}
	return nil
}

// Choices for actions and samples are materialized before executing the trace.
// Replaying a subsequence needs no random generator or saved database IDs.
type suiteOp struct {
	Kind    string `json:"op"`
	Branch  int    `json:"branch,omitempty"`
	Key     int    `json:"key,omitempty"`
	Value   int    `json:"value,omitempty"`
	Pick    int    `json:"pick,omitempty"`
	Reverse bool   `json:"reverse,omitempty"`
}

func (op suiteOp) String() string {
	return fmt.Sprintf("%s branch=%d key=%d value=%d pick=%d reverse=%t", op.Kind, op.Branch, op.Key, op.Value, op.Pick, op.Reverse)
}

func (m *suiteModel) write(op suiteOp, b *suiteBranch) error {
	branch, err := m.fresh(b)
	if err != nil {
		return err
	}
	w := walwriter.New(m.wal, m.branches, m.mat, branch)
	keys := []int{op.Key}
	var lsns []int64
	var posts []bson.Raw
	if op.Kind == "delete" {
		lsn, existed, err := w.Delete(m.t.Context(), suiteCollection, suiteID(op.Key))
		if err != nil {
			return err
		}
		if want := b.current()[op.Key] != nil; want != existed || (lsn == 0) == existed {
			return suiteErrorf("delete result: expected existed=%t, got existed=%t lsn=%d", want, existed, lsn)
		}
		if !existed {
			return nil
		}
		lsns, posts = []int64{lsn}, []bson.Raw{nil}
	} else {
		if op.Kind == "batch" {
			keys = append(keys, (op.Key+1+op.Pick%(suiteKeyCount-1))%suiteKeyCount, op.Key)
		}
		docs := make([]bson.M, len(keys))
		for i, key := range keys {
			d := suiteDoc(key, op.Value+i, op.Reverse != (i%2 != 0))
			raw, err := bson.Marshal(d)
			if err != nil {
				return err
			}
			posts = append(posts, raw)
			// Convert the top level only: nested values stay bson.D, so the
			// writer sees the field order this step chose.
			docs[i] = make(bson.M, len(d))
			for _, e := range d {
				docs[i][e.Key] = e.Value
			}
		}
		lsns, err = w.PutMany(m.t.Context(), suiteCollection, docs)
		if err != nil {
			return err
		}
	}
	if len(lsns) != len(keys) || lsns[0] <= b.head() {
		return suiteErrorf("write LSNs: head %d, %d writes returned %v", b.head(), len(keys), lsns)
	}
	entries, err := m.wal.GetBranchEntries(b.engine.ID, suiteCollection, lsns[0], lsns[len(lsns)-1])
	if err != nil {
		return err
	}
	if len(entries) != len(keys) {
		return suiteErrorf("write row count: expected %d, got %d", len(keys), len(entries))
	}
	for i, key := range keys {
		e := entries[i]
		want, err := suiteDecode(b.current()[key])
		if err != nil {
			return err
		}
		pre, err := suiteDecode(e.PreImage)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(pre, want) {
			return suiteErrorf("writer pre-image: key %d expected %v, got %v", key, want, pre)
		}
		if e.LSN != lsns[0]+int64(i) || e.LSN != lsns[i] || e.DocumentID != wal.DocumentIDString(suiteID(key)) || !bytes.Equal(posts[i], e.PostImage) {
			return suiteErrorf("writer row: expected key=%d lsn=%d post=%v, got key=%s lsn=%d post=%v", key, lsns[i], posts[i], e.DocumentID, e.LSN, e.PostImage)
		}
		m.record(b, lsns[i], key, posts[i])
	}
	return nil
}

func (m *suiteModel) apply(op suiteOp) error {
	b := m.branchesModel[op.Branch%len(m.branchesModel)]
	switch op.Kind {
	case "put", "batch", "delete":
		return m.write(op, b)
	case "fork", "fork-past", "capture", "diff", "merge":
		return m.branchOp(op, b)
	case "check":
		if err := m.checkRead(b, b.head()); err != nil {
			return suiteErrorf("head: %w", err)
		}
		if err := m.checkRead(b, b.versions[op.Pick%len(b.versions)].lsn); err != nil {
			return suiteErrorf("historical: %w", err)
		}
		return m.branchOp(suiteOp{Kind: "diff", Pick: op.Pick}, b)
	case "snapshot", "gc", "undo", "pin", "legacy", "read":
		return m.historyOp(op, b)
	default:
		return suiteErrorf("unknown operation %q", op.Kind)
	}
}

func suiteTrace(seed int64, steps int) []suiteOp {
	rng := rand.New(rand.NewSource(seed))
	// The opening history makes reclamation, both snapshot paths, all four
	// native operations and an actual merging write reachable even in CI.
	var opening []suiteOp
	if seed%2 == 0 {
		// Before the upgrade every row is untyped. After the upgrade every
		// new writer emits key_v, including on branches with old history.
		for _, key := range rng.Perm(suiteKeyCount) {
			opening = append(opening, suiteOp{Kind: "legacy", Key: key, Value: rng.Intn(30), Reverse: rng.Intn(2) == 0})
		}
		opening = append(opening, suiteOp{Kind: "snapshot"}, suiteOp{Kind: "fork"},
			suiteOp{Kind: "legacy", Branch: 1, Key: rng.Intn(suiteKeyCount), Value: 31, Reverse: true})
	}
	opening = append(opening, []suiteOp{
		{Kind: "put"}, {Kind: "batch", Key: 1, Value: 4}, {Kind: "snapshot"},
		{Kind: "put", Key: 1, Value: 6, Reverse: true}, {Kind: "snapshot"},
		{Kind: "gc"}, {Kind: "pin"}, {Kind: "fork"}, {Kind: "put", Branch: 1, Key: 5, Value: 3},
		{Kind: "undo", Branch: 1}, {Kind: "capture", Branch: 1, Key: 7, Value: 4, Reverse: true},
		{Kind: "capture", Branch: 1, Key: 7, Pick: 1, Value: 6},
		{Kind: "capture", Branch: 1, Key: 7, Pick: 2, Value: 9},
		{Kind: "diff", Branch: 1}, {Kind: "merge", Branch: 1}, {Kind: "capture", Branch: 1, Key: 7},
		{Kind: "fork-past", Branch: 1, Pick: 1},
	}...)
	// Weights keep the more expensive operations in the normal run while
	// producing enough writes between each pair of snapshots for GC.
	weights := []struct {
		kind  string
		count int
	}{
		{"put", 35}, {"batch", 11}, {"delete", 10}, {"undo", 8},
		{"snapshot", 6}, {"gc", 3}, {"read", 10}, {"pin", 1}, {"fork", 2},
		{"fork-past", 2}, {"diff", 5}, {"merge", 6}, {"capture", 1},
	}
	var kinds []string
	for _, w := range weights {
		for range w.count {
			kinds = append(kinds, w.kind)
		}
	}
	ops := make([]suiteOp, 0, steps+steps/8+5)
	for i := range steps {
		var op suiteOp
		if i < len(opening) {
			op = opening[i]
		} else {
			op = suiteOp{Kind: kinds[rng.Intn(len(kinds))], Branch: rng.Intn(60), Key: rng.Intn(suiteKeyCount),
				Value: rng.Intn(30), Pick: rng.Intn(65520), Reverse: rng.Intn(2) == 0}
		}
		ops = append(ops, op)
		if (i+1)%8 == 0 {
			ops = append(ops, suiteOp{Kind: "check", Branch: rng.Intn(60), Pick: rng.Intn(65520)})
		}
	}
	for i := range 5 {
		ops = append(ops, suiteOp{Kind: "check", Branch: i, Pick: rng.Intn(65520)})
	}
	return ops
}

type suiteFailure struct {
	step int
	kind string
	err  error
}

// Remember which assertion failed through wrappers such as "conflict ours"
// or "head". A different assertion in the same operation must not qualify
// a shorter trace, even if its rendered error includes the same values.
type suiteAssertion struct {
	site uintptr
	err  error
}

func (e *suiteAssertion) Error() string { return e.err.Error() }
func (e *suiteAssertion) Unwrap() error { return e.err }

func suiteErrorf(format string, args ...any) error {
	pc, _, _, _ := runtime.Caller(1)
	return &suiteAssertion{site: pc, err: fmt.Errorf(format, args...)}
}

func (f *suiteFailure) signature() string {
	var sig strings.Builder
	sig.WriteString(f.kind)
	err := f.err
	for {
		var assertion *suiteAssertion
		if !errors.As(err, &assertion) {
			if sig.Len() == len(f.kind) {
				// Bare engine errors have no assertion site. Require their exact
				// message rather than reducing to any failure of this operation.
				sig.WriteString(err.Error())
			}
			break
		}
		fmt.Fprintf(&sig, "/%x", assertion.site)
		err = errors.Unwrap(assertion.err)
		if err == nil {
			break
		}
	}
	return sig.String()
}

func suiteMinimize(ops []suiteOp, reproduces func([]suiteOp) bool) []suiteOp {
	for parts := 2; len(ops) > 1; {
		size := (len(ops) + parts - 1) / parts
		reduced := false
		for start := 0; start < len(ops); start += size {
			end := min(start+size, len(ops))
			candidate := append(append([]suiteOp(nil), ops[:start]...), ops[end:]...)
			if len(candidate) > 0 && reproduces(candidate) {
				ops = candidate
				parts = max(2, parts-1)
				reduced = true
				break
			}
		}
		if reduced {
			continue
		}
		if parts >= len(ops) {
			break
		}
		parts = min(parts*2, len(ops))
	}
	return ops
}

func runSuiteTrace(t *testing.T, seed int64, ops []suiteOp, requireReclaim bool) {
	h := newHistoryFixture(t, "trace-0")
	trial := 0
	replay := func(ops []suiteOp) *suiteFailure {
		// Each replay has its own project, with fresh branch IDs, sequence,
		// floors, snapshots and pins. The database/client is per seed.
		if trial > 0 {
			h.project = fmt.Sprintf("trace-%d", trial)
			branch, err := h.branches.CreateBranch(h.project, "main", "")
			if err != nil {
				t.Fatal(err)
			}
			h.main = branch
		}
		trial++
		m := &suiteModel{historyFixture: h}
		m.addBranch(h.main, nil, 0, suiteState{})
		if err := m.initHistory(); err != nil {
			t.Fatal(err)
		}
		for i, op := range ops {
			if err := m.apply(op); err != nil {
				return &suiteFailure{i, op.Kind, err}
			}
		}
		if trial == 1 && requireReclaim && m.history.removed == 0 {
			t.Fatal("the opening GC removed no WAL rows")
		}
		return nil
	}
	failure := replay(ops)
	if failure == nil {
		return
	}
	t.Logf("seed %d operation %d (%s): %v", seed, failure.step+1, ops[failure.step], failure.err)
	short := suiteMinimize(ops[:failure.step+1], func(candidate []suiteOp) bool {
		found := replay(candidate)
		return found != nil && found.signature() == failure.signature()
	})
	reproduced := replay(short)
	if reproduced == nil || reproduced.signature() != failure.signature() {
		t.Fatalf("seed %d: divergence did not recur on final replay: %v\noriginal trace: %v", seed, failure.err, ops[:failure.step+1])
	}
	trace, err := json.Marshal(short[:reproduced.step+1])
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("seed %d minimized sequence (%d operations):\nARGON_MODEL_TRACE='%s'\n%s: %v", seed, reproduced.step+1, trace, reproduced.kind, reproduced.err)
}

func suiteSetting(t *testing.T, name string, fallback int) int {
	t.Helper()
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("%s must be an integer, got %q", name, value)
	}
	return n
}

// Paste a printed ARGON_MODEL_TRACE into the environment to replay a reduced
// example directly, even if the generator's distribution subsequently changes.
func TestCorrectnessModel(t *testing.T) {
	if input, ok := os.LookupEnv("ARGON_MODEL_TRACE"); ok {
		var ops []suiteOp
		if err := json.Unmarshal([]byte(input), &ops); err != nil || len(ops) == 0 {
			t.Fatalf("expected a JSON array of operations, got %q (%v)", input, err)
		}
		for _, op := range ops {
			if op.Branch < 0 || op.Pick < 0 || op.Key < 0 || op.Key >= suiteKeyCount {
				t.Fatalf("invalid selector in %s", op)
			}
		}
		t.Run("replay", func(t *testing.T) { runSuiteTrace(t, 0, ops, false) })
		return
	}
	seeds := suiteSetting(t, "ARGON_MODEL_SEEDS", 3)
	steps := suiteSetting(t, "ARGON_MODEL_STEPS", 200)
	first := suiteSetting(t, "ARGON_MODEL_FIRST_SEED", 1)
	if seeds < 1 || steps < 1 {
		t.Fatalf("model needs positive seed and step counts, got %d seeds and %d steps", seeds, steps)
	}
	for i := range seeds {
		seed := int64(first + i)
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			ops := suiteTrace(seed, steps)
			hasGC := slices.ContainsFunc(ops, func(op suiteOp) bool { return op.Kind == "gc" })
			runSuiteTrace(t, seed, ops, hasGC)
		})
	}
}
