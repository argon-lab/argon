package wal_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// The tests in this file hold the changed-documents path, and the point read
// it is built on (MaterializeDocumentsAtLSN), to the collection read on
// history that continues after garbage collection: writes, resets, forks
// from history, snapshots on any branch at any LSN, and further collection
// runs, including one that is under way while plans are computed.
// ARGON_MERGE_GC_SEEDS sets how many random histories are built.

// reproOutcome says how the changed-documents path answered.
type reproOutcome int

const (
	reproAgreed reproOutcome = iota
	reproDeclined
)

// reproComparePlans is requirePlansAgree for histories where the
// changed-documents path may legitimately decline: when it returns a plan,
// the plan must be the reference's; when it declines, Compute must behave
// exactly like the reference.
func reproComparePlans(t *testing.T, svc *merge.Service, sourceID, label string) reproOutcome {
	t.Helper()
	full, fullErr := svc.ComputeFull(sourceID)
	changed, changedErr := svc.ComputeChanged(sourceID)
	if changedErr != nil {
		plan, err := svc.Compute(sourceID)
		if fullErr != nil {
			require.Error(t, err, "%s: compute must refuse where the reference refuses (%v)", label, fullErr)
			return reproDeclined
		}
		require.NoError(t, err, "%s: compute after the changed-documents path declined (%v)", label, changedErr)
		requireSamePlan(t, full, plan, label+": compute (fallback) vs full")
		return reproDeclined
	}
	require.NoError(t, fullErr, "%s: the changed-documents path returned a plan where the reference refuses", label)
	requireSamePlan(t, full, changed, label+": changed vs full")
	plan, err := svc.Compute(sourceID)
	require.NoError(t, err, "%s: compute", label)
	requireSamePlan(t, full, plan, label+": compute vs full")
	return reproAgreed
}

func reproSeeds(def int) int {
	if v, err := strconv.Atoi(os.Getenv("ARGON_MERGE_GC_SEEDS")); err == nil && v > 0 {
		return v
	}
	return def
}

// reproKeyUniverse lists, per collection, every key applyMixedWorkload can
// write, plus one nobody writes.
func reproKeyUniverse() map[string][]string {
	var plain []string
	for i := 0; i < 25; i++ {
		plain = append(plain, fmt.Sprintf("doc-%d", i))
	}
	var batch []string
	for i := 0; i < 12; i++ {
		batch = append(batch, fmt.Sprintf("batch-%d", i))
	}
	plain = append(plain, batch...)
	plain = append(plain, "never-written")
	typed := append([]string{"never-written"}, batch...)
	for _, id := range typedIDs() {
		typed = append(typed, wal.DocumentIDString(id))
	}
	return map[string][]string{"users": plain, "orders": plain, "typed": typed}
}

