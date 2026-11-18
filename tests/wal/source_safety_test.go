package wal_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// sourceAudit records every change the source deployment makes to the source
// database, including collection-level ones, from the moment it is opened.
type sourceAudit struct {
	stream *mongo.ChangeStream
}

func (f *followFixture) audit(t *testing.T) *sourceAudit {
	t.Helper()
	stream, err := f.source.Watch(context.Background(), mongo.Pipeline{},
		options.ChangeStream().SetShowExpandedEvents(true).SetMaxAwaitTime(300*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stream.Close(context.Background()) })
	return &sourceAudit{stream: stream}
}

// operations drains the audit and counts events by type and collection.
func (a *sourceAudit) operations(t *testing.T) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := make(map[string]int)
	for idle := 0; idle < 3 && ctx.Err() == nil; {
		if !a.stream.TryNext(ctx) {
			require.NoError(t, a.stream.Err())
			idle++
			continue
		}
		idle = 0
		var event struct {
			OperationType string `bson:"operationType"`
			NS            struct {
				Collection string `bson:"coll"`
			} `bson:"ns"`
		}
		require.NoError(t, bson.Unmarshal(a.stream.Current, &event))
		seen[event.OperationType+" "+event.NS.Collection]++
	}
	return seen
}

// Every Argon path that drops a database is run against a project that
// follows a source, with the branch records pointing where they should and
// then tampered with to point at the source. The source's own change stream
// is the witness: it must show no drop, no rename and no write that the test
// did not make itself.
func TestSource_NoArgonPathDropsOrRewritesIt(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		audit := f.audit(t)
		services := f.services
		metadata := services.Client.Database(f.metadataName)
		expected := f.sourceState(t, nil)
		intact := func(label string) {
			t.Helper()
			names, err := f.source.ListCollectionNames(ctx, bson.D{})
			require.NoError(t, err)
			require.Equal(t, []string{"catalog"}, names, "%s: the source's collections", label)
			require.Equal(t, expected, f.sourceState(t, nil), "%s: the source's documents", label)
		}

		// An import that fails after it reserved its project and bound the
		// source: the automatic cleanup removes Argon's rows only.
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := services.ImportDatabaseOnline(canceled, walcli.OnlineImport{URI: sourceURI, Database: f.source.Name(), Project: f.project})
		require.Error(t, err)
		intact("canceled import")

		// A crashed import, recovered with the explicit cleanup command.
		crashed, err := services.Projects.BeginImport(ctx, "crashed-"+f.project)
		require.NoError(t, err)
		require.NoError(t, services.Branches.SetFollows(ctx, crashed.MainBranchID, f.source.Name()))
		require.NoError(t, services.Sources.Bind(ctx, &source.Binding{BranchID: crashed.MainBranchID, ProjectID: crashed.ID, Database: f.source.Name(), SetName: "rs"}))
		require.NoError(t, services.Projects.CleanupImport(ctx, "crashed-"+f.project))
		leftover, err := metadata.Collection("wal_sources").CountDocuments(ctx, bson.M{"project_id": crashed.ID})
		require.NoError(t, err)
		require.Zero(t, leftover, "cleanup removes the binding it left behind")
		intact("import cleanup")

		// The real import, then the whole sandbox lifecycle on top of it.
		f.importOnline(t, walcli.OnlineImport{})
		stop := f.follow(t, services, walcli.FollowSourceOptions{})
		main := f.main(t)

		_, err = services.Checkout.Checkout(ctx, main.ID)
		require.ErrorContains(t, err, "cannot be checked out", "the refresh path of checkout never runs for a followed branch")
		require.NoError(t, services.Checkout.Release(ctx, main.ID), "releasing a branch with no physical database drops nothing")
		intact("checkout and release of main")

		discarded, _ := f.agentSandbox(t, "to-discard")
		require.NoError(t, services.Sandbox.Discard(ctx, discarded.BranchID))
		expired, _ := f.agentSandbox(t, "to-expire")
		past := time.Now().Add(-time.Minute)
		require.NoError(t, services.Branches.SetExpiry(expired.BranchID, &past))
		report, err := services.Sandbox.Sweep(ctx, main.ProjectID)
		require.NoError(t, err)
		require.Equal(t, []string{"to-expire"}, report.Reaped)
		released, _ := f.agentSandbox(t, "to-release")
		require.NoError(t, services.Checkout.Release(ctx, released.BranchID))
		_, err = services.Checkout.Checkout(ctx, released.BranchID)
		require.NoError(t, err, "checking a branch out again rebuilds its own database")
		intact("sandbox discard, sweep, release and refresh")

		// Now the branch records lie. A sandbox claims the source database
		// as its physical database; so does main. Every path that would
		// drop "the branch's database" must refuse, and must leave the
		// source alone.
		branches := metadata.Collection("wal_branches")
		victim, _ := f.agentSandbox(t, "tampered")
		claim := func(branchID string) {
			_, err := branches.UpdateOne(ctx, bson.M{"_id": branchID},
				bson.M{"$set": bson.M{"physical_db": f.source.Name(), "state": "live"}})
			require.NoError(t, err)
		}
		claim(victim.BranchID)
		require.ErrorContains(t, services.Checkout.Release(ctx, victim.BranchID), "refusing to drop")
		intact("release of a branch record naming the source")
		require.ErrorContains(t, services.Sandbox.Discard(ctx, victim.BranchID), "refusing to drop")
		intact("discard of a branch record naming the source")
		require.NoError(t, services.Branches.SetExpiry(victim.BranchID, &past))
		report, err = services.Sandbox.Sweep(ctx, main.ProjectID)
		require.NoError(t, err)
		require.Empty(t, report.Reaped)
		require.Len(t, report.Skipped, 1, "the sweep reports what it would not drop")
		require.Contains(t, report.Skipped[0], "refusing to drop")
		intact("sweep of a branch record naming the source")

		claim(main.ID)
		require.ErrorContains(t, services.Checkout.Release(ctx, main.ID), "refusing to drop")
		require.ErrorContains(t, services.Checkout.Discard(ctx, main.ID), "refusing to drop")
		_, err = branches.UpdateOne(ctx, bson.M{"_id": main.ID}, bson.M{"$unset": bson.M{"physical_db": "", "state": ""}})
		require.NoError(t, err)
		intact("release and discard of main naming the source")

		// The registry's own check, which checkout consults before every
		// drop, refuses the source and anything not created by Argon.
		require.ErrorContains(t, services.Sources.GuardDrop(ctx, f.source.Name()), "refusing to drop")
		require.ErrorContains(t, services.Sources.GuardDrop(ctx, "production"), "refusing to drop")

		// Detaching ends the tie; it does not touch the source either.
		require.NoError(t, stop())
		require.NoError(t, services.DetachSource(ctx, f.project))
		info, err := services.Checkout.Checkout(ctx, main.ID)
		require.NoError(t, err, "a detached main is an ordinary branch again")
		require.Equal(t, "argon_br_"+main.ID, info.PhysicalDB)
		require.NoError(t, services.Checkout.Release(ctx, main.ID))
		intact("detach, checkout and release")

		// The witness. Everything Argon did above is absent from it.
		require.Empty(t, audit.operations(t), "Argon made no change of any kind to the source database")
	})
}

