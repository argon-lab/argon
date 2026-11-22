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
	"strconv"
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

		// The follower also checks for a live checkout on main. Stop it
		// before manufacturing that state so this test measures the drop
		// guards independently of when the follower next checks main.
		require.NoError(t, stop())
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

// Database drops are confined to explicit call sites. Every write or command
// in a package that handles source connections, including calls through
// helpers that take a MongoDB handle, belongs to the inventory below.
func TestSource_DestructiveCallsAreConfined(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	drops, sites, err := confinedSourceCalls(root)
	require.NoError(t, err)
	require.Equal(t, []string{
		"cli/cmd/doctor.go:probeCapture",
		"cli/cmd/doctor.go:probeCapture",
		"internal/checkout/service.go:dropPhysical",
	}, drops, "a new database or collection drop must go through checkout's guarded dropPhysical")
	require.Equal(t, []string{
		// The doctor inspects the deployment and writes to its own probe databases.
		"cli/cmd/doctor.go:inspectDeployment RunCommand",
		"cli/cmd/doctor.go:inspectDeployment RunCommand",
		"cli/cmd/doctor.go:inspectDeployment probeCapture",
		"cli/cmd/doctor.go:probeCapture Drop",
		"cli/cmd/doctor.go:probeCapture Drop",
		"cli/cmd/doctor.go:probeCapture InsertOne",
		"cli/cmd/doctor.go:probeCapture InsertOne",
		"cli/cmd/doctor.go:probeCapture InsertOne",
		"cli/cmd/doctor.go:probeCapture RunCommand",
		"cli/cmd/doctor.go:probeCapture UpdateOne",
		"cli/cmd/doctor.go:probeCapture WithTransaction",
		// Argon metadata indexes and checkout of a branch's own physical database.
		"internal/branch/wal/service.go:NewBranchService CreateMany",
		// User management targets the database derived from the branch ID.
		// connectionStatus inspects the engine's authenticated connection.
		"internal/checkout/credential.go:IssueSandboxCredential RunCommand",
		"internal/checkout/credential.go:RequireSandboxAuthentication RunCommand",
		"internal/checkout/credential.go:revokeSandboxUser RunCommand",
		"internal/checkout/service.go:EnablePrePostImages RunCommand",
		"internal/checkout/service.go:EnablePrePostImages RunCommand",
		"internal/checkout/service.go:ReadCaptureBoundary RunCommand",
		// Index builds run on the checkout's own physical database.
		"internal/checkout/service.go:buildIndexes Build",
		"internal/checkout/service.go:dropPhysical Drop",
		"internal/checkout/service.go:fill DeleteOne",
		"internal/checkout/service.go:fill ReadCaptureBoundary",
		"internal/checkout/service.go:fill UpdateOne",
		"internal/checkout/service.go:fill buildIndexes",
		"internal/checkout/service.go:fill loadCollection",
		"internal/checkout/service.go:loadCollection EnablePrePostImages",
		"internal/checkout/service.go:loadCollection InsertMany",
		"internal/checkout/service.go:loadCollection InsertMany",
		// Clears the stream position of a checkout that failed.
		"internal/checkout/service.go:removeUnfinished DeleteOne",
		// Reads the source and enables its images when requested.
		"internal/importer/online.go:DryRunOnline prepareOnline",
		"internal/importer/online.go:appendCopyBatch WithTransaction",
		"internal/importer/online.go:importOnline CatchUp",
		"internal/importer/online.go:importOnline EnableImages",
		"internal/importer/online.go:importOnline prepareOnline",
		"internal/importer/online.go:prepareOnline IdentifyAt",
		"internal/importer/online.go:prepareOnline PrimaryTime",
		"internal/importer/online.go:prepareOnline ProbeStream",
		"internal/importer/service.go:ImportDatabase DryRunOnline",
		"internal/importer/service.go:PreviewImport RunCommand",
		// createIndexes on the collection checkout passes in, never a source.
		"internal/indexspec/store.go:Build RunCommand",
		// Reads the source stream and writes Argon's metadata and WAL.
		"internal/ingest/follow.go:CatchUp followStream",
		"internal/ingest/follow.go:Follow followStream",
		"internal/ingest/follow.go:PrimaryTime primaryTime",
		"internal/ingest/follow.go:commit WithTransaction",
		"internal/ingest/follow.go:commitAndCheck primaryTime",
		"internal/ingest/follow.go:convert documentAt",
		"internal/ingest/follow.go:followStream WithTransaction",
		"internal/ingest/follow.go:followStream primaryTime",
		"internal/ingest/follow.go:primaryTime RunCommand",
		"internal/ingest/follow.go:reached primaryTime",
		"internal/ingest/follow.go:readPostImage documentAt",
		"internal/ingest/images.go:documentAt RunCommand",
		"internal/ingest/images.go:recoverImages documentAt",
		"internal/ingest/images.go:recoverImages documentAt",
		"internal/ingest/lifecycle.go:Drain DeleteOne",
		"internal/ingest/lifecycle.go:Drain InsertOne",
		"internal/ingest/lifecycle.go:report FindOneAndUpdate",
		"internal/ingest/lifecycle.go:report FindOneAndUpdate",
		"internal/ingest/resync.go:Resync resync",
		"internal/ingest/resync.go:commit WithTransaction",
		"internal/ingest/resync.go:resync WithTransaction",
		"internal/ingest/resync.go:resync followStream",
		"internal/ingest/resync.go:resync primaryTime",
		"internal/ingest/resync.go:resync probeStream",
		// Capture of an Argon branch's physical database and its status.
		"internal/ingest/service.go:ClearResumeState DeleteOne",
		"internal/ingest/service.go:convertEvent ensurePrePostImages",
		"internal/ingest/service.go:convertEvent ensurePrePostImages",
		"internal/ingest/service.go:convertEvent recoverImages",
		"internal/ingest/service.go:convertEvent recoverImages",
		"internal/ingest/service.go:ensurePrePostImages EnablePrePostImages",
		"internal/ingest/service.go:flush UpdateOne",
		"internal/ingest/service.go:flush UpdateOne",
		"internal/ingest/service.go:flush WithTransaction",
		"internal/ingest/service.go:runStream DeleteOne",
		"internal/ingest/service.go:runStream InsertOne",
		"internal/ingest/service.go:runStream UpdateOne",
		"internal/ingest/service.go:runStream convertEvent",
		"internal/ingest/service.go:runStream ensurePrePostImages",
		// Argon's wal_ingest_state.
		"internal/ingest/source_lease.go:InitSourceState InsertOne",
		"internal/ingest/source_lease.go:acquireSourceLease FindOneAndUpdate",
		"internal/ingest/source_lease.go:acquireSourceLease WithTransaction",
		"internal/ingest/source_lease.go:releaseSourceLease DeleteOne",
		"internal/ingest/source_lease.go:releaseSourceLease UpdateOne",
		"internal/ingest/source_lease.go:reportSource FindOneAndUpdate",
		"internal/ingest/source_lease.go:reportSource FindOneAndUpdate",
		"internal/ingest/source_lease.go:updateSourceLease UpdateOne",
		// Reads the source to probe its change stream.
		"internal/ingest/source_stream.go:ProbeStream probeStream",
		"internal/ingest/source_stream.go:probeStream primaryTime",
		// Argon's wal_merge_plans and writes to a branch's own physical database.
		"internal/merge/service.go:Apply UpdateOne",
		"internal/merge/service.go:Apply UpdateOne",
		"internal/merge/service.go:Apply WithTransaction",
		"internal/merge/service.go:SavePlan InsertOne",
		"internal/merge/service.go:applyPhysical DeleteOne",
		"internal/merge/service.go:applyPhysical ReplaceOne",
		// Argon's wal_merge_plans and the source write-back transaction.
		"internal/merge/writeback.go:ApplyToSource UpdateOne",
		"internal/merge/writeback.go:ApplyToSource UpdateOne",
		"internal/merge/writeback.go:ApplyToSource writeSourceTransaction",
		"internal/merge/writeback.go:EnsureIndexes CreateOne",
		"internal/merge/writeback.go:SettleWriteBack UpdateOne",
		"internal/merge/writeback.go:claimWriteBack FenceWriteBackClaim",
		"internal/merge/writeback.go:claimWriteBack UpdateOne",
		"internal/merge/writeback.go:claimWriteBack WithTransaction",
		// Applies the plan one document at a time in existing source collections.
		"internal/merge/writeback.go:writeChanges DeleteOne",
		"internal/merge/writeback.go:writeChanges FindOne", // compare the before-image
		"internal/merge/writeback.go:writeChanges FindOne", // verify the final stored image
		"internal/merge/writeback.go:writeChanges InsertOne",
		"internal/merge/writeback.go:writeChanges ReplaceOne",
		"internal/merge/writeback.go:writeSourceTransaction AbortTransaction",
		"internal/merge/writeback.go:writeSourceTransaction CommitTransaction",
		"internal/merge/writeback.go:writeSourceTransaction writeChanges",
		// Argon metadata indexes.
		"internal/pin/service.go:NewService CreateMany",
		"internal/project/wal/service.go:NewProjectService CreateMany",
		"internal/snapshot/service.go:NewService NewServiceWithStore",
		"internal/snapshot/service.go:NewServiceWithStore CreateMany",
		// IdentifyAt adds a check of the connection string to Identify.
		"internal/source/atlas.go:IdentifyAt Identify",
		"internal/source/source.go:Bind InsertOne",
		// Alters the source to enable images when requested.
		"internal/source/source.go:EnableImages RunCommand",
		// Serializes the claim with deletion of Argon's source binding.
		"internal/source/source.go:FenceWriteBackClaim UpdateOne",
		// Reads hello and replSetGetConfig on the source.
		"internal/source/source.go:Identify RunCommand",
		"internal/source/source.go:Identify RunCommand",
		// Argon's wal_sources and metadata indexes.
		"internal/source/source.go:NewRegistry CreateMany",
		"internal/source/source.go:Unbind DeleteOne",
		"internal/wal/service.go:NewService CreateMany",
		"internal/wal/service.go:NewService DropOne",
		// Runs hello, as checkout does, in doctor's own probe database.
		"pkg/walcli/capture_boundary.go:CheckCaptureBoundary ReadCaptureBoundary",
		// Connects to and initializes Argon's metadata.
		"pkg/walcli/services.go:Connect newServices",
		"pkg/walcli/services.go:NewServicesAt newServices",
		// Enables images on an Argon branch's own collection.
		"pkg/walcli/services.go:PrepareCollection EnablePrePostImages",
		// Initializes Argon's metadata collections.
		"pkg/walcli/services.go:newServices NewBranchService",
		"pkg/walcli/services.go:newServices NewProjectService",
		"pkg/walcli/services.go:newServices NewRegistry",
		"pkg/walcli/services.go:newServices NewService",
		"pkg/walcli/services.go:newServices NewService",
		"pkg/walcli/services.go:newServices NewServiceWithStore",
		// Opens and follows or resyncs a source, enabling images when requested.
		"pkg/walcli/source.go:DetachSourceWithResult WithTransaction",
		"pkg/walcli/source.go:DryRunOnlineImport DryRunOnline",
		"pkg/walcli/source.go:FollowSource EnableImages",
		"pkg/walcli/source.go:FollowSource Follow",
		"pkg/walcli/source.go:FollowSource resyncSource",
		"pkg/walcli/source.go:ResyncSource resyncSource",
		"pkg/walcli/source.go:openSource IdentifyAt",
		"pkg/walcli/source.go:resyncSource Resync",
	}, sites, "a database write or command was added to, or removed from, code that can hold a source connection; check what it is sent to, then update this list")
}

