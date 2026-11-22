package wal_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/undo"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// startIngesterWith runs a capture with the given service (a fresh one stands
// in for a restarted process).
func (f *ingestFixture) startIngesterWith(t *testing.T, svc *ingest.Service) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- svc.Run(ctx, f.branchID, ingest.WithReady(ready)) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("ingester exited before opening the stream: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("ingester never became ready")
	}
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("ingester did not stop")
		}
	}
}

func reviewAllEntries(t *testing.T, f *ingestFixture) []*wal.Entry {
	t.Helper()
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(main.ID, "", 0, main.HeadLSN)
	require.NoError(t, err)
	return entries
}

func reviewAssertNoMarkerEntries(t *testing.T, f *ingestFixture) {
	t.Helper()
	for _, e := range reviewAllEntries(t, f) {
		assert.NotEqual(t, wal.MergeMarkerCollection, e.Collection, "marker reached the WAL: %+v", e)
	}
}

// After a live merge, the marker never reaches the WAL, the materialized
// branch, or a branch checked out from it.
func TestMarkerReview_NoLeakIntoWALOrCheckouts(t *testing.T) {
	f := newIngestFixture(t, "marker-review-leak")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	feature, err := f.branches.CreateBranch("marker-review-leak", "feature", main.ID)
	require.NoError(t, err)
	writer := walwriter.New(f.wal, f.branches, f.mat, feature)
	_, err = writer.Put(ctx, "docs", bson.M{"_id": "a", "v": int32(1)})
	require.NoError(t, err)
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	plan, err := merges.Preview(ctx, feature.ID)
	require.NoError(t, err)
	_, err = merges.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)

	reviewAssertNoMarkerEntries(t, f)
	main, err = f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	state, err := f.mat.MaterializeBranch(main)
	require.NoError(t, err)
	assert.NotContains(t, state, wal.MergeMarkerCollection)

	// A branch forked from the live branch after the merge and checked out.
	child, err := f.branches.CreateBranch("marker-review-leak", "child", main.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.client.Database(checkout.PhysicalDBName(child.ID)).Drop(context.Background()) })
	info, err := f.checkout.Checkout(ctx, child.ID)
	require.NoError(t, err)
	names, err := f.client.Database(info.PhysicalDB).ListCollectionNames(ctx, bson.M{})
	require.NoError(t, err)
	assert.NotContains(t, names, wal.MergeMarkerCollection)
	assert.Contains(t, names, "docs")
}

// Merges applied while capture is stopped are attributed once capture comes
// back in a new process, each to its own plan.
func TestMarkerReview_MergesWhileCaptureStopped(t *testing.T) {
	f := newIngestFixture(t, "marker-review-stopped")
	ctx := context.Background()
	stop := f.startIngester(t)
	_, err := f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "base", "v": int32(0)})
	require.NoError(t, err)
	f.waitForEntries(t, "docs", 1)
	stop()

	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	var plans []*merge.Plan
	for i := 0; i < 2; i++ {
		feature, err := f.branches.CreateBranch("marker-review-stopped", fmt.Sprintf("f%d", i), main.ID)
		require.NoError(t, err)
		w := walwriter.New(f.wal, f.branches, f.mat, feature)
		for j := 0; j < 3; j++ {
			_, err = w.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("f%d-%d", i, j)})
			require.NoError(t, err)
		}
		p, err := merges.Preview(ctx, feature.ID)
		require.NoError(t, err)
		_, err = merges.Apply(ctx, p.ID, "")
		require.NoError(t, err)
		plans = append(plans, p)
	}
	// An application write after both merges.
	_, err = f.physical.Collection("docs").InsertOne(ctx, bson.M{"_id": "app"})
	require.NoError(t, err)

	stop = f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
	defer stop()
	f.waitForEntries(t, "docs", 8)

	reviewAssertNoMarkerEntries(t, f)
	byPlan := map[string]int{}
	for _, e := range reviewAllEntries(t, f) {
		if !e.IsData() || e.Collection != "docs" {
			continue
		}
		p, _ := e.Metadata["merge_plan"].(string)
		switch e.DocumentID {
		case "base", "app":
			assert.Equal(t, "ingest", e.Actor, e.DocumentID)
			assert.Empty(t, p, e.DocumentID)
		default:
			byPlan[p]++
		}
	}
	assert.Equal(t, map[string]int{plans[0].ID.Hex(): 3, plans[1].ID.Hex(): 3}, byPlan)
}