// A source can never be a database Argon itself creates and drops, nor the
// metadata database, so the names can never collide.
func TestSource_ManagedDatabasesCannotBeFollowed(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	for _, name := range []string{"argon_br_6afd13a0894eacff7fdeab7d", f.metadataName, "admin", "local", "config"} {
		_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: f.sourceURI, Database: name, Project: f.project})
		require.ErrorContains(t, err, "cannot be followed as a source", name)
	}
	projects, err := f.services.Projects.ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects)
}

// Database drops are confined to one guarded function, and the code that
// talks to a source issues no collection-level or multi-document write.
func TestSource_DestructiveCallsAreConfined(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	calls := make(map[string][]string) // method -> "file:function"
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "web", "tests", "examples", "compat", "npm", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					calls[selector.Sel.Name] = append(calls[selector.Sel.Name], filepath.ToSlash(relative)+":"+function.Name.Name)
				}
				return true
			})
		}
		return nil
	}))
	unique := func(sites []string) []string {
		set := make(map[string]bool)
		for _, site := range sites {
			set[site] = true
		}
		out := make([]string, 0, len(set))
		for site := range set {
			out = append(out, site)
		}
		sort.Strings(out)
		return out
	}

	// Drop: checkout's guarded function, and doctor's own probe database.
	require.Equal(t, []string{
		"cli/cmd/doctor.go:probeCapture",
		"internal/checkout/service.go:dropPhysical",
	}, unique(calls["Drop"]), "a new database or collection drop must go through checkout's guarded dropPhysical")

	// Code that holds a connection to a source.
	sourceFacing := map[string]bool{
		"internal/ingest/follow.go":   true,
		"internal/importer/online.go": true,
		"internal/merge/writeback.go": true,
		"internal/source/source.go":   true,
		"pkg/walcli/source.go":        true,
	}
	for _, method := range []string{"Drop", "DeleteMany", "UpdateMany", "InsertMany", "BulkWrite", "Rename", "CreateCollection", "CreateView", "CreateOne", "CreateMany", "DropOne", "DropAll"} {
		for _, site := range unique(calls[method]) {
			file := strings.SplitN(site, ":", 2)[0]
			if file == "internal/source/source.go" && site == "internal/source/source.go:NewRegistry" {
				continue // indexes on Argon's own wal_sources collection
			}
			if file == "internal/merge/writeback.go" && site == "internal/merge/writeback.go:EnsureIndexes" {
				continue // an index on Argon's own wal_merge_plans collection
			}
			require.False(t, sourceFacing[file], "%s calls %s; code that reaches a source database may only read it or write single documents", site, method)
		}
	}
	// The single-document writes to a source all live in the write-back
	// transaction body.
	for _, method := range []string{"DeleteOne", "ReplaceOne"} {
		for _, site := range unique(calls[method]) {
			file := strings.SplitN(site, ":", 2)[0]
			if sourceFacing[file] && file != "pkg/walcli/source.go" {
				require.Contains(t, []string{"internal/merge/writeback.go:writeChanges", "internal/source/source.go:Unbind"}, site)
			}
		}
	}
}

