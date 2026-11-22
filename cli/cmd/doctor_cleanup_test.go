package cmd

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A user with readWriteAnyDatabase alone fails the probe at dropDatabase.
// The probe must still leave nothing behind: such a user may drop the
// collections it created, and a database with no collections is gone.
func TestDoctorProbeCleansUpWhenDropDatabaseIsRefused(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_AUTH_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_AUTH_MONGODB_URI (a root user on an access-controlled replica set) to test the probe's cleanup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Disconnect(context.Background()) }()

	id := primitive.NewObjectID().Hex()
	user, password := "doctor_limited_"+id, primitive.NewObjectID().Hex()+primitive.NewObjectID().Hex()
	if err := admin.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: user}, {Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{"readWriteAnyDatabase"}},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	metadataName := "argon_dt_" + id
	t.Setenv("ARGON_METADATA_DB", metadataName)
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = admin.Database("admin").RunCommand(cleanup, bson.D{{Key: "dropUser", Value: user}}).Err()
		_ = admin.Database(metadataName).Drop(cleanup)
		names, _ := admin.ListDatabaseNames(cleanup, bson.D{})
		for _, name := range names {
			if probeDatabaseName.MatchString(name) {
				_ = admin.Database(name).Drop(cleanup)
			}
		}
	}()

	limitedURI, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	limitedURI.User = url.UserPassword(user, password)
	limited, err := mongo.Connect(ctx, options.Client().ApplyURI(limitedURI.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limited.Disconnect(context.Background()) }()

	probeDatabases := func() []string {
		t.Helper()
		all, err := admin.ListDatabaseNames(ctx, bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, name := range all {
			if probeDatabaseName.MatchString(name) {
				names = append(names, name)
			}
		}
		return names
	}
	before := probeDatabases()

	err = probeCapture(ctx, limited)
	if err == nil || !walcli.IsUnauthorized(err) {
		t.Fatalf("the probe must fail on the refused dropDatabase, got %v", err)
	}
	if !strings.Contains(err.Error(), "remove temporary physical database") {
		t.Fatalf("the failure names the step that was refused: %v", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Fatal("the error must not carry the connection string")
	}
	if after := probeDatabases(); len(after) != len(before) {
		t.Fatalf("the probe left a database behind: before %v, after %v", before, after)
	}
	collections, err := admin.Database(metadataName).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range collections {
		if strings.HasPrefix(name, "doctor_") {
			t.Fatalf("the probe left its metadata collection %s behind", name)
		}
	}
}
