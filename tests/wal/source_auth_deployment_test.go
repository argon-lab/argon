package wal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type authDeployment struct {
	*followFixture
	cli string
}

const authRolePassword = "auth-fixture-password"

// Provisioning and source assertions use the fixture administrator. Each CLI
// invocation gets a fresh connection with only its named custom role.
func (a *authDeployment) role(t *testing.T, collection string, actions []string, omit string) string {
	t.Helper()
	ctx := t.Context()
	admin := a.sourceClient.Database("admin")
	id := primitive.NewObjectID().Hex()
	privileges := bson.A{}
	for _, action := range actions {
		if action == omit {
			continue
		}
		resource := bson.M{"db": a.source.Name(), "collection": collection}
		switch action {
		case "replSetGetConfig":
			resource = bson.M{"cluster": true}
		case "listCollections", "changeStream":
			resource["collection"] = ""
		case "find":
			if slices.Contains(actions, "changeStream") {
				resource["collection"] = "" // A database stream requires database-wide find.
			}
		}
		privileges = append(privileges, bson.M{"resource": resource, "actions": bson.A{action}})
	}
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "createRole", Value: id}, {Key: "privileges", Value: privileges}, {Key: "roles", Value: bson.A{}},
	}).Err())
	t.Cleanup(func() {
		require.NoError(t, admin.RunCommand(context.Background(), bson.D{{Key: "dropRole", Value: id}}).Err())
	})
	require.NoError(t, admin.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: id}, {Key: "pwd", Value: authRolePassword}, {Key: "roles", Value: bson.A{id}},
	}).Err())
	t.Cleanup(func() {
		require.NoError(t, admin.RunCommand(context.Background(), bson.D{{Key: "dropUser", Value: id}}).Err())
	})
	u, err := url.Parse(a.sourceURI)
	require.NoError(t, err)
	u.User = url.UserPassword(id, authRolePassword)
	query := u.Query()
	query.Set("authSource", "admin")
	u.RawQuery = query.Encode()
	return u.String()
}

func (a *authDeployment) command(ctx context.Context, sourceURI string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.cli, args...)
	cmd.Env = append(os.Environ(), walcli.SourceURIEnv+"="+sourceURI)
	return cmd
}

