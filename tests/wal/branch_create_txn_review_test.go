package wal_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	branchwal "github.com/argon-lab/argon/v2/internal/branch/wal"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// Review tests for the transaction in insertBranch and its error handling.

func reviewCounter(t *testing.T, db *mongo.Database, project string) int64 {
	t.Helper()
	var counter struct {
		Next int64 `bson:"next"`
	}
	require.NoError(t, db.Collection("wal_counters").FindOne(context.Background(), bson.M{"_id": project}).Decode(&counter))
	return counter.Next
}

// A duplicate key that is not the name index's must not be answered with
// "branch already exists". Here the project's LSN counter is behind its WAL
// (a metadata database restored from a dump whose wal_counters was copied
// before wal_log, for one), so the create_branch record collides on
// (project_id, lsn). No branch has the name.
func TestReview_WALDuplicateKeyIsNotReportedAsBranchExists(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "review-wal-dup"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	for _, name := range []string{"a", "b", "c", "d"} {
		_, err := f.branches.CreateBranch(project, name, main.ID)
		require.NoError(t, err)
	}
	// Put the counter three behind: the next three reservations name LSNs
	// that the WAL already holds.
	_, err = db.Collection("wal_counters").UpdateOne(context.Background(),
		bson.M{"_id": project}, bson.M{"$inc": bson.M{"next": -3}})
	require.NoError(t, err)

	_, err = f.branches.CreateBranch(project, "never-used", main.ID)
	require.Error(t, err)
	t.Logf("CreateBranch error: %v", err)
	_, lookupErr := f.branches.GetBranch(project, "never-used")
	require.Error(t, lookupErr, "no branch has the name")
	assert.False(t, errors.Is(err, branchwal.ErrBranchExists),
		"a WAL sequence collision is reported as %q for a name no branch has", err)
}

// The same for the branch's _id: CreateBranchWithData takes the caller's ID.
func TestReview_IDDuplicateKeyIsNotReportedAsBranchExists(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "review-id-dup"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	err = f.branches.CreateBranchWithData(&wal.Branch{
		ID: main.ID, ProjectID: project, Name: "never-used", HeadLSN: main.HeadLSN, BaseLSN: main.HeadLSN,
	})
	require.Error(t, err)
	t.Logf("CreateBranchWithData error: %v", err)
	assert.False(t, errors.Is(err, branchwal.ErrBranchExists),
		"an _id collision is reported as %q for a name no branch has", err)
}

// The WAL append's error is wrapped twice with fmt.Errorf before the driver
// sees it. A transient one (here a write conflict on the record's LSN, held
// by another transaction) must still make the driver run the callback
// again, and the branch that results must describe the attempt that
// committed, not the one that was rolled back.
func TestReview_TransientWALConflictIsRetriedAndStateIsFromTheCommittedAttempt(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	const project = "review-transient"
	// A first record so the counter exists.
	_, err := f.wal.Append(&wal.Entry{ProjectID: project, Operation: wal.OpCreateProject, Metadata: map[string]interface{}{"project_name": project}})
	require.NoError(t, err)
	next := reviewCounter(t, db, project) + 1

	session, err := db.Client().StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	require.NoError(t, session.StartTransaction())
	require.NoError(t, mongo.WithSession(ctx, session, func(sc mongo.SessionContext) error {
		_, err := db.Collection("wal_log").InsertOne(sc, bson.M{"project_id": project, "lsn": next, "operation": "create_project"})
		return err
	}))

	branch, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, session.AbortTransaction(ctx))
	require.NoError(t, err, "a transient conflict on the WAL append is retried")

	assert.Greater(t, branch.CreatedLSN, next, "the first attempt's LSN was rolled back")
	assert.Equal(t, branch.CreatedLSN, branch.HeadLSN, "a parentless branch starts at its own record")
	stored, err := f.branches.GetBranch(project, "main")
	require.NoError(t, err)
	assert.Equal(t, branch.ID, stored.ID)
	assert.Equal(t, branch.CreatedLSN, stored.CreatedLSN)
	assert.Equal(t, branch.HeadLSN, stored.HeadLSN)
	assert.EqualValues(t, 1, createBranchEntries(t, db, project, "main"))
	entry, err := f.wal.GetEntry(project, stored.CreatedLSN)
	require.NoError(t, err)
	assert.Equal(t, stored.ID, entry.BranchID)
	assert.Equal(t, wal.OpCreateBranch, entry.Operation)
}

// A create whose name is held by another, uncommitted transaction runs its
// callback again and again until that transaction ends. Whatever the
// number of attempts, one record and one branch result, and they agree.
func TestReview_CreateBehindAHeldNameSettlesOnOneRecord(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	const project = "review-held"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		commit bool
	}{{"holder-aborts", false}, {"holder-commits", true}} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := db.Client().StartSession()
			require.NoError(t, err)
			defer session.EndSession(ctx)
			require.NoError(t, session.StartTransaction())
			require.NoError(t, mongo.WithSession(ctx, session, func(sc mongo.SessionContext) error {
				_, err := db.Collection("wal_branches").InsertOne(sc, bson.M{
					"_id": "holder-" + tc.name, "project_id": project, "name": tc.name, "is_deleted": false})
				return err
			}))
			before := reviewCounter(t, db, project)
			done := make(chan struct{})
			var branch *wal.Branch
			var createErr error
			go func() {
				defer close(done)
				branch, createErr = f.branches.CreateBranch(project, tc.name, main.ID)
			}()
			time.Sleep(400 * time.Millisecond)
			select {
			case <-done:
				t.Fatalf("the create returned while the name was still undecided: %v", createErr)
			default:
			}
			if tc.commit {
				require.NoError(t, session.CommitTransaction(ctx))
			} else {
				require.NoError(t, session.AbortTransaction(ctx))
			}
			<-done
			burned := reviewCounter(t, db, project) - before
			t.Logf("%s: %d LSNs reserved by one create in 400ms behind a held name", tc.name, burned)
			if tc.commit {
				assert.ErrorIs(t, createErr, branchwal.ErrBranchExists)
				assert.EqualValues(t, 0, createBranchEntries(t, db, project, tc.name))
				return
			}
			require.NoError(t, createErr)
			assert.EqualValues(t, 1, createBranchEntries(t, db, project, tc.name))
			stored, err := f.branches.GetBranch(project, tc.name)
			require.NoError(t, err)
			assert.Equal(t, branch.ID, stored.ID)
			assert.Equal(t, branch.CreatedLSN, stored.CreatedLSN)
			entry, err := f.wal.GetEntry(project, stored.CreatedLSN)
			require.NoError(t, err)
			assert.Equal(t, stored.ID, entry.BranchID)
		})
	}
}

