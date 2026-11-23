package wal_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// CreateProject appends its record, creates main and inserts the project row
// last. These tests hold a create inside that window, or end its process
// there, without changing any non-test code: a driver command monitor sees
// every command just before it is sent.

// round2Step names the command CreateProject is about to send.
type round2Step struct{ command, collection string }

var (
	round2BeforeRecord  = round2Step{"findAndModify", "wal_counters"} // nothing made yet
	round2BeforeMain    = round2Step{"insert", "wal_branches"}        // + create_project record
	round2BeforePublish = round2Step{"insert", "wal_projects"}        // + main branch; the row is next
)

// round2Services opens the metadata database on a client of its own whose
// commands pass through before, and returns the three services on it.
func round2Services(t *testing.T, dbName string, before func(round2Step)) (*projectwal.ProjectService, *mongo.Database) {
	t.Helper()
	opts := options.Client().ApplyURI(getTestMongoURI())
	if before != nil {
		opts.SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if e.DatabaseName != dbName {
				return
			}
			if name, ok := e.Command.Lookup(e.CommandName).StringValueOK(); ok {
				before(round2Step{e.CommandName, name})
			}
		}})
	}
	client, err := mongo.Connect(context.Background(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	db := client.Database(dbName)
	log, err := wal.NewService(db)
	require.NoError(t, err)
	branches, err := branchwal.NewBranchService(db, log)
	require.NoError(t, err)
	projects, err := projectwal.NewProjectService(db, log, branches)
	require.NoError(t, err)
	return projects, db
}

// round2HeldCreate starts CreateProject(name) and holds it just before step.
// release lets it go on and returns its result.
func round2HeldCreate(t *testing.T, dbName, name string, step round2Step) (release func() (*wal.Project, error)) {
	t.Helper()
	reached, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	creator, _ := round2Services(t, dbName, func(s round2Step) {
		if armed.Load() && s == step {
			once.Do(func() { close(reached); <-resume })
		}
	})
	type result struct {
		project *wal.Project
		err     error
	}
	done := make(chan result, 1)
	armed.Store(true)
	go func() {
		project, err := creator.CreateProject(name)
		done <- result{project, err}
	}()
	select {
	case <-reached:
	case r := <-done:
		t.Fatalf("the create finished before reaching %v: %v", step, r.err)
	case <-time.After(30 * time.Second):
		t.Fatalf("the create never reached %v", step)
	}
	released := false
	release = func() (*wal.Project, error) {
		if !released {
			released = true
			close(resume)
		}
		select {
		case r := <-done:
			done <- r
			return r.project, r.err
		case <-time.After(60 * time.Second):
			t.Fatal("the held create did not finish")
			return nil, nil
		}
	}
	t.Cleanup(func() { _, _ = release() })
	return release
}

// The same, with nothing held: creates of one name that arrive together.
// Each loser reads the project the moment it is told the project exists.
func TestRound2_ConcurrentCreateLosersCanReadTheProject(t *testing.T) {
	db := setupTestDB(t)
	projects, _ := round2Services(t, db.Name(), nil)
	name := fmt.Sprintf("round2-losers-%d", time.Now().UnixNano())

	const callers = 8
	var wg sync.WaitGroup
	createErrs := make([]error, callers)
	readErrs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, createErrs[i] = projects.CreateProject(name); errors.Is(createErrs[i], wal.ErrProjectExists) {
				_, readErrs[i] = projects.GetProjectByName(name)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	unreadable := 0
	for i := range createErrs {
		if readErrs[i] != nil {
			unreadable++
			t.Logf("caller %d: create said %q, then GetProjectByName said %q", i, createErrs[i], readErrs[i])
		}
	}
	assert.Zero(t, unreadable, "callers told the project exists that could not read it")
}

// A create held before its project row is invisible, and does not hold the
// name: another create of the name goes through. The held one then loses,
// answers "project already exists" for a project that can be read, and
// removes the record and main branch it had made.
func TestRound2_HeldCreateLosesCleanlyToACreateThatFinishes(t *testing.T) {
	for _, step := range []round2Step{round2BeforeMain, round2BeforePublish} {
		t.Run(step.command+" "+step.collection, func(t *testing.T) {
			db := setupTestDB(t)
			observer, _ := round2Services(t, db.Name(), nil)
			ctx := context.Background()
			name := fmt.Sprintf("round2-held-%d", time.Now().UnixNano())

			release := round2HeldCreate(t, db.Name(), name, step)
			_, err := observer.GetProjectByName(name)
			require.Error(t, err, "a project being created cannot be read")

			winner, err := observer.CreateProject(name)
			require.NoError(t, err, "the held create does not reserve the name")

			_, err = release()
			require.ErrorIs(t, err, wal.ErrProjectExists)
			readable, err := observer.GetProjectByName(name)
			require.NoError(t, err, "the answer is for a project that can be read")
			assert.Equal(t, winner.ID, readable.ID)

			for _, collection := range []string{"wal_branches", "wal_log"} {
				n, err := db.Collection(collection).CountDocuments(ctx, bson.M{"project_id": bson.M{"$ne": winner.ID}})
				require.NoError(t, err)
				assert.Zero(t, n, "%s holds rows of the project that was never made", collection)
			}
			n, err := db.Collection("wal_counters").CountDocuments(ctx, bson.M{"_id": bson.M{"$ne": winner.ID}})
			require.NoError(t, err)
			assert.Zero(t, n, "wal_counters holds the counter of the project that was never made")
		})
	}
}

// A create whose process dies leaves no project and does not hold the name:
// the next create of it succeeds. What the dead process had made (a record,
// and a main branch if it got that far) stays under a project ID nothing
// refers to, as it did before.
func TestRound2_CreateAfterAKilledCreateOfTheSameName(t *testing.T) {
	for _, kill := range []struct {
		label string
		step  round2Step
	}{
		{"before the create record", round2BeforeRecord},
		{"before the main branch", round2BeforeMain},
		{"before the project row", round2BeforePublish},
	} {
		t.Run(kill.label, func(t *testing.T) {
			db := setupTestDB(t)
			projects, _ := round2Services(t, db.Name(), nil)
			name := fmt.Sprintf("round2-killed-%d", time.Now().UnixNano())

			cmd := exec.Command(os.Args[0], "-test.run=^TestRound2_HelperCreateProjectAndDie$")
			cmd.Env = append(os.Environ(),
				"ARGON_ROUND2_DIE_DB="+db.Name(), "ARGON_ROUND2_DIE_NAME="+name,
				"ARGON_ROUND2_DIE_COMMAND="+kill.step.command, "ARGON_ROUND2_DIE_COLLECTION="+kill.step.collection)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "the helper process dies: %s", output)
			require.Equal(t, 87, exit.ExitCode(), "the helper process dies at the chosen step: %s", output)

			_, err = projects.GetProjectByName(name)
			require.Error(t, err, "no project of the name can be read")
			listed, err := projects.ListProjects()
			require.NoError(t, err)
			require.Empty(t, listed, "no project is listed")

			created, err := projects.CreateProject(name)
			require.NoError(t, err, "the name is not held by the dead create")
			readable, err := projects.GetProjectByName(name)
			require.NoError(t, err)
			assert.Equal(t, created.ID, readable.ID)
		})
	}
}

// The process TestRound2_CreateAfterAKilledCreateOfTheSameName starts. It
// exits, running no deferred cleanup, just before the chosen command is sent.
func TestRound2_HelperCreateProjectAndDie(t *testing.T) {
	dbName := os.Getenv("ARGON_ROUND2_DIE_DB")
	if dbName == "" {
		t.Skip("helper process for TestRound2_CreateAfterAKilledCreateOfTheSameName")
	}
	step := round2Step{os.Getenv("ARGON_ROUND2_DIE_COMMAND"), os.Getenv("ARGON_ROUND2_DIE_COLLECTION")}
	var armed atomic.Bool
	projects, _ := round2Services(t, dbName, func(s round2Step) {
		if armed.Load() && s == step {
			os.Exit(87)
		}
	})
	armed.Store(true)
	_, err := projects.CreateProject(os.Getenv("ARGON_ROUND2_DIE_NAME"))
	t.Fatalf("the create was not interrupted: %v", err)
}