type sourceCallTarget struct {
	pkg  string
	name string
}

type sourceCallScope struct {
	site        string
	target      sourceCallTarget
	takesHandle bool
	calls       []sourceCallTarget
}

func confinedSourceCalls(root string) (drops, writes []string, err error) {
	var scopes []sourceCallScope
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		scopes = append(scopes, sourceCallScopes(filepath.ToSlash(relative), file)...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Collection and database writers, plus IndexView and SearchIndexView
	// writers. Client's transaction writes go through Session. Aggregate can
	// write through $out or $merge unless the stages are proven read-only.
	// Either RunCommand variant can write.
	methods := make(map[string]bool)
	for _, name := range strings.Fields(
		"InsertOne InsertMany UpdateOne UpdateMany UpdateByID ReplaceOne DeleteOne DeleteMany BulkWrite " +
			"FindOneAndUpdate FindOneAndReplace FindOneAndDelete Drop CreateCollection CreateView " +
			"CreateOne CreateMany DropOne DropOneWithKey DropWithKey DropAll Rename " +
			"RunCommand RunCommandCursor Aggregate WithTransaction AbortTransaction CommitTransaction",
	) {
		methods[name] = true
	}

	// A helper that can write through a handle must itself be checked at
	// each call site. Iterate so a helper can call another such helper.
	helpers := make(map[sourceCallTarget]bool)
	for changed := true; changed; {
		changed = false
		for _, scope := range scopes {
			if !scope.takesHandle || helpers[scope.target] {
				continue
			}
			for _, call := range scope.calls {
				if methods[call.name] || helpers[call] {
					helpers[scope.target] = true
					changed = true
					break
				}
			}
		}
	}

	prefixes := []string{"internal/checkout/", "internal/ingest/", "internal/importer/", "internal/merge/", "internal/source/", "pkg/walcli/", "cli/cmd/"}
	for _, scope := range scopes {
		inSourcePath := helpers[scope.target]
		for _, prefix := range prefixes {
			inSourcePath = inSourcePath || strings.HasPrefix(scope.site, prefix)
		}
		for _, call := range scope.calls {
			if call.name == "Drop" {
				drops = append(drops, scope.site)
			}
			writeBackRead := scope.site == "internal/merge/writeback.go:writeChanges" && call.name == "FindOne"
			if inSourcePath && (methods[call.name] || helpers[call] || writeBackRead) {
				writes = append(writes, scope.site+" "+call.name)
			}
		}
	}
	sort.Strings(drops)
	sort.Strings(writes)
	return drops, writes, nil
}

func sourceCallScopes(path string, file *ast.File) []sourceCallScope {
	dir := filepath.ToSlash(filepath.Dir(path))
	pkg := "github.com/argon-lab/argon/v2/" + dir
	if subdir, ok := strings.CutPrefix(dir, "cli/"); ok {
		pkg = "argon-cli/" + subdir
	}
	if subdir, ok := strings.CutPrefix(dir, "api/"); ok {
		pkg = "github.com/argon-lab/argon/api/v2/" + subdir
	}
	imports := make(map[string]string)
	for _, imported := range file.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(importPath)
		if imported.Name != nil {
			name = imported.Name.Name
		}
		imports[name] = importPath
	}

	var scopes []sourceCallScope
	add := func(target sourceCallTarget, function *ast.FuncType, node ast.Node) {
		scope := sourceCallScope{
			site:        path + ":" + target.name,
			target:      target,
			takesHandle: sourceTakesMongoHandle(function, imports),
		}
		ast.Inspect(node, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callee := ast.Unparen(call.Fun).(type) {
			case *ast.SelectorExpr:
				if callee.Sel.Name == "Aggregate" && sourceReadOnlyAggregate(call, imports) {
					return true
				}
				// Package functions use their import path. Methods are
				// tracked by name, like the MongoDB driver calls.
				var qualifier string
				if receiver, ok := callee.X.(*ast.Ident); ok {
					qualifier = imports[receiver.Name]
				}
				scope.calls = append(scope.calls, sourceCallTarget{pkg: qualifier, name: callee.Sel.Name})
			case *ast.Ident:
				scope.calls = append(scope.calls, sourceCallTarget{pkg: pkg, name: callee.Name})
			}
			return true
		})
		scopes = append(scopes, scope)
	}

	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Body == nil {
				continue
			}
			target := sourceCallTarget{pkg: pkg, name: declaration.Name.Name}
			if declaration.Recv != nil {
				target.pkg = ""
			}
			add(target, declaration.Type, declaration.Body)
		case *ast.GenDecl:
			if declaration.Tok != token.VAR {
				continue
			}
			for _, spec := range declaration.Specs {
				value := spec.(*ast.ValueSpec)
				for i, initializer := range value.Values {
					var function *ast.FuncType
					if literal, ok := ast.Unparen(initializer).(*ast.FuncLit); ok {
						function = literal.Type
					}
					add(sourceCallTarget{pkg: pkg, name: value.Names[i].Name}, function, initializer)
				}
			}
		}
	}
	return scopes
}