// The same for CreateBranchWithData, whose callback mutates the caller's
// branch on every attempt.
func TestReview_CreateWithDataBehindAHeldNameKeepsTheCallersBranch(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	ctx := context.Background()
	const project = "review-held-data"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	session, err := db.Client().StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	require.NoError(t, session.StartTransaction())
	require.NoError(t, mongo.WithSession(ctx, session, func(sc mongo.SessionContext) error {
		_, err := db.Collection("wal_branches").InsertOne(sc, bson.M{
			"_id": "holder", "project_id": project, "name": "restored", "is_deleted": false})
		return err
	}))
	mine := &wal.Branch{ID: primitive.NewObjectID().Hex(), ProjectID: project, Name: "restored",
		ParentID: main.ID, HeadLSN: main.HeadLSN, BaseLSN: main.HeadLSN}
	done := make(chan error, 1)
	go func() { done <- f.branches.CreateBranchWithData(mine) }()
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, session.AbortTransaction(ctx))
	require.NoError(t, <-done)

	stored, err := f.branches.GetBranch(project, "restored")
	require.NoError(t, err)
	assert.Equal(t, mine.ID, stored.ID)
	assert.Equal(t, main.HeadLSN, stored.HeadLSN)
	assert.Equal(t, main.HeadLSN, stored.BaseLSN)
	assert.WithinDuration(t, mine.CreatedAt, stored.CreatedAt, time.Millisecond)
	assert.EqualValues(t, 1, createBranchEntries(t, db, project, "restored"))
}

// The retry-once path under concurrency: an older release put the legacy
// index back, a deleted branch holds the name, and several creates of that
// name arrive together. Each runs into the legacy index, retires it and
// retries; one must win and the others must be told the branch exists.
func TestReview_ConcurrentCreatesThroughTheLegacyIndexRetry(t *testing.T) {
	// A database per round: once a name has been reused, the legacy index
	// can no longer be built there.
	for round := 0; round < 6; round++ {
		db := setupTestDB(t)
		f := newSnapshotFixture(t, db)
		const project = "review-legacy-concurrent"
		main, err := f.branches.CreateBranch(project, "main", "")
		require.NoError(t, err)
		name := "run-" + string(rune('a'+round))
		_, err = f.branches.CreateBranch(project, name, main.ID)
		require.NoError(t, err)
		require.NoError(t, f.branches.DeleteBranch(project, name))
		legacyBranchIndex(t, db) // the older release starts

		const callers = 8
		var wg sync.WaitGroup
		errs := make([]error, callers)
		start := make(chan struct{})
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.branches.CreateBranch(project, name, main.ID)
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
			assert.Truef(t, errors.Is(err, branchwal.ErrBranchExists), "%s caller %d: %v", name, i, err)
		}
		assert.Equal(t, 1, created, "%s: exactly one create succeeds", name)
		assert.EqualValues(t, 2, createBranchEntries(t, db, project, name), "%s: one record per branch that was made", name)
		assert.NotContains(t, branchIndexNames(t, db), "project_id_1_name_1")
	}
}

// How many LSNs do creates that arrive together reserve? Informational.
func TestReview_LSNsReservedByConcurrentCreatesOfOneName(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "review-burn"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)
	before := reviewCounter(t, db, project)
	const callers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = f.branches.CreateBranch(project, "together", main.ID)
		}()
	}
	close(start)
	wg.Wait()
	t.Logf("%d concurrent creates of one name reserved %d LSNs for 1 record", callers, reviewCounter(t, db, project)-before)
}

// A branch made by restore (CreateBranchWithData) should be findable from
// its create record like any other: CreatedLSN names the record, and the
// record names the branch by ID.
func TestReview_CreateWithDataRecordsItsCreation(t *testing.T) {
	db := setupTestDB(t)
	f := newSnapshotFixture(t, db)
	const project = "review-created-lsn"
	main, err := f.branches.CreateBranch(project, "main", "")
	require.NoError(t, err)

	branch, err := f.restore.CreateBranchAtLSN(project, main.ID, "at-lsn", main.HeadLSN)
	require.NoError(t, err)
	stored, err := f.branches.GetBranch(project, "at-lsn")
	require.NoError(t, err)
	var record struct {
		LSN      int64  `bson:"lsn"`
		BranchID string `bson:"branch_id"`
	}
	require.NoError(t, db.Collection("wal_log").FindOne(context.Background(), bson.M{
		"project_id": project, "operation": wal.OpCreateBranch, "metadata.branch_name": "at-lsn"}).Decode(&record))
	assert.Equal(t, record.LSN, stored.CreatedLSN, "CreatedLSN names the create_branch record")
	assert.Equal(t, branch.ID, record.BranchID, "the record names the branch by ID")
}
