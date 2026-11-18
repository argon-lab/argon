package wal_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/gc"
	"github.com/argon-lab/argon/v2/internal/importer"
	"github.com/argon-lab/argon/v2/internal/merge"
	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// TestMergeScale times plan computation against database size. It imports a
// database of each requested size the way `argon import database` does
// (entries plus the post-import snapshot), forks a branch, changes a fixed
// number of documents on it, and times the full reference against the
// changed-documents path. The two plans are compared, so a run also checks
// agreement at sizes the ordinary tests do not reach.
//
// It runs only when ARGON_MERGE_SCALE_DOCS lists the sizes, for example
//
//	ARGON_MERGE_SCALE_DOCS=10000,100000,1000000 go test ./tests/wal -run TestMergeScale -v -timeout 2h
//
// ARGON_MERGE_SCALE_CHANGES sets how many documents the branch changes
// (default 100). The timings describe the machine and MongoDB they ran on.
func TestMergeScale(t *testing.T) {
	spec := os.Getenv("ARGON_MERGE_SCALE_DOCS")
	if spec == "" {
		t.Skip("set ARGON_MERGE_SCALE_DOCS to a comma-separated list of document counts to run")
	}
	changes := 100
	if v := os.Getenv("ARGON_MERGE_SCALE_CHANGES"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err)
		changes = n
	}
	for _, field := range strings.Split(spec, ",") {
		docs, err := strconv.Atoi(strings.TrimSpace(field))
		require.NoError(t, err)
		t.Run(fmt.Sprintf("%d-docs", docs), func(t *testing.T) { runMergeScale(t, docs, changes) })
	}
}

func medianMillis(samples []time.Duration) float64 {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return float64(sorted[len(sorted)/2].Microseconds()) / 1000
}

func runMergeScale(t *testing.T, docs, changes int) {
	ctx := context.Background()
	db := setupTestDB(t)
	sourceName := fmt.Sprintf("argon_merge_scale_src_%d_%d", docs, time.Now().UnixNano())
	source := setupTestSourceDB(t, sourceName)

	payload := strings.Repeat("x", 128)
	const insertBatch = 5000
	batch := make([]interface{}, 0, insertBatch)
	for i := 0; i < docs; i++ {
		batch = append(batch, bson.M{"_id": fmt.Sprintf("doc-%09d", i), "n": int32(i), "payload": payload})
		if len(batch) == insertBatch || i == docs-1 {
			_, err := source.Collection("items").InsertMany(ctx, batch)
			require.NoError(t, err)
			batch = batch[:0]
		}
	}

	f := newSnapshotFixture(t, db)
	projects, err := projectwal.NewProjectService(db, f.wal, f.branches)
	require.NoError(t, err)
	importService := importer.NewImportService(f.wal, projects, f.branches)
	project := fmt.Sprintf("merge-scale-%d", docs)

	started := time.Now()
	result, err := importService.ImportDatabase(ctx, importer.ImportOptions{
		MongoURI: getTestMongoURI(), DatabaseName: sourceName, ProjectName: project, SourceQuiesced: true,
	})
	require.NoError(t, err)
	require.EqualValues(t, docs, result.ImportedDocs)
	main, err := f.branches.GetBranchByID(result.BranchID)
	require.NoError(t, err)
	_, err = f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	t.Logf("docs=%d import_and_snapshot_ms=%.0f", docs, float64(time.Since(started).Milliseconds()))

	feature, err := f.branches.CreateBranch(result.ProjectID, "feature", main.ID)
	require.NoError(t, err)
	fw := walwriter.New(f.wal, f.branches, f.mat, feature)
	step := docs / changes
	if step == 0 {
		step = 1
	}
	wantChanges := 0
	for i := 0; i < changes; i++ {
		id := fmt.Sprintf("doc-%09d", (i*step)%docs)
		switch i % 5 {
		case 3: // an insert
			_, err = fw.Put(ctx, "items", bson.M{"_id": fmt.Sprintf("new-%d", i), "n": int32(i), "payload": payload})
		case 4: // a delete
			_, _, err = fw.Delete(ctx, "items", id)
		default: // an update
			_, err = fw.Put(ctx, "items", bson.M{"_id": id, "n": int32(-i - 1), "payload": payload})
		}
		require.NoError(t, err)
		wantChanges++
	}
	if step == 1 && changes > docs {
		wantChanges = 0 // ids repeat; the count is checked against the reference only
	}

	svc := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
	time3 := func(compute func(string) (*merge.Plan, error)) (*merge.Plan, float64) {
		var plan *merge.Plan
		samples := make([]time.Duration, 0, 3)
		for i := 0; i < 3; i++ {
			begin := time.Now()
			p, err := compute(feature.ID)
			require.NoError(t, err)
			samples = append(samples, time.Since(begin))
			plan = p
		}
		return plan, medianMillis(samples)
	}

	full, fullMS := time3(svc.ComputeFull)
	changed, changedMS := time3(svc.ComputeChanged)
	requireSamePlan(t, full, changed, "changed vs full")
	if wantChanges > 0 {
		require.Len(t, changed.Changes, wantChanges)
	}
	t.Logf("docs=%d changed_docs=%d plan_full_ms=%.1f plan_changed_ms=%.1f", docs, len(changed.Changes), fullMS, changedMS)

	// The first half of a merge: compute the plan and persist it for review.
	begin := time.Now()
	plan, err := svc.Preview(ctx, feature.ID)
	require.NoError(t, err)
	previewMS := float64(time.Since(begin).Microseconds()) / 1000

	// After garbage collection the parent's imported entries are gone and
	// its snapshot is the record of the fork state: the changed-documents
	// path loads it, which is a read the size of the collection.
	_, err = gc.NewService(f.wal, f.branches, f.snapshots).RunProject(ctx, result.ProjectID, zeroRetention)
	require.NoError(t, err)
	fullGC, fullGCMS := time3(svc.ComputeFull)
	changedGC, changedGCMS := time3(svc.ComputeChanged)
	requireSamePlan(t, fullGC, changedGC, "changed vs full after GC")
	requireSamePlan(t, full, changedGC, "plan after GC")
	t.Logf("docs=%d after_gc plan_full_ms=%.1f plan_changed_ms=%.1f", docs, fullGCMS, changedGCMS)

	// The second half: apply the reviewed plan to the parent.
	begin = time.Now()
	_, err = svc.Apply(ctx, plan.ID, "")
	require.NoError(t, err)
	applyMS := float64(time.Since(begin).Microseconds()) / 1000
	t.Logf("docs=%d merge_preview_ms=%.1f merge_apply_ms=%.1f", docs, previewMS, applyMS)
}
