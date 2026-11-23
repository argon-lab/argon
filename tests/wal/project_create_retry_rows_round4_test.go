package wal_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// Round 4 (0e6aae9, CreateProject's insert retry). The rows of the retry loop
// that no test of the commit reaches. They are here as coverage: on 0e6aae9
// all three behave as the code says, and none is a finding.
//
// round4ChurnedCreate runs CreateProject(name) on a client of its own. Every
// time that client sends its project row, onInsert runs first with the
// number of the attempt (1 is the first insert); every time it then reads
// wal_projects to see what holds the name, onRead runs first.
func round4ChurnedCreate(t *testing.T, db *mongo.Database, name string, onInsert, onRead func(attempt int)) (*wal.Project, int, error) {
	t.Helper()
	var inserts atomic.Int32
	creator, _ := round2Services(t, db.Name(), func(s round2Step) {
		switch {
		case s == round2BeforePublish:
			onInsert(int(inserts.Add(1)))
		case inserts.Load() > 0 && s == round2Step{"find", "wal_projects"}:
			onRead(int(inserts.Load()))
		}
	})
	project, err := creator.CreateProject(name)
	return project, int(inserts.Load()), err
}

func round4Hold(t *testing.T, db *mongo.Database, name string, importing bool) {
	t.Helper()
	row := bson.M{"_id": fmt.Sprintf("round4-holder-%d", time.Now().UnixNano()), "name": name, "use_wal": true}
	if importing {
		row["importing"] = true
	}
	_, err := db.Collection("wal_projects").InsertOne(context.Background(), row)
	require.NoError(t, err)
}

func round4Free(t *testing.T, db *mongo.Database, name string) {
	t.Helper()
	_, err := db.Collection("wal_projects").DeleteMany(context.Background(), bson.M{"name": name})
	require.NoError(t, err)
}

// The bound: the name is held at every insert and free at every read. The
// create gives up after three inserts, does not say "project already
// exists", and leaves nothing.
func TestRound4_CreateGivesUpAfterThreeInsertsOfANameThatKeepsComingBack(t *testing.T) {
	db := setupTestDB(t)
	name := fmt.Sprintf("round4-churn-%d", time.Now().UnixNano())

	_, inserts, err := round4ChurnedCreate(t, db, name,
		func(int) { round4Hold(t, db, name, true) },
		func(int) { round4Free(t, db, name) })
	require.Error(t, err)
	t.Logf("after %d inserts: %v", inserts, err)
	assert.Equal(t, 3, inserts, "the first insert and two more")
	assert.NotErrorIs(t, err, wal.ErrProjectExists)
	assert.Contains(t, err.Error(), "try again")
	round3RequireEmpty(t, db)
}

// The name is free at the first read and taken again, by a project, when the
// row is inserted the second time. The answer is "project already exists",
// the project that holds the name is untouched, and the loser leaves nothing.
func TestRound4_CreateWhoseRetryLosesToAProjectSaysItExists(t *testing.T) {
	db := setupTestDB(t)
	name := fmt.Sprintf("round4-retaken-%d", time.Now().UnixNano())

	_, inserts, err := round4ChurnedCreate(t, db, name,
		func(attempt int) { round4Hold(t, db, name, attempt == 1) },
		func(attempt int) {
			if attempt == 1 {
				round4Free(t, db, name)
			}
		})
	t.Logf("after %d inserts: %v", inserts, err)
	require.ErrorIs(t, err, wal.ErrProjectExists)
	assert.Equal(t, 2, inserts)
	assert.EqualValues(t, 1, round3Rows(t, db, "wal_projects", bson.M{"name": name}), "the project that holds the name")
	for _, collection := range []string{"wal_branches", "wal_log", "wal_counters"} {
		assert.Zero(t, round3Rows(t, db, collection, bson.M{}), "%s: the loser's rows", collection)
	}
}

// The name is free at the first read and the second insert fails for a
// reason of its own. The error is that reason, and nothing is left.
func TestRound4_CreateWhoseRetryFailsForAnotherReasonLeavesNothing(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	name := fmt.Sprintf("round4-retry-fails-%d", time.Now().UnixNano())

	_, inserts, err := round4ChurnedCreate(t, db, name,
		func(attempt int) {
			if attempt == 1 {
				round4Hold(t, db, name, true)
			}
		},
		func(attempt int) {
			if attempt == 1 {
				round4Free(t, db, name)
				require.NoError(t, db.RunCommand(ctx, bson.D{
					{Key: "collMod", Value: "wal_projects"},
					{Key: "validator", Value: bson.M{"name": bson.M{"$ne": name}}},
				}).Err())
			}
		})
	require.Error(t, err)
	t.Logf("after %d inserts: %v", inserts, err)
	assert.Equal(t, 2, inserts)
	assert.NotErrorIs(t, err, wal.ErrProjectExists)
	assert.Contains(t, err.Error(), "failed to create project")
	round3RequireEmpty(t, db)
}