// Two merges race into a live branch whose marker collection does not exist
// yet; both transactions create it implicitly.
func TestMarkerReview_ConcurrentMergesIntoFreshLiveBranch(t *testing.T) {
	f := newIngestFixture(t, "marker-review-race")
	ctx := context.Background()
	stop := f.startIngester(t)
	defer stop()

	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	var ids []string
	var planIDs []*merge.Plan
	for i := 0; i < 2; i++ {
		feature, err := f.branches.CreateBranch("marker-review-race", fmt.Sprintf("f%d", i), main.ID)
		require.NoError(t, err)
		w := walwriter.New(f.wal, f.branches, f.mat, feature)
		_, err = w.Put(ctx, "docs", bson.M{"_id": fmt.Sprintf("f%d", i)})
		require.NoError(t, err)
		ids = append(ids, feature.ID)
		p, err := merges.Preview(ctx, feature.ID)
		require.NoError(t, err)
		planIDs = append(planIDs, p)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range planIDs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = merges.Apply(ctx, planIDs[i].ID, "")
		}(i)
	}
	close(start)
	wg.Wait()
	t.Logf("apply errors: %v", errs)
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	require.GreaterOrEqual(t, succeeded, 1, "at least one merge applies: %v", errs)
	f.waitForEntries(t, "docs", succeeded)
	time.Sleep(500 * time.Millisecond)

	reviewAssertNoMarkerEntries(t, f)
	count, err := f.physical.Collection(wal.MergeMarkerCollection).CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.Zero(t, count)
	attributed := 0
	for _, e := range reviewAllEntries(t, f) {
		if e.IsData() && e.Collection == "docs" {
			p, _ := e.Metadata["merge_plan"].(string)
			assert.NotEmpty(t, p, "merged entry %s is attributed", e.DocumentID)
			attributed++
		}
	}
	assert.Equal(t, succeeded, attributed)
	// A failed plan is still pending and can be applied after a new preview.
	for i, err := range errs {
		if err != nil {
			p, gerr := merges.GetPlan(ctx, planIDs[i].ID)
			require.NoError(t, gerr)
			assert.Equal(t, merge.StatusPending, p.Status)
		}
	}
	_ = ids
}