// The source connection string is used and forgotten. Its credentials are in
// no error, no status report and no document Argon stores.
func TestSource_CredentialsAreNotStoredOrReported(t *testing.T) {
	ctx := context.Background()
	const user, password = "argon_follow_test", "s3cr3t/p@ss:w0rd#17"
	plain := newFollowFixture(t, metadataDeploymentURI)
	admin := plain.sourceClient.Database("admin")
	_ = admin.RunCommand(ctx, bson.D{{Key: "dropUser", Value: user}}).Err()
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: user}, {Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{bson.M{"role": "readWriteAnyDatabase", "db": "admin"}, bson.M{"role": "clusterMonitor", "db": "admin"}}},
	}).Err())
	t.Cleanup(func() { _ = admin.RunCommand(context.Background(), bson.D{{Key: "dropUser", Value: user}}).Err() })

	withCredentials := fmt.Sprintf("mongodb://%s:%s@localhost:27017/?authSource=admin", user, url.QueryEscape(password))
	f := newFollowFixture(t, withCredentials)
	secrets := []string{password, url.QueryEscape(password), url.PathEscape(password), user + ":"}
	clean := func(label, text string) {
		t.Helper()
		for _, secret := range secrets {
			require.NotContains(t, text, secret, "%s leaks the connection string's credentials", label)
		}
	}

	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	info, sandboxDB := f.agentSandbox(t, "agent-run")
	agentEdits(t, sandboxDB)
	plan := f.preview(t, info.BranchID)
	_, err := f.services.WriteBackMerge(ctx, plan.ID, "", withCredentials)
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())

	// Failures, each reached with the credentials in hand.
	wrongPassword := strings.Replace(withCredentials, url.QueryEscape(password), url.QueryEscape(password+"-wrong"), 1)
	secrets = append(secrets, password+"-wrong")
	err = f.services.FollowSource(ctx, f.project, walcli.FollowSourceOptions{URI: wrongPassword})
	require.Error(t, err)
	clean("a failed login", err.Error())
	_, err = f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: wrongPassword, Database: f.source.Name(), Project: "other-" + f.project})
	require.Error(t, err)
	clean("a failed import", err.Error())
	_, err = f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: "mongodb://" + user + ":" + password + "@", Database: f.source.Name(), Project: "other-" + f.project})
	require.Error(t, err)
	clean("an unparseable connection string", err.Error())
	_, err = f.services.ImportPreview(ctx, wrongPassword, f.source.Name())
	require.Error(t, err)
	clean("a failed preview", err.Error())

	// A follower that ends degraded stores its error for status to show.
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	_, err = f.source.Collection("scratch").InsertOne(ctx, bson.M{"_id": 1})
	require.NoError(t, err)
	require.NoError(t, f.source.Collection("scratch").Drop(ctx))
	clean("a degraded follower", f.followUntilItStops(t, walcli.FollowSourceOptions{}).Error())
	status, err := f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	require.Equal(t, "degraded", status.State)
	clean("the status report", fmt.Sprintf("%+v", status))
	require.Equal(t, "mongodb://localhost:27017/"+f.source.Name(), status.Location)

	// Nothing Argon keeps contains them.
	metadata := f.services.Client.Database(f.metadataName)
	collections, err := metadata.ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	require.Contains(t, collections, "wal_sources")
	for _, name := range collections {
		cursor, err := metadata.Collection(name).Find(ctx, bson.D{})
		require.NoError(t, err)
		for cursor.Next(ctx) {
			clean("metadata collection "+name, cursor.Current.String())
		}
		require.NoError(t, cursor.Err())
	}
}

