package cmd

import (
	"context"
	"errors"
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
	t.Setenv("ARGON_METADATA_DB", "argon_doctor_test_"+primitive.NewObjectID().Hex())
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
		names, err := client.ListDatabaseNames(ctx, bson.M{"name": bson.M{"$regex": "^argon_doctor_[0-9a-f]{24}$"}})
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