func (a *authDeployment) run(t *testing.T, sourceURI, missing string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := a.command(ctx, sourceURI, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	body := bytes.Join([][]byte{stdout, stderr.Bytes()}, []byte("\n"))
	require.NoError(t, ctx.Err(), "command timed out")
	if bytes.Contains(body, []byte(authRolePassword)) {
		t.Fatal("CLI output contained the source password")
	}
	require.NotContains(t, string(body), "panic:")
	require.NotContains(t, string(body), "goroutine ")
	if missing == "" {
		require.NoError(t, err, "%s", body)
	} else {
		require.IsType(t, &exec.ExitError{}, err, "%s", body)
		require.Contains(t, string(body), missing, "the diagnostic must name the missing MongoDB action")
	}
	if missing != "" {
		return body
	}
	return stdout
}

func (a *authDeployment) importArgs(images bool) []string {
	args := []string{"import", "database", "--online", "--yes", "--database", a.source.Name(), "--project", a.project}
	if images {
		args = append(args, "--enable-source-images")
	}
	return args
}

func (a *authDeployment) noImport(t *testing.T) {
	t.Helper()
	meta := a.services.Client.Database(a.metadataName)
	for _, name := range []string{
		"wal_projects", "wal_branches", "wal_sources", "wal_log", "wal_counters",
		"wal_ingest_state", "wal_snapshots", "wal_snapshot_chunks",
	} {
		count, err := meta.Collection(name).CountDocuments(t.Context(), bson.D{})
		require.NoError(t, err)
		require.Zero(t, count, "a denied import left rows in %s", name)
	}
}

// Run the real foreground command and shut it down through its signal handler
// after we have independently observed its effect on main.
func (a *authDeployment) followAndCompare(t *testing.T, uri string, images bool) {
	t.Helper()
	args := []string{"source", "follow", "-p", a.project}
	if images {
		args = append(args, "--enable-source-images")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := a.command(ctx, uri, args...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Start())
	waited := false
	defer func() {
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	_, err := a.source.Collection("catalog").UpdateByID(ctx, "item-00", bson.M{"$inc": bson.M{"stock": 1}})
	require.NoError(t, err)
	target := a.sourceTime(t)
	require.Eventually(t, func() bool {
		a.tick(t)
		status, err := a.services.SourceStatus(ctx, a.project)
		return err == nil && status.Owner != nil && status.Position != nil && status.Mode == "exact" &&
			!(primitive.Timestamp{T: status.Position.T, I: status.Position.I}).Before(target)
	}, 45*time.Second, 100*time.Millisecond)
	a.requireMainEqualsSource(t, 0, nil, "least-privilege follower")
	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	err = cmd.Wait()
	waited = true
	if strings.Contains(output.String(), authRolePassword) {
		t.Fatal("follower output contained the source password")
	}
	require.NoError(t, err, "%s", output.String())
	if images {
		require.True(t, reproImagesEnabledOn(t, a.followFixture, "catalog"))
	}
}

func TestSourceAuth(t *testing.T) {
	sourceURI := os.Getenv("ARGON_TEST_AUTH_SOURCE_MONGODB_URI")
	if sourceURI == "" {
		t.Skip("set ARGON_TEST_AUTH_SOURCE_MONGODB_URI to an administrator on a dedicated access-controlled source")
	}
	engineURI := os.Getenv("ARGON_TEST_AUTH_ENGINE_MONGODB_URI")
	require.NotEmpty(t, engineURI, "set ARGON_TEST_AUTH_ENGINE_MONGODB_URI to the non-root Argon engine account")
	t.Setenv("MONGODB_URI", engineURI)
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	cli := buildSourceCLI(t)
	setup := func(t *testing.T) *authDeployment {
		f := newFollowFixtureAt(t, engineURI, sourceURI)
		t.Setenv("ARGON_METADATA_DB", f.metadataName)
		var status struct {
			AuthInfo struct {
				Users []bson.M                `bson:"authenticatedUsers"`
				Roles []struct{ Role string } `bson:"authenticatedUserRoles"`
			}
		}
		require.NoError(t, f.services.Client.Database("admin").RunCommand(t.Context(), bson.D{{Key: "connectionStatus", Value: 1}}).Decode(&status))
		require.NotEmpty(t, status.AuthInfo.Users)
		require.NotEmpty(t, status.AuthInfo.Roles)
		for _, role := range status.AuthInfo.Roles {
			require.NotEqual(t, "root", role.Role, "all Argon metadata access must use the non-root engine user")
		}
		f.seedSmall(t)
		return &authDeployment{followFixture: f, cli: cli}
	}

	reads := []string{"find", "changeStream", "listCollections"}
	t.Run("read-only import and follow", func(t *testing.T) {
		a := setup(t)
		before := a.sourceState(t, nil)
		for _, missing := range reads {
			t.Run("import without "+missing, func(t *testing.T) {
				a.run(t, a.role(t, "", reads, missing), missing, a.importArgs(false)...)
				a.noImport(t)
				require.Equal(t, before, a.sourceState(t, nil))
			})
		}
		a.run(t, a.role(t, "", reads, ""), "", a.importArgs(false)...)
		main := a.main(t)
		binding, err := a.services.Sources.Get(t.Context(), main.ID)
		require.NoError(t, err)
		require.Empty(t, binding.ReplicaSetID)
		a.requireMainEqualsSource(t, 0, nil, "least-privilege import")
		follow := []string{"find", "changeStream"}
		for _, missing := range follow {
			t.Run("follow without "+missing, func(t *testing.T) {
				a.run(t, a.role(t, "", follow, missing), missing, "source", "follow", "-p", a.project)
				require.Equal(t, main.HeadLSN, a.main(t).HeadLSN)
				require.Equal(t, before, a.sourceState(t, nil))
			})
		}
		a.followAndCompare(t, a.role(t, "", follow, ""), false)
	})

	t.Run("source images", func(t *testing.T) {
		a := setup(t)
		actions := append(slices.Clone(reads), "replSetGetConfig", "collMod")
		before := a.sourceState(t, nil)
		for _, missing := range []string{"collMod", "replSetGetConfig"} {
			t.Run("import without "+missing, func(t *testing.T) {
				a.run(t, a.role(t, "catalog", actions, missing), missing, a.importArgs(true)...)
				a.noImport(t)
				require.False(t, reproImagesEnabledOn(t, a.followFixture, "catalog"))
				require.Equal(t, before, a.sourceState(t, nil))
			})
		}
		a.run(t, a.role(t, "catalog", actions, ""), "", a.importArgs(true)...)
		require.True(t, reproImagesEnabledOn(t, a.followFixture, "catalog"))
		require.NoError(t, a.source.RunCommand(t.Context(), bson.D{
			{Key: "collMod", Value: "catalog"}, {Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": false}},
		}).Err())
		for _, missing := range []string{"collMod", "listCollections", "replSetGetConfig"} {
			t.Run("follow without "+missing, func(t *testing.T) {
				a.run(t, a.role(t, "catalog", actions, missing), missing, "source", "follow", "-p", a.project, "--enable-source-images")
				require.False(t, reproImagesEnabledOn(t, a.followFixture, "catalog"))
				require.Equal(t, before, a.sourceState(t, nil))
			})
		}
		a.followAndCompare(t, a.role(t, "catalog", actions, ""), true)
	})

	t.Run("images across collections", func(t *testing.T) {
		a := setup(t)
		require.NoError(t, a.source.CreateCollection(t.Context(), "orders"))
		before := a.sourceState(t, nil)
		actions := append(slices.Clone(reads), "replSetGetConfig", "collMod")
		// Import visits catalog then orders. Only the first collMod is granted.
		body := a.run(t, a.role(t, "catalog", actions, ""), "collMod", a.importArgs(true)...)
		require.Contains(t, string(body), "orders", "identify the collection that denied collMod")
		a.noImport(t)
		require.False(t, reproImagesEnabledOn(t, a.followFixture, "orders"))
		require.Equal(t, before, a.sourceState(t, nil))
		if reproImagesEnabledOn(t, a.followFixture, "catalog") {
			require.Contains(t, string(body), "catalog and is still enabled")
			t.Log("catalog retained changeStreamPreAndPostImages after orders denied collMod; the CLI reported catalog")
		}
	})

	t.Run("write-back", func(t *testing.T) {
		a := setup(t)
		ctx := t.Context()
		_, err := a.source.Collection("catalog").Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true),
		})
		require.NoError(t, err)
		a.run(t, a.role(t, "", append(slices.Clone(reads), "replSetGetConfig"), ""), "", a.importArgs(false)...)
		main := a.main(t)
		before := a.sourceState(t, nil)
		t.Run("empty and delete-only plans", func(t *testing.T) {
			idOnly := a.role(t, "", []string{"replSetGetConfig"}, "")
			branch, err := a.services.Branches.CreateBranch(main.ProjectID, "deletions", main.ID)
			require.NoError(t, err)
			empty := a.preview(t, branch.ID)
			require.Empty(t, empty.Changes)
			a.run(t, idOnly, "", "merge", "check", empty.ID.Hex())
			a.run(t, idOnly, "", "merge", "apply", empty.ID.Hex(), "--write-back", "--approve", approvalFor(t, empty))
			require.Equal(t, merge.StatusApplied, a.plan(t, empty).Status)
			w, err := a.services.WriterFor(a.project, branch.Name)
			require.NoError(t, err)
			_, deleted, err := w.Delete(ctx, "catalog", "item-02")
			require.NoError(t, err)
			require.True(t, deleted)
			deletion := a.preview(t, branch.ID)
			require.Len(t, deletion.Changes, 1)
			a.run(t, idOnly, "", "merge", "check", deletion.ID.Hex())
			require.Equal(t, before, a.sourceState(t, nil))
		})
		edits, err := a.services.Branches.CreateBranch(main.ProjectID, "edits", main.ID)
		require.NoError(t, err)
		w, err := a.services.WriterFor(a.project, edits.Name)
		require.NoError(t, err)
		_, err = w.Put(ctx, "catalog", bson.M{"_id": "added", "name": "Added"})
		require.NoError(t, err)
		_, err = w.Put(ctx, "catalog", bson.M{"_id": "item-01", "name": "Edited"})
		require.NoError(t, err)
		_, deleted, err := w.Delete(ctx, "catalog", "item-02")
		require.NoError(t, err)
		require.True(t, deleted)
		// Preview reads only the engine, so no source credential is necessary.
		body := a.run(t, "", "", "merge", "preview", "-p", a.project, "-b", edits.Name, "--output", "json")
		var preview struct {
			Plan struct {
				ID     primitive.ObjectID
				Digest string `json:"approval_digest"`
			}
		}
		require.NoError(t, json.Unmarshal(body, &preview))
		plan, err := a.services.Merge.GetPlan(ctx, preview.Plan.ID)
		require.NoError(t, err)
		require.Len(t, plan.Changes, 3)
		require.Equal(t, approvalFor(t, plan), preview.Plan.Digest)
		stillPending := func(t *testing.T) {
			t.Helper()
			saved := a.plan(t, plan)
			require.Equal(t, merge.StatusPending, saved.Status)
			require.Nil(t, saved.WriteBack)
			require.Equal(t, before, a.sourceState(t, nil), "no partial source transaction")
		}
		check := []string{"listIndexes", "find", "replSetGetConfig"}
		for _, missing := range check {
			t.Run("check without "+missing, func(t *testing.T) {
				a.run(t, a.role(t, "catalog", check, missing), missing, "merge", "check", plan.ID.Hex())
				stillPending(t)
			})
		}
		a.run(t, a.role(t, "catalog", check, ""), "", "merge", "check", plan.ID.Hex())
		stillPending(t)
		apply := []string{"listCollections", "find", "insert", "update", "remove", "replSetGetConfig"}
		args := []string{"merge", "apply", plan.ID.Hex(), "--write-back", "--approve", preview.Plan.Digest}
		for _, missing := range apply {
			t.Run("apply without "+missing, func(t *testing.T) {
				a.run(t, a.role(t, "catalog", apply, missing), missing, args...)
				stillPending(t)
			})
		}
		a.run(t, a.role(t, "catalog", apply, ""), "", args...)
		require.Equal(t, merge.StatusApplied, a.plan(t, plan).Status)
		want, err := a.services.Materializer.MaterializeBranchAtLSN(edits, plan.SourceHead)
		require.NoError(t, err)
		require.Equal(t, want, a.sourceState(t, nil))
		follow := []string{"find", "changeStream", "replSetGetConfig"}
		a.run(t, a.role(t, "", follow, "replSetGetConfig"), "replSetGetConfig", "source", "follow", "-p", a.project)
		require.Equal(t, want, a.sourceState(t, nil))
		a.followAndCompare(t, a.role(t, "", follow, ""), false)
		require.NotNil(t, a.plan(t, plan).WriteBack.SettledAt)
	})
}