// (b) and (c). The commit's random test stops at the first garbage
// collection. This one keeps going: after a GC the branches are written,
// reset, forked from history and from each other, snapshotted anywhere
// (the source included) and collected again, and after every step every
// source's plan is compared with the reference and the point read is
// compared with the collection read at random bounds on every branch.
func TestMergeChanged_RandomHistoriesContinuedAfterGC(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	universe := reproKeyUniverse()

	var agreedTotal, declinedTotal, pointCompared, pointDeclined, pointOnly, singleCompared, singleRefused int
	for seed := int64(1); seed <= int64(reproSeeds(12)); seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			f := newSnapshotFixture(t, db)
			svc := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
			gcService := gc.NewService(f.wal, f.branches, f.snapshots)
			project := fmt.Sprintf("repro-continued-%d", seed)

			reload := func(b *wal.Branch) *wal.Branch {
				fresh, err := f.branches.GetBranchByID(b.ID)
				require.NoError(t, err)
				return fresh
			}
			writer := func(b *wal.Branch) *walwriter.Writer {
				return walwriter.New(f.wal, f.branches, f.mat, reload(b))
			}
			// tolerate lets through the refusals GC entitles the system to.
			tolerate := func(err error, what string) bool {
				if err == nil {
					return true
				}
				require.True(t, errors.Is(err, wal.ErrHistoryReclaimed), "%s: %v", what, err)
				return false
			}
			snapshotAt := func(b *wal.Branch, head bool) {
				b = reload(b)
				if b.HeadLSN <= b.BaseLSN {
					return
				}
				lsn := b.HeadLSN
				if !head {
					lsn = b.BaseLSN + 1 + rng.Int63n(b.HeadLSN-b.BaseLSN)
				}
				_, err := f.snapshots.CreateSnapshot(ctx, b.ID, lsn)
				tolerate(err, "snapshot")
			}
			resetSomewhere := func(b *wal.Branch) {
				b = reload(b)
				if b.HeadLSN-b.BaseLSN < 3 {
					return
				}
				target := b.BaseLSN + 1 + rng.Int63n(b.HeadLSN-b.BaseLSN-1)
				if b.ValidateResetTarget(target) != nil {
					return
				}
				_, err := f.restore.ResetBranchToLSN(b.ID, target)
				tolerate(err, "reset")
			}

			var all, sources []*wal.Branch
			names := 0
			fork := func(parent *wal.Branch, fromHistory bool) *wal.Branch {
				names++
				name := fmt.Sprintf("b%d", names)
				var child *wal.Branch
				var err error
				if fromHistory {
					fresh := reload(parent)
					at := fresh.BaseLSN + rng.Int63n(fresh.HistoryHighWater()-fresh.BaseLSN+1)
					child, err = f.restore.CreateBranchAtLSN(project, parent.ID, name, at)
					if !tolerate(err, "fork from history") {
						child, err = f.branches.CreateBranch(project, name, parent.ID)
					}
				} else {
					child, err = f.branches.CreateBranch(project, name, parent.ID)
				}
				require.NoError(t, err)
				all = append(all, child)
				return child
			}
			agreed, declined := 0, 0
			compareAll := func(label string) {
				for _, s := range sources {
					if reproComparePlans(t, svc, s.ID, fmt.Sprintf("%s, source %s", label, s.Name)) == reproAgreed {
						agreed++
					} else {
						declined++
					}
				}
			}
			checkPointReads := func(label string) {
				for _, b := range all {
					b = reload(b)
					bounds := []int64{b.HeadLSN, b.BaseLSN, b.HistoryHighWater()}
					for i := 0; i < 4; i++ {
						bounds = append(bounds, rng.Int63n(b.HistoryHighWater()+1))
					}
					for _, bound := range bounds {
						for _, collection := range []string{"users", "orders", "typed"} {
							pool := universe[collection]
							keys := make([]string, 0, len(pool))
							for _, key := range pool {
								if rng.Intn(3) != 0 {
									keys = append(keys, key)
								}
							}
							want, wantErr := f.mat.MaterializeCollectionAtLSN(b, collection, bound)
							got, gotErr := f.mat.MaterializeDocumentsAtLSN(b, collection, keys, bound)
							at := fmt.Sprintf("%s: %s on %s at LSN %d (base %d, head %d)", label, collection, b.Name, bound, b.BaseLSN, b.HeadLSN)
							// The single-document read walks the same segments
							// under the same floors. Where it refuses a key as
							// reclaimed, the read of many keys has no better
							// record of that key and must refuse too; where
							// both answer, they must agree.
							for i := 0; i < 3 && len(keys) > 0; i++ {
								key := keys[rng.Intn(len(keys))]
								one, oneErr := f.mat.MaterializeDocumentAtLSN(b, collection, key, bound)
								if oneErr != nil {
									require.True(t, errors.Is(oneErr, wal.ErrHistoryReclaimed), "%s key %s: %v", at, key, oneErr)
									singleRefused++
									require.Error(t, gotErr, "%s: the single-document read refuses key %s as reclaimed (%v), the multi-document read answers %v",
										at, key, oneErr, got[key])
									continue
								}
								if gotErr == nil {
									singleCompared++
									sameDocument(t, one, got[key], at+" single vs multi, key "+key)
								}
							}
							if gotErr != nil {
								pointDeclined++
								continue
							}
							if wantErr != nil {
								pointOnly++ // every key settled above the hole
								continue
							}
							pointCompared++
							for _, key := range keys {
								sameDocument(t, want[key], got[key], at+" key "+key)
							}
							require.LessOrEqual(t, len(got), len(keys), at)
						}
					}
				}
			}

			main, err := f.branches.CreateBranch(project, "main", "")
			require.NoError(t, err)
			all = append(all, main)
			applyMixedWorkload(t, rng, writer(main), 30)
			if rng.Intn(2) == 0 {
				snapshotAt(main, true)
			}
			target := main
			if rng.Intn(2) == 0 {
				target = fork(main, false)
				applyMixedWorkload(t, rng, writer(target), 20)
				applyMixedWorkload(t, rng, writer(main), 8)
			}
			source := fork(target, rng.Intn(3) == 0)
			sources = append(sources, source)
			applyMixedWorkload(t, rng, writer(source), 5+rng.Intn(15))
			applyMixedWorkload(t, rng, writer(target), rng.Intn(10))
			compareAll("before any GC")

			collect := func(label string) {
				_, err := gcService.RunProject(ctx, project, zeroRetention)
				require.NoError(t, err, label)
				compareAll(label)
				checkPointReads(label)
			}
			// First GC, with a snapshot of the parent chain as the commit's
			// test has it — everything below is what that test never does.
			snapshotAt(main, true)
			snapshotAt(target, true)
			collect("first GC")

			for step := 0; step < 14; step++ {
				label := fmt.Sprintf("step %d", step)
				anyBranch := all[rng.Intn(len(all))]
				anySource := sources[rng.Intn(len(sources))]
				switch rng.Intn(11) {
				case 0, 1:
					applyMixedWorkload(t, rng, writer(anySource), 3+rng.Intn(10))
					label += ": write source " + anySource.Name
				case 2:
					applyMixedWorkload(t, rng, writer(target), 1+rng.Intn(10))
					label += ": write target"
				case 3:
					applyMixedWorkload(t, rng, writer(main), 1+rng.Intn(8))
					label += ": write main"
				case 4:
					snapshotAt(anyBranch, true)
					label += ": snapshot head of " + anyBranch.Name
				case 5:
					snapshotAt(anyBranch, false)
					label += ": snapshot inside " + anyBranch.Name
				case 6:
					resetSomewhere(anyBranch)
					applyMixedWorkload(t, rng, writer(anyBranch), rng.Intn(6))
					label += ": reset " + anyBranch.Name
				case 7, 8:
					collect(label + ": GC")
					continue
				case 9:
					if len(sources) < 4 {
						child := fork(target, rng.Intn(2) == 0)
						applyMixedWorkload(t, rng, writer(child), 3+rng.Intn(10))
						sources = append(sources, child)
						label += ": fork " + child.Name + " from target"
					}
				case 10:
					if len(sources) < 4 {
						// A branch of a source: that source is now a target
						// whose own segment has snapshots and floors.
						child := fork(anySource, rng.Intn(2) == 0)
						applyMixedWorkload(t, rng, writer(child), 3+rng.Intn(10))
						sources = append(sources, child)
						label += ": fork " + child.Name + " from " + anySource.Name
					}
				}
				compareAll(label)
			}
			for _, b := range all {
				if rng.Intn(2) == 0 {
					snapshotAt(b, true)
				}
			}
			collect("last GC")
			t.Logf("plans: %d agreed, %d declined", agreed, declined)
			agreedTotal += agreed
			declinedTotal += declined
		})
	}
	t.Logf("plans compared: %d agreed with the reference, %d declined by the changed-documents path", agreedTotal, declinedTotal)
	t.Logf("point reads: %d compared with the collection read, %d declined, %d answered where the collection read is refused",
		pointCompared, pointDeclined, pointOnly)
	t.Logf("single-document reads: %d compared with the multi-document read, %d refused as reclaimed (the multi-document read refused each of those too)",
		singleCompared, singleRefused)
	require.NotZero(t, agreedTotal, "the comparison must not be vacuous")
	require.NotZero(t, pointCompared, "the point-read comparison must not be vacuous")
}

