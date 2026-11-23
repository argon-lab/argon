package wal_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Review probe: deletes and creates of one name running against each other.
// A create answers nil or ErrBranchExists, a delete nil or "branch not
// found"; the name never has two live branches; and every create_branch
// record in the WAL belongs to a branch that was made. This passes on the
// reviewed commit and is kept as the covering test for those rows of the
// case table.
func TestReview_DeletesAndCreatesOfOneNameStayConsistent(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	project := fmt.Sprintf("review-delete-create-%d", time.Now().UnixNano())
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	const workers, rounds = 6, 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	var unexpected []string
	made := map[string]bool{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if (w+i)%2 == 0 {
					branch, err := f.branches.CreateBranch(project, "run-1", main.ID)
					mu.Lock()
					if err == nil {
						made[branch.ID] = true
					} else if !errors.Is(err, branchwal.ErrBranchExists) {
						unexpected = append(unexpected, "create: "+err.Error())
					}
					mu.Unlock()
					continue
				}
				// Two deletes of one branch: the loser answers "branch not
				// found", or the driver's "no documents" from
				// RequireDeletable (unchanged by this commit).
				if err := f.branches.DeleteBranch(project, "run-1"); err != nil && err.Error() != "branch not found" && !errors.Is(err, mongo.ErrNoDocuments) {
					mu.Lock()
					unexpected = append(unexpected, "delete: "+err.Error())
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	assert.Empty(t, unexpected)

	live, err := db.Collection("wal_branches").CountDocuments(ctx, bson.M{"project_id": project, "name": "run-1", "is_deleted": false})
	require.NoError(t, err)
	assert.LessOrEqual(t, live, int64(1), "at most one live branch of the name")

	cursor, err := db.Collection("wal_log").Find(ctx, bson.M{"project_id": project, "operation": wal.OpCreateBranch, "metadata.branch_name": "run-1"})
	require.NoError(t, err)
	var records []struct {
		BranchID string `bson:"branch_id"`
	}
	require.NoError(t, cursor.All(ctx, &records))
	for _, record := range records {
		assert.True(t, made[record.BranchID], "create_branch record for %s, a branch no create returned", record.BranchID)
	}
	assert.Len(t, records, len(made), "one create_branch record per branch made")
}