// Capture is stopped and restarted at random points while merges into the
// live branch commit. Every merged change is recorded exactly once, attributed
// to its plan, and the marker never reaches the WAL.
func TestMarkerReview_CaptureStopRacesMerges(t *testing.T) {
	f := newIngestFixture(t, "marker-review-restart")
	ctx := context.Background()
	merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
	main, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)

	const rounds, perMerge = 4, 150
	rng := rand.New(rand.NewSource(1))
	want := map[string]int{}
	stop := f.startIngester(t)
	// docs exists before the merges; a merge that creates the collection is
	// covered by TestMarkerReview_StopAfterReadingAMergeThatCreatesACollection.
	require.NoError(t, f.physical.CreateCollection(ctx, "docs"))
	for r := 0; r < rounds; r++ {
		feature, err := f.branches.CreateBranch("marker-review-restart", fmt.Sprintf("f%d", r), main.ID)
		require.NoError(t, err)
		w := walwriter.New(f.wal, f.branches, f.mat, feature)
		docs := make([]bson.M, perMerge)
		for i := range docs {
			docs[i] = bson.M{"_id": fmt.Sprintf("r%d-%d", r, i), "pad": fmt.Sprintf("%0256d", i)}
		}
		_, err = w.PutMany(ctx, "docs", docs)
		require.NoError(t, err)
		p, err := merges.Preview(ctx, feature.ID)
		require.NoError(t, err)
		applied := make(chan error, 1)
		go func() { _, err := merges.Apply(ctx, p.ID, ""); applied <- err }()
		time.Sleep(time.Duration(rng.Intn(200)) * time.Millisecond)
		stop()
		require.NoError(t, <-applied)
		want[p.ID.Hex()] = perMerge
		stop = f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
		f.waitForEntries(t, "docs", (r+1)*perMerge)
	}
	defer stop()
	time.Sleep(500 * time.Millisecond)

	reviewAssertNoMarkerEntries(t, f)
	got := map[string]int{}
	seen := map[string]int{}
	for _, e := range reviewAllEntries(t, f) {
		if !e.IsData() || e.Collection != "docs" {
			continue
		}
		p, _ := e.Metadata["merge_plan"].(string)
		got[p]++
		seen[e.DocumentID]++
		assert.Equal(t, "merge:"+fmt.Sprintf("f%c", e.DocumentID[1]), e.Actor, e.DocumentID)
	}
	assert.Equal(t, want, got)
	for id, n := range seen {
		assert.Equal(t, 1, n, "document %s recorded %d times", id, n)
	}
}

// A merge into a checked-out branch that brings a collection the branch does
// not have yet creates it inside the merge's transaction, after the marker.
// Capture is stopped after it has read the transaction and before it flushed
// it (the flush waits for a barrier, about half a second), as on an API
// server shutdown or a Ctrl-C of argon watch. On restart the merge's changes
// must still be attributed to the merge.
func TestMarkerReview_StopAfterReadingAMergeThatCreatesACollection(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprintf("collection_exists=%v", existing), func(t *testing.T) {
			project := fmt.Sprintf("marker-review-create-%v", existing)
			f := newIngestFixture(t, project)
			ctx := context.Background()
			stop := f.startIngester(t)
			if existing {
				require.NoError(t, f.physical.CreateCollection(ctx, "docs"))
			}
			merges := merge.NewService(f.metaDB, f.wal, f.branches, f.mat, f.client)
			main, err := f.branches.GetBranchByID(f.branchID)
			require.NoError(t, err)
			feature, err := f.branches.CreateBranch(project, "feature", main.ID)
			require.NoError(t, err)
			w := walwriter.New(f.wal, f.branches, f.mat, feature)
			_, err = w.Put(ctx, "docs", bson.M{"_id": "x"})
			require.NoError(t, err)
			plan, err := merges.Preview(ctx, feature.ID)
			require.NoError(t, err)
			_, err = merges.Apply(ctx, plan.ID, "")
			require.NoError(t, err)
			time.Sleep(200 * time.Millisecond)
			stop()

			stop = f.startIngesterWith(t, ingest.NewService(f.client, f.metaDB, f.wal, f.branches))
			defer stop()
			f.waitForEntries(t, "docs", 1)
			reviewAssertNoMarkerEntries(t, f)
			for _, e := range reviewAllEntries(t, f) {
				if e.IsData() && e.Collection == "docs" {
					assert.Equal(t, "merge:feature", e.Actor, "merged %s", e.DocumentID)
					assert.Equal(t, plan.ID.Hex(), e.Metadata["merge_plan"], "merged %s", e.DocumentID)
				}
			}
			// The undo by plan must be possible.
			main, err = f.branches.GetBranchByID(f.branchID)
			require.NoError(t, err)
			applied, err := merges.GetPlan(ctx, plan.ID)
			require.NoError(t, err)
			undos := undo.NewService(f.wal, f.branches, f.client, f.mat)
			_, err = undos.BuildMergePlan(main, plan.ID.Hex(), applied.MergeFromLSN, applied.MergeChanges)
			assert.NoError(t, err)
		})
	}
}
