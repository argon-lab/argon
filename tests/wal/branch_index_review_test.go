package wal_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// reviewOldReleaseOpen does what NewBranchService of releases up to 2.12.0
// did when a command started.
func reviewOldReleaseOpen(db *mongo.Database) error {
	_, err := db.Collection("wal_branches").Indexes().CreateMany(context.Background(), []mongo.IndexModel{
		{Keys: bson.D{{Key: "project_id", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "project_id", Value: 1}, {Key: "is_deleted", Value: 1}}},
	})
	return err
}

func reviewSeedLegacyBranches(t *testing.T, db *mongo.Database, project string, n int) {
	t.Helper()
	docs := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, bson.M{"_id": fmt.Sprintf("%s-b%06d", project, i), "project_id": project,
			"name": fmt.Sprintf("b%06d", i), "is_deleted": i%3 == 0, "head_lsn": int64(1), "base_lsn": int64(0)})
	}
	_, err := db.Collection("wal_branches").InsertMany(context.Background(), docs)
	require.NoError(t, err)
}

// reviewPlanStages returns the stages and index names of the winning plan.
func reviewPlanStages(t *testing.T, db *mongo.Database, filter bson.M) (stages []string, indexes []string) {
	t.Helper()
	var out bson.M
	require.NoError(t, db.RunCommand(context.Background(), bson.D{
		{Key: "explain", Value: bson.D{{Key: "find", Value: "wal_branches"}, {Key: "filter", Value: filter}}},
		{Key: "verbosity", Value: "queryPlanner"},
	}).Decode(&out))
	var walk func(node interface{})
	walk = func(node interface{}) {
		switch v := node.(type) {
		case bson.M:
			if stage, ok := v["stage"].(string); ok {
				stages = append(stages, stage)
			}
			if name, ok := v["indexName"].(string); ok {
				indexes = append(indexes, name)
			}
			for key, child := range v {
				if key == "rejectedPlans" {
					continue
				}
				walk(child)
			}
		case bson.A:
			for _, child := range v {
				walk(child)
			}
		}
	}
	planner := out["queryPlanner"].(bson.M)
	walk(planner["winningPlan"])
	return stages, indexes
}

// Every query shape the branch service sends, on a database upgraded from
// the legacy index: none may scan the collection where it used an index.
func TestReview_BranchQueriesKeepAnIndexAfterTheLegacyIndexIsRetired(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, reviewOldReleaseOpen(db))
	reviewSeedLegacyBranches(t, db, "p1", 3000)
	reviewSeedLegacyBranches(t, db, "p2", 3000)

	shapes := map[string]bson.M{
		"GetBranch":        {"project_id": "p1", "name": "b000001", "is_deleted": false},
		"ListBranches":     {"project_id": "p1", "is_deleted": false},
		"ListBranchesAny":  {"project_id": "p1"},
		"RequireDeletable": {"project_id": "p1", "parent_id": "x", "is_deleted": false},
		"GetBranchByID":    {"_id": "p1-b000001", "is_deleted": false},
	}
	before := map[string][]string{}
	for name, filter := range shapes {
		stages, indexes := reviewPlanStages(t, db, filter)
		require.NotContains(t, stages, "COLLSCAN", "before: %s", name)
		before[name] = indexes
	}
	walService, err := wal.NewService(db)
	require.NoError(t, err)
	_, err = branchwal.NewBranchService(db, walService)
	require.NoError(t, err)
	for name, filter := range shapes {
		stages, indexes := reviewPlanStages(t, db, filter)
		t.Logf("%s: before %v, after %v %v", name, before[name], stages, indexes)
		assert.NotContains(t, stages, "COLLSCAN", "after: %s", name)
	}
	_, indexes := reviewPlanStages(t, db, shapes["GetBranch"])
	assert.Equal(t, []string{"project_id_1_name_1_live"}, indexes, "GetBranch is a point lookup on the live-name index")
}

// Many commands start at once on a metadata database an older release
// wrote. Each must open, and the legacy index must be gone.
func TestReview_ConcurrentOpensOfALegacyDatabaseAllSucceed(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, reviewOldReleaseOpen(db))
	reviewSeedLegacyBranches(t, db, "p1", 20000)
	walService, err := wal.NewService(db)
	require.NoError(t, err)

	const openers = 24
	errs := make([]error, openers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = branchwal.NewBranchService(db, walService)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "opener %d", i)
	}
	names := branchIndexNames(t, db)
	assert.NotContains(t, names, "project_id_1_name_1")
	assert.Contains(t, names, "project_id_1_name_1_live")
}

