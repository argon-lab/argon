package wal_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	projectwal "github.com/argon-lab/argon/v2/internal/project/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Round 3: every point at which CreateProject can fail on 6aa0f7a, and what
// removeUnmade leaves behind.

func round3Rows(t *testing.T, db *mongo.Database, collection string, filter bson.M) int64 {
	t.Helper()
	n, err := db.Collection(collection).CountDocuments(context.Background(), filter)
	require.NoError(t, err)
	return n
}

func round3RequireEmpty(t *testing.T, db *mongo.Database) {
	t.Helper()
	for _, collection := range []string{"wal_projects", "wal_branches", "wal_log", "wal_counters"} {
		assert.Zero(t, round3Rows(t, db, collection, bson.M{}), "%s after a failed create", collection)
	}
}

// Row 1: the create_project record cannot be appended. The LSN counter was
// already upserted by the reservation.
func TestRound3_RecordAppendFailureLeavesNothing(t *testing.T) {
	db := setupTestDB(t)
	projects, _ := round2Services(t, db.Name(), nil)
	ctx := context.Background()

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "wal_log"},
		{Key: "validator", Value: bson.M{"operation": bson.M{"$ne": "create_project"}}},
	}).Err())
	_, err := projects.CreateProject("round3-record-fails")
	require.Error(t, err)
	t.Logf("create whose record cannot be appended: %v", err)
	assert.NotErrorIs(t, err, wal.ErrProjectExists)
	assert.NotContains(t, err.Error(), "could not be removed")
	round3RequireEmpty(t, db)

	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "wal_log"},
		{Key: "validator", Value: wal.FormatGuardValidator()},
	}).Err())
	_, err = projects.CreateProject("round3-record-fails")
	require.NoError(t, err, "the name is free")
}

// Row 2: the main branch cannot be made, either because its row is refused
// or because its create record is.
func TestRound3_MainBranchFailureLeavesNothing(t *testing.T) {
	for _, refuse := range []struct {
		label, collection string
		validator         bson.M
	}{
		{"branch row refused", "wal_branches", bson.M{"name": bson.M{"$ne": "main"}}},
		{"create_branch record refused", "wal_log", bson.M{"operation": bson.M{"$ne": "create_branch"}}},
	} {
		t.Run(refuse.label, func(t *testing.T) {
			db := setupTestDB(t)
			projects, _ := round2Services(t, db.Name(), nil)
			ctx := context.Background()

			require.NoError(t, db.RunCommand(ctx, bson.D{
				{Key: "collMod", Value: refuse.collection},
				{Key: "validator", Value: refuse.validator},
			}).Err())
			_, err := projects.CreateProject("round3-main-fails")
			require.Error(t, err)
			t.Logf("create whose main branch cannot be made: %v", err)
			assert.NotErrorIs(t, err, wal.ErrProjectExists)
			assert.NotContains(t, err.Error(), "could not be removed")
			round3RequireEmpty(t, db)

			require.NoError(t, db.RunCommand(ctx, bson.D{
				{Key: "collMod", Value: refuse.collection},
				{Key: "validator", Value: bson.M{}},
			}).Err())
			_, err = projects.CreateProject("round3-main-fails")
			require.NoError(t, err, "the name is free")
		})
	}
}

