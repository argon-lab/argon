package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func doctorCheck(report deploymentReport, name string) (deploymentCheck, bool) {
	for _, check := range report.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return deploymentCheck{}, false
}

func TestDoctorChecksCaptureBoundary(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the doctor probe against MongoDB")
	}
	t.Setenv("MONGODB_URI", uri)
	t.Setenv("ARGON_METADATA_DB", "argon_dt_"+primitive.NewObjectID().Hex())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = client.Database(metadataDatabase()).Drop(cleanup)
		_ = client.Disconnect(cleanup)
	}()
	probeDatabases := func() []string {
		t.Helper()
		// Atlas accepts only a plain regex or string for the name filter,
		// not the {$regex: ...} operator form.
		names, err := client.ListDatabaseNames(ctx, bson.M{"name": primitive.Regex{Pattern: "^argon_doctor_[0-9a-f]{24}$"}})
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	before := len(probeDatabases())

	t.Run("passes when hello carries an operationTime", func(t *testing.T) {
		report := inspectDeployment(ctx, true)
		check, ok := doctorCheck(report, "capture_permissions")
		if !ok || !check.OK || !report.Healthy {
			t.Fatalf("doctor should pass on a replica set: %+v", report.Checks)
		}
		if !strings.Contains(check.Detail, "capture boundary") {
			t.Errorf("detail does not say the capture boundary was verified: %q", check.Detail)
		}
		if got := len(probeDatabases()); got != before {
			t.Errorf("temporary probe database left behind: %d before, %d after", before, got)
		}
	})

	t.Run("fails and names operationTime when hello carries none", func(t *testing.T) {
		original := checkCaptureBoundary
		defer func() { checkCaptureBoundary = original }()
		checkCaptureBoundary = func(ctx context.Context, db *mongo.Database) error {
			return errors.New("MongoDB replica set with change streams is required (hello returned no operationTime)")
		}
		report := inspectDeployment(ctx, true)
		check, ok := doctorCheck(report, "capture_permissions")
		if !ok || check.OK || report.Healthy {
			t.Fatalf("doctor must fail without a capture boundary: %+v", report.Checks)
		}
		if !strings.Contains(check.Detail, "operationTime") {
			t.Errorf("failure does not name operationTime: %q", check.Detail)
		}
		if got := len(probeDatabases()); got != before {
			t.Errorf("temporary probe database left behind after a failed check: %d before, %d after", before, got)
		}
	})

	t.Run("status stays read-only", func(t *testing.T) {
		original := checkCaptureBoundary
		defer func() { checkCaptureBoundary = original }()
		checkCaptureBoundary = func(context.Context, *mongo.Database) error {
			t.Error("status must not run the capture boundary probe")
			return nil
		}
		if report := inspectDeployment(ctx, false); !report.Healthy {
			t.Fatalf("status should be healthy: %+v", report.Checks)
		}
	})
}

// A refused probe names the roles to grant, and says so in Atlas terms when
// the deployment is Atlas. Atlas reports a missing privilege as AtlasError
// 8000 rather than Unauthorized (13).
func TestDoctorRoleHintForRefusedProbe(t *testing.T) {
	atlasRefusal := mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "user is not allowed to do action [dropDatabase] on [argon_doctor_x.]"}
	selfManagedRefusal := mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized on argon_doctor_x to execute command { dropDatabase: 1 }"}
	wrapped := func(err error) error { return fmt.Errorf("remove temporary physical database: %w", err) }

	atlas := explainProbeError(wrapped(atlasRefusal), []string{"ac-abc-shard-00-00.q4bf3vu.mongodb.net:27017"})
	for _, want := range []string{"dropDatabase", "MongoDB Atlas", "readWriteAnyDatabase", "dbAdminAnyDatabase", "Atlas admin"} {
		if !strings.Contains(atlas.Error(), want) {
			t.Errorf("Atlas refusal hint lacks %q: %v", want, atlas)
		}
	}
	var original mongo.CommandError
	if !errors.As(atlas, &original) || original.Code != 8000 {
		t.Error("the original error must stay wrapped")
	}

	selfManaged := explainProbeError(wrapped(selfManagedRefusal), []string{"localhost:27017"})
	for _, want := range []string{"readWriteAnyDatabase", "dbAdminAnyDatabase"} {
		if !strings.Contains(selfManaged.Error(), want) {
			t.Errorf("refusal hint lacks %q: %v", want, selfManaged)
		}
	}
	if strings.Contains(selfManaged.Error(), "Atlas") {
		t.Errorf("a self-managed deployment gets no Atlas instructions: %v", selfManaged)
	}

	other := errors.New("capture probe did not observe its update")
	if got := explainProbeError(other, []string{"ac-abc-shard-00-00.q4bf3vu.mongodb.net:27017"}); got != other {
		t.Errorf("errors other than a refusal are returned unchanged, got %v", got)
	}
}

func TestDoctorAtlasUsage(t *testing.T) {
	if got := formatAtlasUsage(12, 45, 3_400_000); got != "12 databases, 45 collections, 3.4 MB of data and indexes (the free M0 allows 100 databases, 500 collections and 0.5 GB)" {
		t.Errorf("formatAtlasUsage = %q", got)
	}
	if !isAtlasDeployment([]string{"ac-abc-shard-00-01.q4bf3vu.mongodb.net:27017"}) || isAtlasDeployment([]string{"localhost:27017"}) {
		t.Error("isAtlasDeployment misclassifies hosts")
	}

	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to count usage on a real deployment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	name := "argon_usage_" + primitive.NewObjectID().Hex()
	db := client.Database(name)
	defer func() { _ = db.Drop(context.Background()) }()
	for _, c := range []string{"a", "b"} {
		if _, err := db.Collection(c).InsertOne(ctx, bson.M{"v": strings.Repeat("x", 1000)}); err != nil {
			t.Fatal(err)
		}
	}
	before := struct{ dbs, colls int }{}
	dbs, colls, bytes, err := atlasUsage(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	before.dbs, before.colls = dbs, colls
	if dbs < 1 || colls < 2 || bytes < 2000 {
		t.Errorf("usage %d databases, %d collections, %d bytes; want at least 1, 2, 2000", dbs, colls, bytes)
	}
	if _, err := client.Database(name).Collection("c").InsertOne(ctx, bson.M{"v": 1}); err != nil {
		t.Fatal(err)
	}
	_, colls, _, err = atlasUsage(ctx, client)
	if err != nil || colls != before.colls+1 {
		t.Errorf("a new collection should count: before %d, after %d (%v)", before.colls, colls, err)
	}
}