// The state the changelog describes: an older release connected after the
// upgrade and put the legacy index back, and no name has been reused yet.
// Agents then reuse the names of branches they deleted, together. Each such
// create must make its branch: no live branch has any of these names.
func TestReview_ConcurrentReuseOfDeletedNamesWithTheLegacyIndexPutBack(t *testing.T) {
	const project = "reuse-together"
	const callers = 16
	for round := 0; round < 6; round++ {
		db := setupTestDB(t)
		f := newSnapshotFixture(t, db)
		main, err := f.branches.CreateBranch(project, "main", "")
		require.NoError(t, err)
		for i := 0; i < callers; i++ {
			name := fmt.Sprintf("run-%d-%d", round, i)
			_, err := f.branches.CreateBranch(project, name, main.ID)
			require.NoError(t, err)
			require.NoError(t, f.branches.DeleteBranch(project, name))
		}
		require.NoError(t, reviewOldReleaseOpen(db), "the older release starts")

		errs := make([]error, callers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.branches.CreateBranch(project, fmt.Sprintf("run-%d-%d", round, i), main.ID)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			assert.NoError(t, err, "round %d: run-%d-%d has no live branch and must be created", round, round, i)
		}
		for i := 0; i < callers; i++ {
			name := fmt.Sprintf("run-%d-%d", round, i)
			live, _ := db.Collection("wal_branches").CountDocuments(context.Background(), bson.M{"project_id": project, "name": name, "is_deleted": false})
			assert.EqualValues(t, 1, live, "%s: one live branch", name)
			assert.EqualValues(t, 2, createBranchEntries(t, db, project, name), "%s: one create_branch record per branch made", name)
		}
	}
}

// Commands of this release start (open) while others create and delete
// branches under names of their own. No open and no create may fail.
func TestReview_OpensAndCreatesTogether(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, reviewOldReleaseOpen(db))
	reviewSeedLegacyBranches(t, db, "old", 5000)
	f := newSnapshotFixture(t, db)
	const project = "open-and-create"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var openErrs, createErrs sync.Map
	var opens, creates atomic.Int64
	for o := 0; o < 4; o++ {
		wg.Add(1)
		go func(o int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := branchwal.NewBranchService(db, f.wal); err != nil {
					openErrs.Store(fmt.Sprintf("%d-%d", o, opens.Load()), err)
				}
				opens.Add(1)
			}
		}(o)
	}
	for c := 0; c < 8; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			name := fmt.Sprintf("agent-%d", c)
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := f.branches.CreateBranch(project, name, main.ID); err != nil {
					createErrs.Store(fmt.Sprintf("%s#%d create", name, n), err)
					continue
				}
				creates.Add(1)
				if err := f.branches.DeleteBranch(project, name); err != nil {
					createErrs.Store(fmt.Sprintf("%s#%d delete", name, n), err)
				}
			}
		}(c)
	}
	time.Sleep(4 * time.Second)
	close(stop)
	wg.Wait()
	t.Logf("%d opens, %d creates", opens.Load(), creates.Load())
	openErrs.Range(func(k, v interface{}) bool { t.Errorf("open %v: %v", k, v); return true })
	createErrs.Range(func(k, v interface{}) bool { t.Errorf("%v: %v", k, v); return true })
}