// Row 5: the create loses at the project row and its cleanup fails too. The
// cleanup is one transaction, made to fail at its last statement by putting
// a view where wal_counters is. The answer must still be "project exists",
// name the ID, and leave the winner alone; the transaction leaves the
// loser's rows whole, not half removed.
func TestRound3_CleanupFailureKeepsTheCauseAndTheWinner(t *testing.T) {
	db := setupTestDB(t)
	observer, _ := round2Services(t, db.Name(), nil)
	ctx := context.Background()
	name := fmt.Sprintf("round3-cleanup-fails-%d", time.Now().UnixNano())

	release := round2HeldCreate(t, db.Name(), name, round2BeforePublish)
	winner, err := observer.CreateProject(name)
	require.NoError(t, err)

	var loser wal.Branch
	require.NoError(t, db.Collection("wal_branches").FindOne(ctx, bson.M{"project_id": bson.M{"$ne": winner.ID}}).Decode(&loser))

	admin := db.Client().Database("admin")
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "renameCollection", Value: db.Name() + ".wal_counters"},
		{Key: "to", Value: db.Name() + ".wal_counters_real"},
	}).Err())
	require.NoError(t, db.CreateView(ctx, "wal_counters", "wal_counters_real", mongo.Pipeline{}))

	_, err = release()
	require.Error(t, err)
	t.Logf("create that lost and could not clean up: %v", err)
	assert.ErrorIs(t, err, wal.ErrProjectExists, "the cause survives a failed cleanup")
	assert.Contains(t, err.Error(), "could not be removed")
	assert.Contains(t, err.Error(), loser.ProjectID, "the error names the project ID whose rows were left")

	assert.EqualValues(t, 1, round3Rows(t, db, "wal_branches", bson.M{"project_id": loser.ProjectID}), "the failed cleanup removed nothing: main branch")
	assert.EqualValues(t, 2, round3Rows(t, db, "wal_log", bson.M{"project_id": loser.ProjectID}), "the failed cleanup removed nothing: records")
	assert.EqualValues(t, 1, round3Rows(t, db, "wal_branches", bson.M{"project_id": winner.ID, "is_deleted": false}), "the winner keeps its main branch")
	assert.EqualValues(t, 2, round3Rows(t, db, "wal_log", bson.M{"project_id": winner.ID}), "the winner keeps its records")
	readable, err := observer.GetProjectByName(name)
	require.NoError(t, err)
	assert.Equal(t, winner.ID, readable.ID)
}

