package wal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/sandbox"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

func sandboxAuthURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("ARGON_TEST_AUTH_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_AUTH_MONGODB_URI to a root account on an authenticated replica set")
	}
	return uri
}

func credentialServices(t *testing.T, uri, metadataName string) *walcli.Services {
	t.Helper()
	svc, err := walcli.NewServicesAt(uri, metadataName)
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.Monitor.Stop()
		require.NoError(t, svc.Client.Disconnect(context.Background()))
	})
	return svc
}

func credentialProject(t *testing.T) (*walcli.Services, *mongo.Database, *wal.Branch) {
	t.Helper()
	name := fmt.Sprintf("argon_credential_test_%d", time.Now().UnixNano())
	svc := credentialServices(t, sandboxAuthURI(t), name)
	db := svc.Client.Database(name)
	t.Cleanup(func() {
		ctx := context.Background()
		ids, err := db.Collection("wal_branches").Distinct(ctx, "_id", bson.M{})
		require.NoError(t, err)
		for _, id := range ids {
			physical := svc.Client.Database(checkout.PhysicalDBName(id.(string)))
			require.NoError(t, physical.RunCommand(ctx, bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err())
			require.NoError(t, physical.Drop(ctx))
		}
		require.NoError(t, db.Drop(ctx))
	})
	project, err := svc.Projects.CreateProject("credential-test")
	require.NoError(t, err)
	writer, err := svc.WriterFor(project.Name, "main")
	require.NoError(t, err)
	_, err = writer.Put(t.Context(), "tickets", bson.M{"_id": "seed"})
	require.NoError(t, err)
	main, err := svc.Branches.GetBranch(project.ID, "main")
	require.NoError(t, err)
	return svc, db, main
}

func credentialClient(t *testing.T, uri string) *mongo.Client {
	t.Helper()
	// A single socket and no write retry exercise the connection that actually
	// wrote before revocation, including its first write after revocation.
	client, err := mongo.Connect(t.Context(), options.Client().ApplyURI(uri).
		SetMaxPoolSize(1).SetRetryWrites(false).SetServerSelectionTimeout(5*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	return client
}

func sandboxUserCount(t *testing.T, db *mongo.Database) int {
	t.Helper()
	var reply struct{ Users []bson.Raw }
	require.NoError(t, db.RunCommand(t.Context(), bson.D{{Key: "usersInfo", Value: 1}}).Decode(&reply))
	return len(reply.Users)
}

// Check raw stored documents and logs without printing the password if it leaks.
func requirePasswordAbsent(t *testing.T, metadata *mongo.Database, password, processLog string) {
	t.Helper()
	ctx := t.Context()
	collections, err := metadata.ListCollectionNames(ctx, bson.M{})
	require.NoError(t, err)
	for _, collection := range collections {
		cursor, err := metadata.Collection(collection).Find(ctx, bson.M{})
		require.NoError(t, err)
		for cursor.Next(ctx) {
			if bytes.Contains(cursor.Current, []byte(password)) {
				t.Errorf("sandbox password appeared in metadata collection %s", collection)
			}
		}
		require.NoError(t, cursor.Err())
		require.NoError(t, cursor.Close(ctx))
	}
	logBytes, err := os.ReadFile(processLog)
	require.NoError(t, err)
	if bytes.Contains(logBytes, []byte(password)) {
		t.Error("sandbox password appeared in the application log")
	}
	var mongoLog struct{ Log []string }
	require.NoError(t, metadata.Client().Database("admin").RunCommand(ctx, bson.D{{Key: "getLog", Value: "global"}}).Decode(&mongoLog))
	for _, line := range mongoLog.Log {
		if strings.Contains(line, password) {
			t.Fatal("sandbox password appeared in the MongoDB log")
		}
	}
}

func TestSandboxCredentialLifecycle(t *testing.T) {
	svc, db, main := credentialProject(t)
	ctx := t.Context()
	mainCheckout, err := svc.Checkout.Checkout(ctx, main.ID)
	require.NoError(t, err)

	other, err := svc.Sandbox.Create(ctx, main.ProjectID, main.ID, "legacy", time.Hour)
	require.NoError(t, err)
	require.Empty(t, other.CredentialURI)
	require.Zero(t, sandboxUserCount(t, svc.Client.Database(other.PhysicalDB)))
	legacy, err := db.Collection("wal_branches").FindOne(ctx, bson.M{"_id": other.BranchID}).Raw()
	require.NoError(t, err)
	_, hasUsername := legacy.Lookup("sandbox_username").StringValueOK()
	require.False(t, hasUsername)
	require.NoError(t, credentialClient(t, svc.BranchConnectionString(other.PhysicalDB)).
		Database(db.Name()).Collection("wal_branches").FindOne(ctx, bson.M{"_id": main.ID}).Err())

	logFile, err := os.CreateTemp(t.TempDir(), "engine")
	require.NoError(t, err)
	savedLog := log.Writer()
	log.SetOutput(logFile)
	t.Cleanup(func() {
		log.SetOutput(savedLog)
		require.NoError(t, logFile.Close())
	})
	passwords := map[string]bool{}
	for _, ending := range []string{"keep-and-discard", "sweep"} {
		t.Run(ending, func(t *testing.T) {
			ctx := t.Context()
			info, err := svc.Sandbox.CreateWithCredential(ctx, main.ProjectID, main.ID, ending, time.Hour, svc.MongoURI)
			require.NoError(t, err)
			u, err := url.Parse(info.CredentialURI)
			require.True(t, err == nil, "unparseable credential URI")
			require.NotNil(t, u.User)
			require.Equal(t, "/"+info.PhysicalDB, u.Path)
			require.Equal(t, info.PhysicalDB, u.Query().Get("authSource"))
			password, present := u.User.Password()
			require.True(t, present)
			decoded, err := base64.RawURLEncoding.DecodeString(password)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(decoded), 24)
			require.False(t, passwords[password], "sandboxes reused a password")
			passwords[password] = true
			branch, err := svc.Branches.GetBranchByID(info.BranchID)
			require.NoError(t, err)
			require.NotEmpty(t, branch.SandboxUsername)
			require.Equal(t, branch.SandboxUsername, u.User.Username())
			var account struct{ Users []struct{ Roles []bson.M } }
			require.NoError(t, svc.Client.Database(info.PhysicalDB).RunCommand(ctx,
				bson.D{{Key: "usersInfo", Value: branch.SandboxUsername}}).Decode(&account))
			require.Len(t, account.Users, 1)
			require.Equal(t, []bson.M{{"role": "readWrite", "db": info.PhysicalDB}}, account.Users[0].Roles)
			requirePasswordAbsent(t, db, password, logFile.Name())

			client := credentialClient(t, info.CredentialURI)
			tickets := client.Database(info.PhysicalDB).Collection("tickets")
			require.NoError(t, tickets.FindOne(ctx, bson.M{"_id": "seed"}).Err())
			for _, target := range []string{other.PhysicalDB, mainCheckout.PhysicalDB, db.Name(), "argon_wal"} {
				err := client.Database(target).Collection("tickets").FindOne(ctx, bson.M{}).Err()
				var denied mongo.CommandError
				require.ErrorAs(t, err, &denied, "read on %s", target)
				require.EqualValues(t, 13, denied.Code, "read on %s", target)
			}
			// Expire the branch deterministically. Its URI still works while a
			// sweeper has yet to visit it, or indefinitely once Keep succeeds.
			past := time.Now().Add(-time.Minute)
			require.NoError(t, svc.Branches.SetExpiry(info.BranchID, &past))
			_, err = tickets.InsertOne(ctx, bson.M{"_id": "unswept"})
			require.NoError(t, err)
			if ending == "keep-and-discard" {
				require.NoError(t, svc.Sandbox.Keep(ctx, info.BranchID))
				report, err := svc.Sandbox.Sweep(ctx, main.ProjectID)
				require.NoError(t, err)
				require.Empty(t, report.Reaped)
				require.Empty(t, report.Skipped)
				require.Error(t, svc.Branches.DeleteBranch(main.ProjectID, ending))
				require.Error(t, svc.Branches.ForceDeleteBranch(main.ProjectID, ending))
				require.Error(t, svc.Projects.DeleteProject(main.ProjectID))
				require.Error(t, svc.Checkout.Release(ctx, info.BranchID))
				_, err = tickets.InsertOne(ctx, bson.M{"_id": "kept"})
				require.NoError(t, err)
				require.NoError(t, svc.Sandbox.Discard(ctx, info.BranchID))
			} else {
				report, err := svc.Sandbox.Sweep(ctx, main.ProjectID)
				require.NoError(t, err)
				require.Equal(t, []string{ending}, report.Reaped)
				require.Empty(t, report.Skipped)
			}
			_, err = tickets.InsertOne(ctx, bson.M{"_id": "revoked"})
			var denied mongo.ServerError
			require.ErrorAs(t, err, &denied, "next write on the previously authenticated connection")
			require.True(t, denied.HasErrorCode(13), "next write must be unauthorized: %v", err)
			fresh := credentialClient(t, info.CredentialURI)
			require.ErrorContains(t, fresh.Ping(ctx, readpref.Primary()), "AuthenticationFailed")
			require.Zero(t, sandboxUserCount(t, svc.Client.Database(info.PhysicalDB)))
			dbs, err := svc.Client.ListDatabaseNames(ctx, bson.M{"name": info.PhysicalDB})
			require.NoError(t, err)
			require.Empty(t, dbs, "revoked client recreated the physical database")
			requirePasswordAbsent(t, db, password, logFile.Name())
		})
	}
}

func TestSandboxCredentialPermissionFailures(t *testing.T) {
	root, db, main := credentialProject(t)
	ctx := t.Context()
	// The engine can do ordinary checkout, release and metadata operations,
	// but has no user administration privileges.
	admin := root.Client.Database("admin")
	username := db.Name() + "_engine"
	password := rand.Text()
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: username}, {Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{"readWriteAnyDatabase", "dbAdminAnyDatabase"}},
	}).Err())
	t.Cleanup(func() {
		require.NoError(t, admin.RunCommand(context.Background(), bson.D{{Key: "dropUser", Value: username}}).Err())
	})
	u, err := url.Parse(root.MongoURI)
	require.True(t, err == nil)
	u.User = url.UserPassword(username, password)
	query := u.Query()
	query.Set("authSource", "admin")
	u.RawQuery = query.Encode()
	limited := credentialServices(t, u.String(), db.Name())

	_, err = limited.Sandbox.CreateWithCredential(ctx, main.ProjectID, main.ID, "denied", time.Hour, limited.MongoURI)
	require.ErrorContains(t, err, "userAdmin")
	require.ErrorContains(t, err, "userAdminAnyDatabase")
	_, err = root.Branches.GetBranch(main.ProjectID, "denied")
	require.Error(t, err, "partly created sandbox survived")
	var archived wal.Branch
	require.NoError(t, db.Collection("wal_branches").FindOne(ctx, bson.M{"name": "denied"}).Decode(&archived))
	physical := checkout.PhysicalDBName(archived.ID)
	names, err := root.Client.ListDatabaseNames(ctx, bson.M{"name": physical})
	require.NoError(t, err)
	require.Empty(t, names)
	require.Zero(t, sandboxUserCount(t, root.Client.Database(physical)))

	info, err := root.Sandbox.CreateWithCredential(ctx, main.ProjectID, main.ID, "blocked", time.Hour, root.MongoURI)
	require.NoError(t, err)
	past := time.Now().Add(-time.Minute)
	require.NoError(t, root.Branches.SetExpiry(info.BranchID, &past))
	report, err := limited.Sandbox.Sweep(ctx, main.ProjectID)
	require.NoError(t, err)
	require.Empty(t, report.Reaped)
	require.Len(t, report.Skipped, 1)
	require.Contains(t, report.Skipped[0], "blocked")
	require.Contains(t, strings.ToLower(report.Skipped[0]), "revok")
	// Both the database and its user must remain available for a retry.
	client := credentialClient(t, info.CredentialURI)
	require.NoError(t, client.Database(info.PhysicalDB).Collection("tickets").FindOne(ctx, bson.M{"_id": "seed"}).Err())
	report, err = root.Sandbox.Sweep(ctx, main.ProjectID)
	require.NoError(t, err)
	require.Equal(t, []string{"blocked"}, report.Reaped)
	require.Empty(t, report.Skipped)
	require.Zero(t, sandboxUserCount(t, root.Client.Database(info.PhysicalDB)))
}

