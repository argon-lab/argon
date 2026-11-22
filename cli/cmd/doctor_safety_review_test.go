package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FINDING 1 (important, this change). A user with no rights on argon_doctor_*
// (readWrite on one database only: the usual narrowly scoped application user)
// is refused the probe's first command, so the probe database never exists.
// The new fallback still runs, is refused listCollections, and the error now
// tells the operator to drop by hand a database that was never created.
func TestReviewProbeDoesNotAskToDropADatabaseItNeverCreated(t *testing.T) {
	admin, uri := reviewAdmin(t)
	r := reviewRunProbe(t, admin, uri, bson.A{"@meta:readWrite"}, nil)
	if r.err == nil || !strings.Contains(r.err.Error(), "create collection with exact images") {
		t.Fatalf("setup: the probe should be refused at create, got %v", r.err)
	}
	if r.everCreated || len(r.leftCollections) != 0 {
		t.Fatalf("setup: the probe database should never have existed (created=%v, collections=%v)", r.everCreated, r.leftCollections)
	}
	exists, err := admin.ListDatabaseNames(context.Background(), bson.D{{Key: "name", Value: r.database}})
	if err != nil {
		t.Fatal(err)
	}
	if len(exists) != 0 {
		t.Fatalf("setup: %s exists", r.database)
	}
	if strings.Contains(r.err.Error(), "drop it by hand") || strings.Contains(r.err.Error(), "could not be emptied") {
		t.Fatalf("the probe created nothing (commands sent to %s: %v) and no such database exists, yet the error asks for it to be dropped by hand:\n%s",
			r.database, r.commands, reviewShorten(r.err.Error()))
	}
}

// FINDING 2 (pre-existing, made visible by the new message). When the user may
// drop neither databases nor collections, the probe leaves its metadata
// collection doctor_<id> behind too, and the error, which now lists what to
// remove by hand, names only the probe database.
func TestReviewProbeNamesEverythingItLeavesBehind(t *testing.T) {
	admin, uri := reviewAdmin(t)
	r := reviewRunProbe(t, admin, uri, bson.A{}, reviewReadWriteNoDrop)
	if r.err == nil || !strings.Contains(r.err.Error(), "drop it by hand") {
		t.Fatalf("setup: both drops should be refused, got %v", r.err)
	}
	if len(r.leftCollections) == 0 {
		t.Fatalf("setup: the probe collection should have stayed")
	}
	for _, name := range r.leftMetadata {
		// doctor_<id> is a substring of argon_doctor_<id>, so look for the
		// metadata database's name or any mention of the metadata collection.
		if !strings.Contains(r.err.Error(), r.metadata) && !strings.Contains(r.err.Error(), "metadata") {
			t.Fatalf("the probe left %s.%s behind (and %s.%v), but the error names only the database:\n%s",
				r.metadata, name, r.database, r.leftCollections, reviewShorten(r.err.Error()))
		}
	}
}

// FINDING 3 (nit, test only). The new test's deferred cleanup drops every
// database named argon_doctor_<24 hex> on the deployment as root, not only the
// one its own probe made: a doctor run (or another test) in flight on the same
// deployment loses its probe database mid-run.
func TestReviewCleanupTestLeavesOtherRunsAlone(t *testing.T) {
	admin, _ := reviewAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Stands in for another doctor run's probe database, still in use.
	other := admin.Database("argon_doctor_" + primitive.NewObjectID().Hex())
	if _, err := other.Collection("probe").InsertOne(ctx, bson.M{"_id": "probe"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Drop(context.Background()) }()

	if !t.Run("the test under review", TestDoctorProbeCleansUpWhenDropDatabaseIsRefused) {
		t.Log("the test under review failed (it compares counts of probe databases, so another run on the deployment can do that)")
	}

	names, err := admin.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: other.Name()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatalf("TestDoctorProbeCleansUpWhenDropDatabaseIsRefused dropped %s, a probe database it did not create", other.Name())
	}
}

func reviewShorten(s string) string {
	// The server's refusals carry the whole command; keep the report readable.
	for {
		i := strings.Index(s, ", lsid: {")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "$db:")
		if j < 0 {
			return s
		}
		s = s[:i] + ", ... " + s[i+j:]
	}
}

// Not a finding (passes): the name guard refuses a database that is not a
// probe database and drops nothing in it, for root as well.
func TestReviewGuardRefusesOtherDatabases(t *testing.T) {
	admin, _ := reviewAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := primitive.NewObjectID().Hex()
	for _, name := range []string{"review_app_" + id, "argon_doctorx_" + id, "Argon_doctor_" + id, "x_argon_doctor_" + id} {
		db := admin.Database(name)
		if _, err := db.Collection("keep").InsertOne(ctx, bson.M{"_id": 1}); err != nil {
			t.Fatal(err)
		}
		err := dropProbeCollections(ctx, db)
		names, listErr := db.ListCollectionNames(ctx, bson.D{})
		_ = db.Drop(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if err == nil || len(names) != 1 {
			t.Errorf("%s: err=%v, collections after=%v", name, err, names)
		}
	}
}
