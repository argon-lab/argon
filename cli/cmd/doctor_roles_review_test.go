package cmd

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// reviewProbeResult is what one probeCapture run, made as a freshly created
// user, returned and left on the deployment.
type reviewProbeResult struct {
	err             error
	database        string   // the probe's own argon_doctor_<hex>
	metadata        string   // the metadata database of this run
	everCreated     bool     // the oplog holds a create in the probe database
	leftCollections []string // collections still in the probe database
	leftMetadata    []string // doctor_* collections still in the metadata database
	leftPreimages   int64    // config.system.preimages documents of the probe collection
	commands        []string // commands the probe's client sent to the probe database
}

func reviewAdmin(t *testing.T) (*mongo.Client, string) {
	t.Helper()
	uri := os.Getenv("ARGON_TEST_AUTH_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_AUTH_MONGODB_URI (a root user on an access-controlled replica set)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal("connect as root failed")
	}
	t.Cleanup(func() { _ = admin.Disconnect(context.Background()) })
	return admin, uri
}

// reviewRunProbe creates a user with the given roles (a role written
// "@meta:<role>" is granted on the run's metadata database only; customActions,
// when not nil, become a custom role on every database), runs probeCapture as
// that user, and inspects the deployment as root. It removes only what this
// run created.
func reviewRunProbe(t *testing.T, admin *mongo.Client, uri string, roles bson.A, customActions []string) reviewProbeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := primitive.NewObjectID().Hex()
	user, password := "review_roles_"+id, primitive.NewObjectID().Hex()+primitive.NewObjectID().Hex()
	metadataName := "argon_rv_" + id
	t.Setenv("ARGON_METADATA_DB", metadataName)
	granted := bson.A{}
	for _, r := range roles {
		if s, ok := r.(string); ok && strings.HasPrefix(s, "@meta:") {
			granted = append(granted, bson.D{{Key: "role", Value: strings.TrimPrefix(s, "@meta:")}, {Key: "db", Value: metadataName}})
		} else {
			granted = append(granted, r)
		}
	}
	if customActions != nil {
		role := "review_role_" + id
		actions := bson.A{}
		for _, a := range customActions {
			actions = append(actions, a)
		}
		if err := admin.Database("admin").RunCommand(ctx, bson.D{
			{Key: "createRole", Value: role},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: ""}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: actions},
			}}},
			{Key: "roles", Value: bson.A{}},
		}).Err(); err != nil {
			t.Fatalf("createRole: %v", err)
		}
		t.Cleanup(func() {
			_ = admin.Database("admin").RunCommand(context.Background(), bson.D{{Key: "dropRole", Value: role}}).Err()
		})
		granted = append(granted, role)
	}
	if err := admin.Database("admin").RunCommand(ctx, bson.D{
		{Key: "createUser", Value: user}, {Key: "pwd", Value: password}, {Key: "roles", Value: granted},
	}).Err(); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	result := reviewProbeResult{metadata: metadataName}
	var mu sync.Mutex
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = admin.Database("admin").RunCommand(cleanup, bson.D{{Key: "dropUser", Value: user}}).Err()
		_ = admin.Database(metadataName).Drop(cleanup)
		mu.Lock()
		name := result.database
		mu.Unlock()
		if strings.HasPrefix(name, "argon_doctor_") {
			_ = admin.Database(name).Drop(cleanup) // only this run's own database
		}
	})

	limitedURI, err := url.Parse(uri)
	if err != nil {
		t.Fatal("parse uri failed")
	}
	limitedURI.User = url.UserPassword(user, password)
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(e.DatabaseName, "argon_doctor_") {
			result.database = e.DatabaseName
			result.commands = append(result.commands, e.CommandName)
		}
	}}
	limited, err := mongo.Connect(ctx, options.Client().ApplyURI(limitedURI.String()).SetMonitor(monitor))
	if err != nil {
		t.Fatal("connect as limited user failed")
	}
	defer func() { _ = limited.Disconnect(context.Background()) }()

	result.err = probeCapture(ctx, limited)
	if result.err != nil && strings.Contains(result.err.Error(), password) {
		t.Fatal("the error carries the password")
	}
	if result.database == "" {
		t.Fatal("the probe never addressed its database")
	}
	// What the probe really created, from the oplog (root may read it).
	var created struct {
		UI primitive.Binary `bson:"ui"`
	}
	err = admin.Database("local").Collection("oplog.rs").FindOne(ctx, bson.D{
		{Key: "op", Value: "c"}, {Key: "ns", Value: result.database + ".$cmd"}, {Key: "o.create", Value: "probe"},
	}).Decode(&created)
	if err == nil {
		result.everCreated = true
		result.leftPreimages, err = admin.Database("config").Collection("system.preimages").CountDocuments(ctx, bson.D{{Key: "_id.nsUUID", Value: created.UI}})
		if err != nil {
			t.Fatalf("count preimages: %v", err)
		}
	} else if err != mongo.ErrNoDocuments {
		t.Fatalf("read oplog: %v", err)
	}
	result.leftCollections, err = admin.Database(result.database).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	names, err := admin.Database(metadataName).ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "doctor_") {
			result.leftMetadata = append(result.leftMetadata, name)
		}
	}
	return result
}

var reviewReadWriteNoDrop = []string{"find", "insert", "update", "remove", "createCollection", "createIndex", "changeStream", "listCollections", "listIndexes"}