func TestSandboxCredentialDropDatabaseRetainsUsers(t *testing.T) {
	client := credentialClient(t, sandboxAuthURI(t))
	db := client.Database(fmt.Sprintf("argon_drop_user_test_%d", time.Now().UnixNano()))
	ctx := t.Context()
	t.Cleanup(func() {
		require.NoError(t, db.RunCommand(context.Background(), bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err())
		require.NoError(t, db.Drop(context.Background()))
	})
	_, err := db.Collection("tickets").InsertOne(ctx, bson.M{"_id": "seed"})
	require.NoError(t, err)
	require.NoError(t, db.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "probe"}, {Key: "pwd", Value: rand.Text()}, {Key: "roles", Value: bson.A{"readWrite"}},
	}).Err())
	require.NoError(t, db.Drop(ctx))
	require.Equal(t, 1, sandboxUserCount(t, db), "dropping the data alone leaves the database user behind")
}

func TestSandboxCredentialCancelledAfterCreatingUser(t *testing.T) {
	root, db, main := credentialProject(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var created atomic.Bool
	client := electionClient(t, root.MongoURI, &event.CommandMonitor{
		Succeeded: func(_ context.Context, cmd *event.CommandSucceededEvent) {
			if cmd.CommandName == "createUser" {
				created.Store(true)
				cancel()
			}
		},
	})
	meta := client.Database(db.Name())
	f := newSnapshotFixture(t, meta)
	svc := sandbox.NewService(f.branches, checkout.NewService(client, meta, f.branches, f.mat))
	_, err := svc.CreateWithCredential(ctx, main.ProjectID, main.ID, "interrupted", time.Hour, root.MongoURI)
	require.True(t, created.Load(), "createUser never succeeded")
	require.Error(t, err)
	_, err = root.Branches.GetBranch(main.ProjectID, "interrupted")
	require.Error(t, err, "the failed create left a sandbox")
	var archived wal.Branch
	require.NoError(t, db.Collection("wal_branches").FindOne(t.Context(), bson.M{"name": "interrupted"}).Decode(&archived))
	physical := root.Client.Database(checkout.PhysicalDBName(archived.ID))
	require.Zero(t, sandboxUserCount(t, physical))
	names, err := root.Client.ListDatabaseNames(t.Context(), bson.M{"name": physical.Name()})
	require.NoError(t, err)
	require.Empty(t, names, "the failed create left its physical database")
}

func TestSandboxCredentialDiscardDuringIssuance(t *testing.T) {
	root, db, main := credentialProject(t)
	started, proceed := make(chan struct{}), make(chan struct{})
	allowCreate := sync.OnceFunc(func() { close(proceed) })
	defer allowCreate()
	client := electionClient(t, root.MongoURI, &event.CommandMonitor{
		Started: func(ctx context.Context, cmd *event.CommandStartedEvent) {
			if cmd.CommandName == "createUser" {
				close(started)
				select {
				case <-proceed:
				case <-ctx.Done():
				}
			}
		},
	})
	meta := client.Database(db.Name())
	f := newSnapshotFixture(t, meta)
	svc := sandbox.NewService(f.branches, checkout.NewService(client, meta, f.branches, f.mat))
	finished := make(chan error, 1)
	go func() {
		_, err := svc.CreateWithCredential(t.Context(), main.ProjectID, main.ID, "busy", time.Hour, root.MongoURI)
		finished <- err
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("create finished before createUser was sent: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("create never reached createUser")
	}
	branch, err := root.Branches.GetBranch(main.ProjectID, "busy")
	require.NoError(t, err)
	// While createUser is waiting to send, an independent engine must not be
	// able to drop the database and report that the sandbox has been removed.
	short, cancel := context.WithTimeout(t.Context(), time.Second)
	err = root.Sandbox.Discard(short, branch.ID)
	cancel()
	allowCreate()
	require.NoError(t, <-finished)
	require.True(t, mongo.IsTimeout(err), "concurrent discard should wait for issuance: %v", err)
	require.NoError(t, root.Sandbox.Discard(t.Context(), branch.ID))
	require.Zero(t, sandboxUserCount(t, root.Client.Database(checkout.PhysicalDBName(branch.ID))))
}

func TestSandboxCredentialExpiryFence(t *testing.T) {
	svc, _, main := credentialProject(t)
	expiry := time.Now().Truncate(time.Millisecond)
	branch, err := svc.Branches.CreateCredentialSandbox(main.ProjectID, "expiry", main.ID, expiry)
	require.NoError(t, err)
	claim := func(cutoff time.Time) error {
		return svc.Branches.BeginSandboxRevocation(t.Context(), branch, &cutoff)
	}
	require.Error(t, claim(expiry), "expiry must precede the sweep")
	justAfter := expiry.Add(time.Nanosecond)
	require.NoError(t, svc.Sandbox.Keep(t.Context(), branch.ID))
	require.Error(t, claim(justAfter), "a sweep cannot revoke a branch that was kept")
	require.NoError(t, svc.Branches.SetExpiry(branch.ID, &expiry))
	require.NoError(t, claim(justAfter), "an expiry in the same BSON millisecond already passed")
	require.Error(t, svc.Sandbox.Keep(t.Context(), branch.ID), "the sweep has already claimed this branch")
}

func TestSandboxRefusesCredentialWithoutAuthentication(t *testing.T) {
	db := setupTestDB(t)
	f, svc, mainID := newSandboxFixture(t, db, "unauth")
	_, err := svc.CreateWithCredential(t.Context(), "unauth", mainID, "refused", time.Hour, metadataDeploymentURI)
	require.ErrorContains(t, err, "does not enforce authentication")
	_, err = f.branches.GetBranch("unauth", "refused")
	require.Error(t, err)
}
