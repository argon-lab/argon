package cmd

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Review tests for the probe's cleanup: one per row of the case table
// (physical.Drop) x (probe body) x (fallback) x (metadata.Drop).

var reviewProbeName = regexp.MustCompile(`argon_doctor_[0-9a-f]{24}`)

type reviewDeployment struct {
	t        *testing.T
	ctx      context.Context
	admin    *mongo.Client
	uri      string
	id       string
	metadata string
}

func newReviewDeployment(t *testing.T) *reviewDeployment {
	t.Helper()
	uri := os.Getenv("ARGON_TEST_AUTH_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_AUTH_MONGODB_URI (a root user on an access-controlled replica set)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	admin, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	d := &reviewDeployment{t: t, ctx: ctx, admin: admin, uri: uri, id: primitive.NewObjectID().Hex()}
	d.metadata = "argon_rv_" + d.id
	t.Setenv("ARGON_METADATA_DB", d.metadata)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = admin.Database(d.metadata).Drop(cleanup)
		_ = admin.Disconnect(cleanup)
	})
	return d
}

// user creates a user with the given roles (built-in role names, or
// privileges for a role made for this test) and returns its client.
func (d *reviewDeployment) user(name string, roles bson.A, privileges bson.A) *mongo.Client {
	d.t.Helper()
	adminDB := d.admin.Database("admin")
	user, password := "rv_"+name+"_"+d.id, primitive.NewObjectID().Hex()+primitive.NewObjectID().Hex()
	role := ""
	if privileges != nil {
		role = "rv_role_" + name + "_" + d.id
		if err := adminDB.RunCommand(d.ctx, bson.D{{Key: "createRole", Value: role}, {Key: "privileges", Value: privileges}, {Key: "roles", Value: bson.A{}}}).Err(); err != nil {
			d.t.Fatal(err)
		}
		roles = append(roles, role)
	}
	if err := adminDB.RunCommand(d.ctx, bson.D{{Key: "createUser", Value: user}, {Key: "pwd", Value: password}, {Key: "roles", Value: roles}}).Err(); err != nil {
		d.t.Fatal(err)
	}
	limitedURI, err := url.Parse(d.uri)
	if err != nil {
		d.t.Fatal("the test URI does not parse")
	}
	limitedURI.User = url.UserPassword(user, password)
	client, err := mongo.Connect(d.ctx, options.Client().ApplyURI(limitedURI.String()))
	if err != nil {
		d.t.Fatal("connect as the limited user failed")
	}
	d.t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = client.Disconnect(cleanup)
		_ = adminDB.RunCommand(cleanup, bson.D{{Key: "dropUser", Value: user}}).Err()
		if role != "" {
			_ = adminDB.RunCommand(cleanup, bson.D{{Key: "dropRole", Value: role}}).Err()
		}
	})
	return client
}

// exists reports whether the deployment has a database of this name, and
// removes it at the end of the test either way.
func (d *reviewDeployment) exists(database string) bool {
	d.t.Helper()
	d.t.Cleanup(func() { _ = d.admin.Database(database).Drop(context.Background()) })
	names, err := d.admin.ListDatabaseNames(d.ctx, bson.D{{Key: "name", Value: database}})
	if err != nil {
		d.t.Fatal(err)
	}
	return len(names) == 1
}

func (d *reviewDeployment) metadataCollections() []string {
	d.t.Helper()
	names, err := d.admin.Database(d.metadata).ListCollectionNames(d.ctx, bson.D{})
	if err != nil {
		d.t.Fatal(err)
	}
	return names
}

// captureProbeName makes the probe report its own database's name: other
// runs against the same deployment make a count of probe databases unsafe.
func captureProbeName(t *testing.T, inside func(ctx context.Context, physical *mongo.Database) error) *string {
	t.Helper()
	name := new(string)
	original := checkCaptureBoundary
	t.Cleanup(func() { checkCaptureBoundary = original })
	checkCaptureBoundary = func(ctx context.Context, physical *mongo.Database) error {
		*name = physical.Name()
		if inside != nil {
			return inside(ctx, physical)
		}
		return original(ctx, physical)
	}
	return name
}