func sourceReadOnlyAggregate(call *ast.CallExpr, imports map[string]string) bool {
	if len(call.Args) < 2 {
		return false
	}
	pipeline, ok := ast.Unparen(call.Args[1]).(*ast.CompositeLit)
	if !ok {
		return false
	}
	selector, ok := pipeline.Type.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Pipeline" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || imports[pkg.Name] != "go.mongodb.org/mongo-driver/mongo" {
		return false
	}
	for _, expr := range pipeline.Elts {
		stage, ok := expr.(*ast.CompositeLit)
		if !ok || len(stage.Elts) != 1 {
			return false
		}
		element, ok := stage.Elts[0].(*ast.CompositeLit)
		if !ok || len(element.Elts) != 2 {
			return false
		}
		key := element.Elts[0]
		if pair, ok := key.(*ast.KeyValueExpr); ok {
			field, ok := pair.Key.(*ast.Ident)
			if !ok || field.Name != "Key" {
				return false
			}
			key = pair.Value
		}
		literal, ok := key.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return false
		}
		name, _ := strconv.Unquote(literal.Value)
		switch name {
		case "$match", "$project", "$lookup", "$set":
		default:
			return false
		}
	}
	return true
}

func sourceTakesMongoHandle(function *ast.FuncType, imports map[string]string) bool {
	if function == nil {
		return false
	}
	for _, parameter := range function.Params.List {
		handle := ast.Unparen(parameter.Type)
		if pointer, ok := handle.(*ast.StarExpr); ok {
			handle = ast.Unparen(pointer.X)
		}
		if selector, ok := handle.(*ast.SelectorExpr); ok {
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || imports[pkg.Name] != "go.mongodb.org/mongo-driver/mongo" {
				continue
			}
			switch selector.Sel.Name {
			case "Client", "Database", "Collection", "IndexView", "SearchIndexView":
				return true
			}
		}
	}
	return false
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
	_, err := f.services.WriteBackMerge(ctx, plan.ID, "", withCredentials, approvalFor(t, plan))
	require.NoError(t, err)
	f.waitCaughtUp(t)

	// A write-back that is refused stores its reason with the plan.
	second, secondDB := f.agentSandbox(t, "agent-two")
	_, err = secondDB.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-09"}, bson.M{"$set": bson.M{"stock": int32(1)}})
	require.NoError(t, err)
	refused := f.preview(t, second.BranchID)
	_, err = f.source.Collection("catalog").UpdateOne(ctx, bson.M{"_id": "item-09"}, bson.M{"$set": bson.M{"stock": int32(2)}})
	require.NoError(t, err)
	_, err = f.services.WriteBackMerge(ctx, refused.ID, "", withCredentials, approvalFor(t, refused))
	require.Error(t, err)
	clean("a refused write-back", err.Error())
	require.NotEmpty(t, f.plan(t, refused).WriteBackLastError)
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

	// A resync that cannot log in, and one that runs and stores its status.
	_, err = f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: wrongPassword})
	require.Error(t, err)
	clean("a failed resync", err.Error())
	resynced, err := f.services.ResyncSource(ctx, f.project, walcli.ResyncSourceOptions{URI: withCredentials})
	require.NoError(t, err)
	clean("the resync's report", fmt.Sprintf("%+v", resynced))
	status, err = f.services.SourceStatus(ctx, f.project)
	require.NoError(t, err)
	clean("the status report after a resync", fmt.Sprintf("%+v", status))

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
