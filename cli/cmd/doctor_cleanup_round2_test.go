package cmd

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Round 2 review of the probe's cleanup (b42458b): the rows of the case
// table the kept tests do not reach, and what the fixes left.

// round2Proxy stands between the probe's client and the deployment, to lose
// a reply (a command that failed for the client and took effect on the
// server) or to stop answering altogether.
type round2Proxy struct {
	listener net.Listener
	target   string
	// cut: the message is delivered, the server's reply is awaited and
	// thrown away, and the client's connection is closed.
	cut func(message []byte) bool
	// stallOn: from this message on (it is not delivered) nothing passes in
	// either direction, on any connection, old or new.
	stallOn func(message []byte) bool
	stalled atomic.Bool
	cuts    atomic.Int32

	mu   sync.Mutex
	seen []byte
}

func newRound2Proxy(t *testing.T, target string) *round2Proxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &round2Proxy{listener: listener, target: target}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(client)
		}
	}()
	return p
}

func (p *round2Proxy) serve(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	var dropReply atomic.Bool
	done := make(chan struct{}, 2)
	go func() { // client to server
		defer func() { done <- struct{}{} }()
		buffer := make([]byte, 1<<20)
		for {
			n, err := client.Read(buffer)
			if n > 0 {
				message := buffer[:n]
				if p.stallOn != nil && p.stallOn(message) {
					p.stalled.Store(true)
				}
				if !p.stalled.Load() {
					p.mu.Lock()
					p.seen = append(p.seen, message...)
					p.mu.Unlock()
					if p.cut != nil && p.cut(message) {
						dropReply.Store(true)
					}
					if _, err := server.Write(message); err != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { // server to client
		defer func() { done <- struct{}{} }()
		buffer := make([]byte, 1<<20)
		for {
			n, err := server.Read(buffer)
			if n > 0 && !p.stalled.Load() {
				if dropReply.Load() {
					p.cuts.Add(1)
					_ = client.Close()
					return
				}
				if _, err := client.Write(buffer[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	<-done
	if p.stalled.Load() {
		// A stalled deployment keeps its connections open and silent.
		<-done
	}
	_ = client.Close()
	_ = server.Close()
}

func (p *round2Proxy) probeName() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return reviewProbeName.FindString(string(p.seen))
}

// round2User creates a user (built-in roles, plus a role of its own when
// privileges are given) and returns a client for it; through the proxy when
// one is given. The role's name is returned for later grants.
func round2User(d *reviewDeployment, name string, roles bson.A, privileges bson.A, proxy *round2Proxy) (*mongo.Client, string) {
	d.t.Helper()
	adminDB := d.admin.Database("admin")
	user, password := "r2_"+name+"_"+d.id, primitive.NewObjectID().Hex()+primitive.NewObjectID().Hex()
	role := ""
	if privileges != nil {
		role = "r2_role_" + name + "_" + d.id
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
	if proxy != nil {
		limitedURI.Host = proxy.listener.Addr().String()
		query := limitedURI.Query()
		query.Del("replicaSet")
		query.Set("directConnection", "true")
		limitedURI.RawQuery = query.Encode()
	}
	client, err := mongo.Connect(d.ctx, options.Client().ApplyURI(limitedURI.String()).SetServerSelectionTimeout(5*time.Second))
	if err != nil {
		d.t.Fatal("connect as the limited user failed")
	}
	d.t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = adminDB.RunCommand(cleanup, bson.D{{Key: "dropUser", Value: user}}).Err()
		if role != "" {
			_ = adminDB.RunCommand(cleanup, bson.D{{Key: "dropRole", Value: role}}).Err()
		}
		short, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = client.Disconnect(short)
	})
	return client, role
}

func round2Target(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal("the test URI does not parse")
	}
	return parsed.Host
}

// round2Created reports whether the oplog holds the probe collection's
// create in this database: what the probe really made.
func round2Created(d *reviewDeployment, database string) bool {
	d.t.Helper()
	err := d.admin.Database("local").Collection("oplog.rs").FindOne(d.ctx, bson.D{
		{Key: "op", Value: "c"}, {Key: "ns", Value: database + ".$cmd"}, {Key: "o.create", Value: "probe"},
	}).Err()
	if err != nil && err != mongo.ErrNoDocuments {
		d.t.Fatal(err)
	}
	return err == nil
}

// FINDING (nit, test only; the round 1 fix is incomplete). The kept test now
// spares the probe databases that were there when it started, and still
// treats every probe database that appears while it runs as its own: one
// from another doctor run (or another test) that starts a moment later
// makes it fail with "the probe left a database behind", and its cleanup
// then drops that database as root.
func TestRound2DoctorCleanupTestLeavesLaterRunsAlone(t *testing.T) {
	admin, _ := reviewAdmin(t)
	other := admin.Database("argon_doctor_" + primitive.NewObjectID().Hex())
	t.Cleanup(func() { _ = other.Drop(context.Background()) })
	// Another run's probe database appears while the test under review is
	// inside its own probe.
	captureProbeName(t, func(ctx context.Context, physical *mongo.Database) error {
		if _, err := other.Collection("probe").InsertOne(ctx, bson.M{"_id": "probe"}); err != nil {
			return err
		}
		return walcli.CheckCaptureBoundary(ctx, physical)
	})

	passed := t.Run("the test under review", TestDoctorProbeCleansUpWhenDropDatabaseIsRefused)

	names, err := admin.ListDatabaseNames(context.Background(), bson.D{{Key: "name", Value: other.Name()}})
	if err != nil {
		t.Fatal(err)
	}
	if !passed {
		t.Errorf("TestDoctorProbeCleansUpWhenDropDatabaseIsRefused failed although its own probe left nothing: it counted %s, another run's database", other.Name())
	}
	if len(names) == 0 {
		t.Errorf("TestDoctorProbeCleansUpWhenDropDatabaseIsRefused dropped %s, a probe database it did not create", other.Name())
	}
}

// Row: create fails for the client (its reply is lost) and took effect on
// the server. Not a refusal, so the database may exist and must be dropped.
// No kept test reaches this row: physicalMayExist = false after any create
// error passes all of them.
func TestRound2DoctorCreateThatFailedButTookEffectIsDropped(t *testing.T) {
	d := newReviewDeployment(t)
	proxy := newRound2Proxy(t, round2Target(t, d.uri))
	proxy.cut = func(message []byte) bool {
		return bytes.Contains(message, []byte("changeStreamPreAndPostImages"))
	}
	limited, _ := round2User(d, "lost", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil, proxy)

	err := probeCapture(d.ctx, limited)
	if err == nil || !strings.HasPrefix(err.Error(), "create collection with exact images: ") {
		t.Fatalf("the probe must fail at create, got %v", err)
	}
	if walcli.IsUnauthorized(err) {
		t.Fatalf("a lost reply must not read as a refusal: %v", err)
	}
	name := proxy.probeName()
	if name == "" || proxy.cuts.Load() != 1 {
		t.Fatalf("the proxy did not see the create (name %q, cuts %d)", name, proxy.cuts.Load())
	}
	if !round2Created(d, name) {
		t.Fatalf("%s was never created, the premise of this test is wrong", name)
	}
	if d.exists(name) {
		t.Errorf("the create failed for the client and took effect; the probe left %s behind", name)
	}
	if strings.Contains(err.Error(), "drop it by hand") {
		t.Errorf("the database was removed, but the error says to drop by hand: %v", err)
	}
}

// Row: the metadata insert fails for the client (both attempts' replies are
// lost) and took effect. Not a refusal, so the collection may exist and
// must be dropped.
func TestRound2DoctorMetadataInsertThatFailedButTookEffectIsDropped(t *testing.T) {
	d := newReviewDeployment(t)
	proxy := newRound2Proxy(t, round2Target(t, d.uri))
	proxy.cut = func(message []byte) bool {
		return bytes.Contains(message, []byte("doctor_")) && bytes.Contains(message, []byte("setup")) && bytes.Contains(message, []byte("insert"))
	}
	limited, _ := round2User(d, "lostmeta", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil, proxy)
	name := captureProbeName(t, nil)

	err := probeCapture(d.ctx, limited)
	if err == nil || !strings.HasPrefix(err.Error(), "write metadata: ") {
		t.Fatalf("the probe must fail at its metadata insert, got %v (cuts %d)", err, proxy.cuts.Load())
	}
	if walcli.IsUnauthorized(err) || mongo.IsDuplicateKeyError(err) {
		t.Fatalf("expected a lost reply, got %v", err)
	}
	if proxy.cuts.Load() == 0 {
		t.Fatal("the proxy lost no reply, the premise of this test is wrong")
	}
	if d.exists(*name) {
		t.Errorf("the probe left %s behind", *name)
	}
	if left := d.metadataCollections(); len(left) != 0 {
		t.Errorf("the insert failed for the client and took effect; metadata collections left behind: %v", left)
	}
	if strings.Contains(err.Error(), "drop it by hand") {
		t.Errorf("everything was removed, but the error says to drop by hand: %v", err)
	}
}

// Row: create ok, the metadata insert is refused. The collection was never
// created, so the error must not name it for removal by hand. No kept test
// reaches this row: dropping the line that clears metadataMayExist on a
// refusal passes all of them.
func TestRound2DoctorRefusedMetadataInsertIsNotToldToDropByHand(t *testing.T) {
	d := newReviewDeployment(t)
	// May create the probe collection anywhere, and nothing else, until the
	// probe's database is known.
	limited, role := round2User(d, "nometa", bson.A{}, bson.A{bson.D{
		{Key: "resource", Value: bson.D{{Key: "db", Value: ""}, {Key: "collection", Value: "probe"}}},
		{Key: "actions", Value: bson.A{"createCollection"}},
	}}, nil)
	name := captureProbeName(t, func(ctx context.Context, physical *mongo.Database) error {
		// Everything on the probe's own database, nothing on the metadata
		// database.
		if err := d.admin.Database("admin").RunCommand(ctx, bson.D{
			{Key: "grantPrivilegesToRole", Value: role},
			{Key: "privileges", Value: bson.A{bson.D{
				{Key: "resource", Value: bson.D{{Key: "db", Value: physical.Name()}, {Key: "collection", Value: ""}}},
				{Key: "actions", Value: bson.A{"find", "insert", "update", "remove", "changeStream", "listCollections", "dropCollection", "dropDatabase"}},
			}}},
		}).Err(); err != nil {
			return err
		}
		return walcli.CheckCaptureBoundary(ctx, physical)
	})

	err := probeCapture(d.ctx, limited)
	if err == nil || !walcli.IsUnauthorized(err) {
		t.Fatalf("the probe must fail on the refused metadata insert, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "write metadata: ") {
		t.Fatalf("the probe's own error must come first: %v", err)
	}
	if d.exists(*name) {
		t.Errorf("the probe left %s behind", *name)
	}
	if left := d.metadataCollections(); len(left) != 0 {
		t.Fatalf("metadata collections exist, the premise of this test is wrong: %v", left)
	}
	if strings.Contains(err.Error(), "drop it by hand") || strings.Contains(err.Error(), "could not be removed") {
		t.Errorf("the metadata collection was never created, but the error names it for removal:\n%v", err)
	}
	if !strings.Contains(explainProbeError(err, []string{"localhost:27019"}).Error(), "readWriteAnyDatabase") {
		t.Errorf("the role hint is missing: %v", err)
	}
}

// Row: Drop stalls for its whole 5 seconds, body ok. The metadata collection
// must still be removed: that is what "one timeout per step" is for. The
// kept test for this (TestReviewStalledDropStillRemovesMetadata) accepts a
// collection that is left behind as long as the error mentions it, so it
// passes with the single shared timeout restored.
func TestRound2DoctorStalledDropLeavesNoMetadataCollection(t *testing.T) {
	d := newReviewDeployment(t)
	limited, _ := round2User(d, "stall", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil, nil)
	session, err := d.admin.StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = session.AbortTransaction(context.Background())
		session.EndSession(context.Background())
	}()
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
		return walcli.CheckCaptureBoundary(ctx, physical)
	})

	started := time.Now()
	err = probeCapture(d.ctx, limited)
	took := time.Since(started)
	_ = session.AbortTransaction(context.Background())
	_ = d.exists(*name) // registers its removal
	if err == nil || !strings.HasPrefix(err.Error(), "remove temporary physical database: ") {
		t.Fatalf("expected the stalled dropDatabase to be the result, got %v", err)
	}
	t.Logf("the probe took %s", took.Round(100*time.Millisecond))
	if left := d.metadataCollections(); len(left) != 0 {
		t.Errorf("the stalled dropDatabase took the metadata drop's time: %v left behind", left)
	}
	if strings.Contains(err.Error(), d.metadata) {
		t.Errorf("the metadata collection was removed, but the error names it: %v", err)
	}
}

// round2StalledProbe runs the probe with a 2s deadline against a deployment
// that stops answering at the given point, and returns how long after the
// deadline the probe came back, and its error.
func round2StalledProbe(t *testing.T, at string) (time.Duration, error) {
	t.Helper()
	d := newReviewDeployment(t)
	proxy := newRound2Proxy(t, round2Target(t, d.uri))
	limited, _ := round2User(d, "silent", bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}, nil, proxy)
	name := captureProbeName(t, func(ctx context.Context, physical *mongo.Database) error {
		if at == "boundary" {
			proxy.stalled.Store(true)
		}
		return walcli.CheckCaptureBoundary(ctx, physical)
	})
	if at == "metadata insert" {
		proxy.stallOn = func(message []byte) bool {
			return bytes.Contains(message, []byte("doctor_")) && bytes.Contains(message, []byte("setup")) && bytes.Contains(message, []byte("insert"))
		}
	}
	const deadline = 2 * time.Second
	ctx, cancel := context.WithTimeout(d.ctx, deadline)
	defer cancel()
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- probeCapture(ctx, limited) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(80 * time.Second):
		t.Fatalf("the probe had not returned 80s after a %s deadline", deadline)
	}
	over := time.Since(started) - deadline
	_ = d.exists(*name) // registers its removal
	t.Logf("returned %s after its deadline: %v", over.Round(100*time.Millisecond), err)
	return over, err
}

// FINDING (nit, this change). The deployment stops answering once the probe
// database exists (here at the capture boundary). The cleanup used to give
// up after 5 seconds in all; each step now waits its own 5 seconds on the
// same dead deployment, so doctor returns 10 seconds after its --timeout
// (15 once the metadata collection exists).
func TestRound2DoctorStalledDeploymentCleanupTime(t *testing.T) {
	over, err := round2StalledProbe(t, "boundary")
	if err == nil || !strings.HasPrefix(err.Error(), "capture boundary: ") {
		t.Fatalf("expected the boundary check to time out, got %v", err)
	}
	if over > 6*time.Second {
		t.Errorf("the probe returned %s after its deadline; with one cleanup timeout it was 5s", over.Round(100*time.Millisecond))
	}
}

// FINDING (pre-existing, not in this diff). The deployment stops answering
// once the change stream is open (here at the metadata insert). The probe's
// deferred stream.Close runs with context.Background(), so before the
// cleanup's three 5-second steps start, it waits for the driver's own
// connect timeout: doctor returns some 40 seconds after its --timeout.
func TestRound2DoctorStalledDeploymentChangeStreamClose(t *testing.T) {
	over, err := round2StalledProbe(t, "metadata insert")
	if err == nil || !strings.HasPrefix(err.Error(), "write metadata: ") {
		t.Fatalf("expected the metadata insert to time out, got %v", err)
	}
	// The cleanup's own budget is three steps of 5 seconds; a close bounded
	// like one of them would make 20.
	if over > 22*time.Second {
		t.Errorf("the probe returned %s after its deadline, far more than the cleanup's 15s: closing the change stream has no deadline", over.Round(100*time.Millisecond))
	}
}
