package wal_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Review (pre-existing, same shape as #220 one level up): CreateProject
// checks the name, appends a create_project record, creates the main branch
// and only then inserts the project. Creates of one project name that
// arrive together each use their own project ID, so the branch name index
// never sees them collide; all but one fail at the project name index with
// a raw E11000, after each made a main branch and two WAL records for a
// project that was never made.
func TestReview_ConcurrentCreatesOfOneProjectLeaveNoOrphans(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	projects, err := projectwal.NewProjectService(db, f.wal, f.branches)
	require.NoError(t, err)
	ctx := context.Background()
	name := fmt.Sprintf("review-project-%d", time.Now().UnixNano())

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = projects.CreateProject(name)
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	for i, err := range errs {
		if err == nil {
			created++
			continue
		}
		assert.NotContains(t, err.Error(), "E11000", "caller %d: a losing create names the project, not the index", i)
	}
	require.Equal(t, 1, created, "exactly one create succeeds")
	project, err := projects.GetProjectByName(name)
	require.NoError(t, err)

	orphanBranches, err := db.Collection("wal_branches").CountDocuments(ctx, bson.M{"project_id": bson.M{"$ne": project.ID}})
	require.NoError(t, err)
	assert.Zero(t, orphanBranches, "main branches of projects that were never made")
	orphanEntries, err := db.Collection("wal_log").CountDocuments(ctx, bson.M{"project_id": bson.M{"$ne": project.ID}})
	require.NoError(t, err)
	assert.Zero(t, orphanEntries, "WAL records of projects that were never made")
}