// round3HeldAtNameTaken starts CreateProject(name) on a client of its own
// and holds it after its project row was refused as a duplicate, just before
// nameTaken reads the row that holds the name.
func round3HeldAtNameTaken(t *testing.T, dbName, name string) (release func() error) {
	t.Helper()
	reached, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var inserted atomic.Bool
	creator, _ := round2Services(t, dbName, func(s round2Step) {
		switch {
		case s == round2BeforePublish:
			inserted.Store(true)
		case inserted.Load() && s == round2Step{"find", "wal_projects"}:
			once.Do(func() { close(reached); <-resume })
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := creator.CreateProject(name)
		done <- err
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("the create finished before it read the holder of the name: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the create never read the holder of the name")
	}
	released := false
	release = func() error {
		if !released {
			released = true
			close(resume)
		}
		select {
		case err := <-done:
			done <- err
			return err
		case <-time.After(60 * time.Second):
			t.Fatal("the held create did not finish")
			return nil
		}
	}
	t.Cleanup(func() { _ = release() })
	return release
}

// Row 4c: the row that refused the create is gone by the time nameTaken
// reads it. Here it was an import's reservation, and the import was aborted.
// No project of the name ever existed, so "project already exists" is the
// answer round 2 objected to: one for a project nothing can read.
func TestRound3_CreateLosesToAnImportThatIsThenAborted(t *testing.T) {
	_, projects, meta, _ := importRecoveryFixture(t)
	ctx := context.Background()
	name := fmt.Sprintf("round3-vanished-%d", time.Now().UnixNano())

	staged, err := projects.BeginImport(ctx, name)
	require.NoError(t, err)
	release := round3HeldAtNameTaken(t, meta.Name(), name)
	require.NoError(t, projects.AbortImport(ctx, staged.ID))

	// The name is free again by the time the create looks at what held it,
	// so it inserts its project row again and succeeds. It never answers
	// "project already exists" for a name nothing has.
	err = release()
	require.NoError(t, err, "the name is free once the import is aborted")
	created, err := projects.GetProjectByName(name)
	require.NoError(t, err)
	assert.EqualValues(t, 1, round3Rows(t, meta, "wal_branches", bson.M{}), "only the created project's main branch")
	assert.EqualValues(t, 1, round3Rows(t, meta, "wal_branches", bson.M{"project_id": created.ID}))
	assert.EqualValues(t, 1, round3Rows(t, meta, "wal_counters", bson.M{}))
}

// Row 4b, the other order: the import publishes between the refused insert
// and nameTaken's read. "Project already exists" is then true.
func TestRound3_CreateLosesToAnImportThatThenCompletes(t *testing.T) {
	_, projects, meta, _ := importRecoveryFixture(t)
	ctx := context.Background()
	name := fmt.Sprintf("round3-completed-%d", time.Now().UnixNano())

	staged, err := projects.BeginImport(ctx, name)
	require.NoError(t, err)
	release := round3HeldAtNameTaken(t, meta.Name(), name)
	require.NoError(t, projects.CompleteImport(ctx, staged.ID))

	err = release()
	require.ErrorIs(t, err, wal.ErrProjectExists)
	readable, err := projects.GetProjectByName(name)
	require.NoError(t, err)
	assert.Equal(t, staged.ID, readable.ID)
	assert.EqualValues(t, 1, round3Rows(t, meta, "wal_branches", bson.M{}), "only the import's main branch remains")
	assert.Zero(t, round3Rows(t, meta, "wal_branches", bson.M{"project_id": bson.M{"$ne": staged.ID}}))
	assert.Zero(t, round3Rows(t, meta, "wal_log", bson.M{"project_id": bson.M{"$ne": staged.ID}}))
	assert.Zero(t, round3Rows(t, meta, "wal_counters", bson.M{"_id": bson.M{"$ne": staged.ID}}))
}

// Creates of one name that arrive together with an import that reserves the
// name and gives it up, over and over, and in some rounds publishes at the
// end. Whatever each caller is told, at the end every branch, record and
// counter belongs to a project row, and every project row has its live main
// branch. Nothing is held here; the callers told "project already exists"
// with no project to read are counted and logged, not failed on: the held
// test above is the deterministic form of that.
func TestRound3_CreatesAndImportReservationsLeaveNoStrayRows(t *testing.T) {
	_, projects, meta, _ := importRecoveryFixture(t)
	ctx := context.Background()

	const rounds, creators = 24, 5
	tally := map[string]int{}
	for round := 0; round < rounds; round++ {
		name := fmt.Sprintf("round3-mixed-%d-%d", time.Now().UnixNano(), round)
		start := make(chan struct{})
		var wg sync.WaitGroup
		createErrs := make([]error, creators)
		readErrs := make([]error, creators)
		for i := 0; i < creators; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if _, createErrs[i] = projects.CreateProject(name); errors.Is(createErrs[i], wal.ErrProjectExists) {
					_, readErrs[i] = projects.GetProjectByName(name)
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Some rounds let the creates get ahead of the first reservation.
			time.Sleep(time.Duration(round%4) * 2 * time.Millisecond)
			for i := 0; i < 4; i++ {
				staged, err := projects.BeginImport(ctx, name)
				if err != nil {
					continue
				}
				if round%3 == 2 && i == 3 {
					if err := projects.CompleteImport(ctx, staged.ID); err != nil {
						t.Errorf("publish of the import's own reservation: %v", err)
					}
					continue
				}
				if err := projects.AbortImport(ctx, staged.ID); err != nil {
					t.Errorf("abort of the import's own reservation: %v", err)
				}
			}
		}()
		close(start)
		wg.Wait()

		made := 0
		for i, err := range createErrs {
			switch {
			case err == nil:
				made++
				tally["created"]++
			case errors.Is(err, wal.ErrProjectExists) && !strings.Contains(err.Error(), "could not be removed"):
				tally["exists"]++
				if readErrs[i] != nil {
					tally["exists, with no project to read"]++
				}
			case strings.Contains(err.Error(), "import cleanup") && !strings.Contains(err.Error(), "could not be removed"):
				tally["reserved by an import"]++
			default:
				t.Errorf("round %d caller %d: %v", round, i, err)
			}
		}
		assert.LessOrEqual(t, made, 1, "round %d: creates that succeeded", round)
		assert.LessOrEqual(t, round3Rows(t, meta, "wal_projects", bson.M{"name": name}), int64(1), "round %d: project rows of the name", round)
	}
	t.Logf("what the creates were told: %v", tally)

	ids, err := meta.Collection("wal_projects").Distinct(ctx, "_id", bson.M{})
	require.NoError(t, err)
	t.Logf("projects at the end: %d", len(ids))
	assert.Zero(t, round3Rows(t, meta, "wal_projects", bson.M{"importing": true}), "every reservation was given up or published")
	assert.Zero(t, round3Rows(t, meta, "wal_branches", bson.M{"project_id": bson.M{"$nin": ids}}), "branches of no project")
	assert.Zero(t, round3Rows(t, meta, "wal_log", bson.M{"project_id": bson.M{"$nin": ids}}), "records of no project")
	assert.Zero(t, round3Rows(t, meta, "wal_counters", bson.M{"_id": bson.M{"$nin": ids}}), "counters of no project")
	for _, id := range ids {
		assert.EqualValues(t, 1, round3Rows(t, meta, "wal_branches", bson.M{"project_id": id, "name": "main", "is_deleted": false}), "project %v has its main branch", id)
		var project wal.Project
		require.NoError(t, meta.Collection("wal_projects").FindOne(ctx, bson.M{"_id": id}).Decode(&project))
		assert.EqualValues(t, 1, round3Rows(t, meta, "wal_branches", bson.M{"_id": project.MainBranchID, "project_id": id, "is_deleted": false}), "project %v points at its own main branch", id)
	}
}

// Row 5b: removeUnmade is asked to clean up an ID that has a project row.
// The only way there is an insert that was applied and still reported as
// failed. It is staged here by inserting the very row the create is about
// to send, a moment before it does, so the create's own insert comes back as
// a duplicate. The cleanup must refuse, and the project must keep its main
// branch, records and counter.
func TestRound3_CleanupRefusesAnIDThatHasAProjectRow(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	name := fmt.Sprintf("round3-applied-%d", time.Now().UnixNano())

	var once sync.Once
	var staged atomic.Value
	opts := options.Client().ApplyURI(getTestMongoURI())
	opts.SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.DatabaseName != db.Name() || e.CommandName != "insert" {
			return
		}
		if target, _ := e.Command.Lookup("insert").StringValueOK(); target != "wal_projects" {
			return
		}
		once.Do(func() {
			row, err := e.Command.LookupErr("documents", "0")
			if err != nil {
				staged.Store(err)
				return
			}
			var project wal.Project
			if err := row.Unmarshal(&project); err != nil {
				staged.Store(err)
				return
			}
			if _, err := db.Collection("wal_projects").InsertOne(context.Background(), &project); err != nil {
				staged.Store(err)
				return
			}
			staged.Store(project.ID)
		})
	}})
	client, err := mongo.Connect(ctx, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	cdb := client.Database(db.Name())
	log, err := wal.NewService(cdb)
	require.NoError(t, err)
	branches, err := branchwal.NewBranchService(cdb, log)
	require.NoError(t, err)
	creator, err := projectwal.NewProjectService(cdb, log, branches)
	require.NoError(t, err)

	_, err = creator.CreateProject(name)
	require.Error(t, err)
	t.Logf("create whose row was already in place: %v", err)
	id, ok := staged.Load().(string)
	require.True(t, ok, "the row was staged: %v", staged.Load())
	assert.Contains(t, err.Error(), "refusing cleanup")

	stored, err := creator.GetProjectByName(name)
	require.NoError(t, err)
	require.Equal(t, id, stored.ID)
	main, err := branches.GetBranchByID(stored.MainBranchID)
	require.NoError(t, err, "the project that exists keeps its main branch")
	assert.Equal(t, id, main.ProjectID)
	assert.EqualValues(t, 2, round3Rows(t, db, "wal_log", bson.M{"project_id": id}), "and its records")
	assert.EqualValues(t, 1, round3Rows(t, db, "wal_counters", bson.M{"_id": id}), "and its counter")
}