// Redaction is applied to text the driver produced, whatever form the
// credentials took in the connection string.
func TestSource_RedactAndDisplay(t *testing.T) {
	uri := "mongodb+srv://app%40corp:p%40ss%2Fword@cluster0.example.mongodb.net/?retryWrites=true&authMechanismProperties=AWS_SESSION_TOKEN:abcdefghijklmnop"
	for _, text := range []string{
		"server selection error for " + uri,
		"auth failed for app%40corp:p%40ss%2Fword",
		"password p@ss/word rejected",
		"token AWS_SESSION_TOKEN:abcdefghijklmnop expired",
	} {
		cleaned := source.Redact(text, uri)
		for _, secret := range []string{"p%40ss%2Fword", "p@ss/word", "abcdefghijklmnop", "app%40corp:"} {
			require.NotContains(t, cleaned, secret)
		}
		require.Contains(t, cleaned, "[redacted]")
	}
	require.Equal(t, "nothing secret here", source.Redact("nothing secret here", uri))
	require.Equal(t, "mongodb+srv://cluster0.example.mongodb.net/shop", source.Display(uri, "shop"))
	require.Equal(t, "mongodb://a:27017,b:27017/shop", source.Display("mongodb://u:p@a:27017,b:27017/other?replicaSet=rs0", "shop"))
	require.NotContains(t, source.Display("not a uri with secret", "shop"), "secret")

	_, err := source.Connect(context.Background(), "mongodb://u:hunter2@")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "hunter2")
}