// Concurrent GC, best effort: while a GC reclaims the parent's entries, every
// plan the changed-documents path returns must still be the plan from before.
func TestMergeChanged_ConcurrentGC(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	answered, declined := 0, 0
	for round := 0; round < 6; round++ {
		f, svc, _ := newCountingMergeFixture(t, db)
		project := fmt.Sprintf("repro-concurrent-gc-%d", round)
		main, err := f.branches.CreateBranch(project, "main", "")
		require.NoError(t, err)
		mw := walwriter.New(f.wal, f.branches, f.mat, main)
		// Several entries per document, so a half-finished deletion can
		// leave an older entry of a document standing.
		for pass := 0; pass < 6; pass++ {
			batch := make([]bson.M, 0, 400)
			for i := 0; i < 400; i++ {
				batch = append(batch, bson.M{"_id": fmt.Sprintf("p%04d", i), "price": int32(pass)})
			}
			_, err = mw.PutMany(ctx, "products", batch)
			require.NoError(t, err)
		}
		main, err = f.branches.GetBranchByID(main.ID)
		require.NoError(t, err)
		_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
		require.NoError(t, err)

		feature, err := f.branches.CreateBranch(project, "feature", main.ID)
		require.NoError(t, err)
		fw := walwriter.New(f.wal, f.branches, f.mat, feature)
		batch := make([]bson.M, 0, 200)
		for i := 0; i < 200; i++ {
			batch = append(batch, bson.M{"_id": fmt.Sprintf("p%04d", i*2), "price": int32(1000 + i)})
		}
		_, err = fw.PutMany(ctx, "products", batch)
		require.NoError(t, err)
		before := requirePlansAgree(t, svc, feature.ID, "before GC")
		require.Len(t, before.Changes, 200)

		var wg sync.WaitGroup
		wg.Add(1)
		var gcErr error
		go func() {
			defer wg.Done()
			_, gcErr = gc.NewService(f.wal, f.branches, f.snapshots).RunProject(ctx, project, zeroRetention)
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		for running := true; running; {
			select {
			case <-done:
				running = false
			default:
			}
			plan, err := svc.ComputeChanged(feature.ID)
			if err != nil {
				declined++
				continue
			}
			answered++
			requireSamePlan(t, before, plan, fmt.Sprintf("round %d: plan computed while GC runs", round))
		}
		require.NoError(t, gcErr)
		require.Zero(t, countBranchEntriesIn(t, f, main.ID, "products"), "GC reclaimed the parent")
		requirePlansAgree(t, svc, feature.ID, "after GC")
	}
	t.Logf("plans computed while GC ran: %d answered, %d declined", answered, declined)
}