// Row: Drop refused, body failed at create (refused), fallback refused,
// metadata.Drop refused. A user with no access to the probe's database made
// nothing, so nothing is left behind, and the error must not send the
// operator to drop a database that does not exist.
func TestReviewUserWithoutAccessIsNotToldToDropByHand(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("app", bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "rv_app_" + d.id}}}, nil)

	err := probeCapture(d.ctx, limited)
	if err == nil || !walcli.IsUnauthorized(err) {
		t.Fatalf("the probe must fail on the refused create, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "create collection with exact images: ") {
		t.Fatalf("the probe's own error must come first: %v", err)
	}
	database := reviewProbeName.FindString(err.Error())
	if database == "" {
		t.Fatalf("the error does not name the probe's database: %v", err)
	}
	if d.exists(database) {
		t.Fatalf("%s exists, the premise of this test is wrong", database)
	}
	if strings.Contains(err.Error(), "drop it by hand") {
		t.Errorf("%s was never created, but the error tells the operator to drop it by hand:\n%v", database, err)
	}
}

// Row: Drop refused, body failed at create (refused), fallback ok (nothing
// to drop), metadata.Drop refused. A read-only user leaves nothing behind
// and gets the create refusal, with no hint about a leftover database.
func TestReviewReadOnlyUserLeavesNothing(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("ro", bson.A{"readAnyDatabase"}, nil)

	err := probeCapture(d.ctx, limited)
	if err == nil || !walcli.IsUnauthorized(err) {
		t.Fatalf("the probe must fail on the refused create, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "create collection with exact images: ") {
		t.Fatalf("the probe's own error must come first: %v", err)
	}
	if strings.Contains(err.Error(), "drop it by hand") {
		t.Errorf("nothing was created, but the error says to drop by hand: %v", err)
	}
	if database := reviewProbeName.FindString(err.Error()); database == "" || d.exists(database) {
		t.Errorf("the probe's database %q is missing from the error or was left behind", database)
	}
	if left := d.metadataCollections(); len(left) != 0 {
		t.Errorf("metadata collections left behind: %v", left)
	}
	hinted := explainProbeError(err, []string{"localhost:27019"})
	if !strings.Contains(hinted.Error(), "readWriteAnyDatabase") {
		t.Errorf("the role hint is missing: %v", hinted)
	}
}

// Row: Drop refused, body failed after the metadata collection exists (not
// an authorization error), fallback ok, metadata.Drop ok. The body's error
// is the result, unchanged, and nothing is left behind.
func TestReviewBodyErrorSurvivesRefusedDrop(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("rw", bson.A{"readWriteAnyDatabase"}, nil)
	name := captureProbeName(t, func(ctx context.Context, physical *mongo.Database) error {
		// Take the probe's metadata _id so its own insert fails.
		collection := "doctor_" + strings.TrimPrefix(physical.Name(), "argon_doctor_")
		_, err := d.admin.Database(d.metadata).Collection(collection).InsertOne(ctx, bson.M{"_id": "setup"})
		return err
	})

	err := probeCapture(d.ctx, limited)
	if err == nil || !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("the probe must fail on its metadata insert, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "write metadata: ") || strings.Contains(err.Error(), "drop it by hand") || strings.Contains(err.Error(), "remove temporary") {
		t.Errorf("the body's error must be the result, unchanged: %v", err)
	}
	if walcli.IsUnauthorized(err) {
		t.Errorf("a body failure that is not a refusal must not read as one: %v", err)
	}
	if d.exists(*name) {
		t.Errorf("the probe left %s behind", *name)
	}
	if left := d.metadataCollections(); len(left) != 0 {
		t.Errorf("metadata collections left behind: %v", left)
	}
}

// noDropPrivileges lets a user do everything the probe's body does, and
// drop nothing.
func noDropPrivileges() bson.A {
	return bson.A{bson.D{
		{Key: "resource", Value: bson.D{{Key: "db", Value: ""}, {Key: "collection", Value: ""}}},
		{Key: "actions", Value: bson.A{"find", "insert", "update", "remove", "createCollection", "changeStream", "listCollections"}},
	}}
}

// Row: Drop refused, body ok, fallback refused, metadata.Drop refused. The
// refusal is the result and still reads as one; the database is named. The
// metadata collection is left behind too, and the error must name it as
// well: "drop it by hand" otherwise sends the operator to half of what is
// left.
func TestReviewRefusedCleanupNamesEverythingLeftBehind(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("nodrop", bson.A{}, noDropPrivileges())
	name := captureProbeName(t, nil)

	err := probeCapture(d.ctx, limited)
	if err == nil || !walcli.IsUnauthorized(err) {
		t.Fatalf("the probe must fail on the refused dropDatabase, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "remove temporary physical database: ") {
		t.Fatalf("the failure names the step that was refused: %v", err)
	}
	if !d.exists(*name) {
		t.Fatalf("%s is gone, the premise of this test is wrong", *name)
	}
	if !strings.Contains(err.Error(), "the probe's database "+*name+" could not be emptied either") || !strings.Contains(err.Error(), "drop it by hand") {
		t.Errorf("the error does not name the database left behind: %v", err)
	}
	if !strings.Contains(explainProbeError(err, []string{"localhost:27019"}).Error(), "dbAdminAnyDatabase") {
		t.Errorf("the role hint is missing: %v", err)
	}
	collection := "doctor_" + strings.TrimPrefix(*name, "argon_doctor_")
	left := d.metadataCollections()
	if len(left) != 1 || left[0] != collection {
		t.Fatalf("expected the metadata collection %s to be left behind, found %v", collection, left)
	}
	// The collection's name is part of the database's, so look for its
	// namespace.
	if !strings.Contains(err.Error(), d.metadata) {
		t.Errorf("%s.%s was left behind and the error does not mention it:\n%v", d.metadata, collection, err)
	}
}

// Row: Drop refused, body ok, fallback ok, metadata.Drop refused: a user
// that may drop the probe's collection and nothing else. The database goes,
// the metadata collection stays, and the error must say so.
func TestReviewRefusedMetadataDropIsReported(t *testing.T) {
	d := newReviewDeployment(t)
	privileges := append(noDropPrivileges(), bson.D{
		{Key: "resource", Value: bson.D{{Key: "db", Value: ""}, {Key: "collection", Value: "probe"}}},
		{Key: "actions", Value: bson.A{"dropCollection"}},
	})
	limited := d.user("probeonly", bson.A{}, privileges)
	name := captureProbeName(t, nil)

	err := probeCapture(d.ctx, limited)
	if err == nil || !walcli.IsUnauthorized(err) {
		t.Fatalf("the probe must fail on the refused dropDatabase, got %v", err)
	}
	if d.exists(*name) {
		t.Fatalf("the fallback should have removed %s", *name)
	}
	// The metadata collection is rightly named for removal by hand; the
	// database, which is gone, must not be.
	if strings.Contains(err.Error(), "could not be emptied either") {
		t.Errorf("the database is gone, but the error says to drop it by hand: %v", err)
	}
	collection := "doctor_" + strings.TrimPrefix(*name, "argon_doctor_")
	left := d.metadataCollections()
	if len(left) != 1 || left[0] != collection {
		t.Fatalf("expected the metadata collection %s to be left behind, found %v", collection, left)
	}
	if !strings.Contains(err.Error(), d.metadata) {
		t.Errorf("%s.%s was left behind and the error does not mention it:\n%v", d.metadata, collection, err)
	}
}

// Row: Drop refused, body failed (not an authorization error), fallback
// refused, metadata.Drop refused. The body's error stays the result and is
// still wrapped; the database left behind is named.
func TestReviewBodyErrorKeptWhenFallbackIsRefused(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("nodrop2", bson.A{}, noDropPrivileges())
	sentinel := errors.New("review: boundary unavailable")
	name := captureProbeName(t, func(context.Context, *mongo.Database) error { return sentinel })

	err := probeCapture(d.ctx, limited)
	if !errors.Is(err, sentinel) {
		t.Fatalf("the body's error must stay wrapped, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "capture boundary: review: boundary unavailable; the probe's database "+*name+" could not be emptied either") {
		t.Errorf("the body's error must come first, then the database left behind: %v", err)
	}
	if !d.exists(*name) {
		t.Errorf("%s is gone, the premise of this test is wrong", *name)
	}
}

// Row: Drop fails for another reason (the client is gone), body failed,
// fallback failed, metadata.Drop failed. The fallback runs for any Drop
// error; the body's error stays the result and the database is named.
func TestReviewDropFailsForAnotherReason(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("gone", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil)
	name := captureProbeName(t, func(ctx context.Context, physical *mongo.Database) error {
		return physical.Client().Disconnect(ctx)
	})

	err := probeCapture(d.ctx, limited)
	if !errors.Is(err, mongo.ErrClientDisconnected) {
		t.Fatalf("expected the disconnected client's error, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "open change stream: ") {
		t.Errorf("the body's error must come first: %v", err)
	}
	if walcli.IsUnauthorized(err) {
		t.Errorf("a lost connection must not read as a refusal: %v", err)
	}
	if !d.exists(*name) {
		t.Fatalf("%s is gone, the premise of this test is wrong", *name)
	}
	// Not a refusal, so the collection-by-collection fallback is not tried.
	if !strings.Contains(err.Error(), "the probe's database "+*name+" may be left behind") || !strings.Contains(err.Error(), "drop it by hand") {
		t.Errorf("the database left behind is not named: %v", err)
	}
}

// Row: Drop ok, body ok, metadata.Drop ok, for the roles the hint names.
func TestReviewFullRolesPassAndLeaveNothing(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("full", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil)
	name := captureProbeName(t, nil)

	if err := probeCapture(d.ctx, limited); err != nil {
		t.Fatalf("the probe must pass with the roles the hint names: %v", err)
	}
	if d.exists(*name) {
		t.Errorf("the probe left %s behind", *name)
	}
	if left := d.metadataCollections(); len(left) != 0 {
		t.Errorf("metadata collections left behind: %v", left)
	}
}

// Row: Drop does not answer within the cleanup's 5 seconds (another
// session's open transaction holds the database's lock), body ok. The one
// cleanup context is spent on the first step, so the fallback and the
// metadata drop get no time at all: the metadata collection is left behind
// and the error does not mention it.
func TestReviewStalledDropStillRemovesMetadata(t *testing.T) {
	d := newReviewDeployment(t)
	limited := d.user("stall", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil)
	session, err := d.admin.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = session.AbortTransaction(context.Background())
		session.EndSession(context.Background())
	}()
	original := checkCaptureBoundary
	name := captureProbeName(t, func(ctx context.Context, physical *mongo.Database) error {
		if err := session.StartTransaction(); err != nil {
			return err
		}
		held := d.admin.Database(physical.Name()).Collection("probe")
		if err := mongo.WithSession(ctx, session, func(sc mongo.SessionContext) error {
			_, err := held.InsertOne(sc, bson.M{"_id": "held"})
			return err
		}); err != nil {
			return err
		}
		return original(ctx, physical)
	})

	err = probeCapture(d.ctx, limited)
	_ = session.AbortTransaction(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the stalled dropDatabase to time out, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "remove temporary physical database: ") {
		t.Fatalf("the failure names the step that stalled: %v", err)
	}
	collection := "doctor_" + strings.TrimPrefix(*name, "argon_doctor_")
	_ = d.exists(*name) // registers its removal
	for _, left := range d.metadataCollections() {
		if left == collection && !strings.Contains(err.Error(), d.metadata) {
			t.Errorf("%s.%s was left behind and the error does not mention it:\n%v", d.metadata, collection, err)
		}
	}
}