// Mixed versions, as the changelog allows for a while: commands of an older
// release keep starting (each puts the legacy index back) while commands of
// this release start and create branches under names never used before.
func TestReview_OlderReleaseKeepsStartingWhileThisOneWorks(t *testing.T) {
	db := setupTestDB(t)
	reviewSeedLegacyBranches(t, db, "old", 3000)
	f := newSnapshotFixture(t, db)
	const project = "mixed-running"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var newOpenErrs, oldOpenErrs, createErrs sync.Map
	var newOpens, oldOpens, creates atomic.Int64
	running := func() bool {
		select {
		case <-stop:
			return false
		default:
			return true
		}
	}
	for o := 0; o < 2; o++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for running() {
				if err := reviewOldReleaseOpen(db); err != nil {
					oldOpenErrs.Store(oldOpens.Load(), err)
				}
				oldOpens.Add(1)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for running() {
				if _, err := branchwal.NewBranchService(db, f.wal); err != nil {
					newOpenErrs.Store(newOpens.Load(), err)
				}
				newOpens.Add(1)
			}
		}()
	}
	for c := 0; c < 6; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for n := 0; running(); n++ {
				name := fmt.Sprintf("agent-%d-%d", c, n)
				if _, err := f.branches.CreateBranch(project, name, main.ID); err != nil {
					createErrs.Store(name, err)
					continue
				}
				creates.Add(1)
			}
		}(c)
	}
	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()
	t.Logf("%d opens of this release, %d of the older one, %d creates", newOpens.Load(), oldOpens.Load(), creates.Load())
	seen := map[string]int{}
	// Not asserted: an older command can lose its index build to this
	// release retiring the index (IndexBuildAborted), which is the older
	// release's failure and outside what this change can fix.
	oldOpenErrs.Range(func(k, v interface{}) bool {
		var command mongo.CommandError
		if errors.As(v.(error), &command) {
			seen[command.Name]++
		} else {
			seen[fmt.Sprintf("%.80v", v)]++
		}
		return true
	})
	for name, n := range seen {
		t.Logf("older release's open failed %d times with %s", n, name)
	}
	newOpenErrs.Range(func(k, v interface{}) bool { t.Errorf("open %v: %v", k, v); return true })
	createErrs.Range(func(k, v interface{}) bool { t.Errorf("create %v: %v", k, v); return true })
}

// Branch listings have no sort: their order is the order of the index the
// planner picks. Until this change that was the order of creation.
func TestReview_ListBranchesOrderOnAFreshDatabase(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "list-order"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	created := []string{"main"}
	for _, name := range []string{"zeta", "alpha", "nightly", "beta", "release", "feature-1", "aa"} {
		_, err := f.branches.CreateBranch(project, name, main.ID)
		require.NoError(t, err)
		created = append(created, name)
	}
	// Another project, so the planner has something to choose between.
	for i := 0; i < 50; i++ {
		_, err := f.branches.CreateBranch("other", fmt.Sprintf("b%02d", i), "")
		require.NoError(t, err)
	}
	stages, indexes := reviewPlanStages(t, db, bson.M{"project_id": project, "is_deleted": false})
	t.Logf("ListBranches plan: %v %v", stages, indexes)
	for attempt := 0; attempt < 5; attempt++ {
		branches, err := f.branches.ListBranches(project)
		require.NoError(t, err)
		var listed []string
		for _, b := range branches {
			listed = append(listed, b.Name)
		}
		assert.Equal(t, created, listed, "branches are listed in the order they were created (attempt %d)", attempt)
	}
}

func TestReview_ListBranchesOrderWithManyBranches(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			db := setupTestDB(t)
			if legacy {
				require.NoError(t, reviewOldReleaseOpen(db))
			}
			const project = "list-order-many"
			var docs []interface{}
			var created []string
			for i := 0; i < 400; i++ {
				name := fmt.Sprintf("b%04d", 9999-i) // creation order is the reverse of name order
				deleted := i%4 == 1
				docs = append(docs, bson.M{"_id": fmt.Sprintf("id%04d", i), "project_id": project, "name": name,
					"is_deleted": deleted, "head_lsn": int64(1), "base_lsn": int64(0)})
				if !deleted {
					created = append(created, name)
				}
			}
			_, err := db.Collection("wal_branches").InsertMany(context.Background(), docs)
			require.NoError(t, err)
			f := newSnapshotFixture(t, db)
			stages, indexes := reviewPlanStages(t, db, bson.M{"project_id": project, "is_deleted": false})
			t.Logf("ListBranches plan: %v %v", stages, indexes)
			for attempt := 0; attempt < 5; attempt++ {
				branches, err := f.branches.ListBranches(project)
				require.NoError(t, err)
				var listed []string
				for _, b := range branches {
					listed = append(listed, b.Name)
				}
				require.Equal(t, created, listed, "branches are listed in the order they were created (attempt %d)", attempt)
			}
		})
	}
}
