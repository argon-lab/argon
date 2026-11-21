package wal_test

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Run the CLI against a source that requires both authentication and TLS.
// The CA belongs to the fixture and is not in the system trust store.
func TestSourceTLS(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_TLS_SOURCE_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_TLS_SOURCE_MONGODB_URI for the TLS replica set")
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal("invalid TLS fixture URI")
	}
	password, ok := u.User.Password()
	require.True(t, ok && password != "", "TLS fixture must authenticate")
	require.Equal(t, "true", u.Query().Get("tls"))
	require.NotEmpty(t, u.Query().Get("tlsCAFile"))

	const wrongPassword = "tls-wrong/p@ss:46"
	badLogin := *u
	badLogin.User = url.UserPassword(u.User.Username(), wrongPassword)
	_, wirePassword, _ := strings.Cut(u.User.String(), ":")
	_, wrongWirePassword, _ := strings.Cut(badLogin.User.String(), ":")
	checkSecrets := func(label string, body []byte) {
		t.Helper()
		for _, secret := range []string{uri, password, wirePassword, badLogin.String(), wrongPassword, wrongWirePassword} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatalf("%s contains the source password", label)
			}
		}
	}

	t.Setenv(walcli.SourceURIEnv, uri)
	require.True(t, walcli.SourceURI("") == uri && walcli.SourceURI(badLogin.String()) == badLogin.String(),
		"source URI from the environment or flag must pass through unchanged")
	engine := os.Getenv("ARGON_TEST_AUTH_ENGINE_MONGODB_URI")
	if engine == "" {
		engine = metadataDeploymentURI
	}
	f := newFollowFixtureAt(t, engine, uri)
	f.seedSmall(t)
	cli := buildSourceCLI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	logPath := filepath.Join(t.TempDir(), "driver.log")
	command := func(source string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Env = append(os.Environ(), "MONGODB_URI="+engine, "ARGON_METADATA_DB="+f.metadataName,
			walcli.SourceURIEnv+"="+source, "MONGODB_LOG_ALL=debug", "MONGODB_LOG_PATH="+logPath)
		return cmd
	}
	run := func(source string, args ...string) (string, error) {
		t.Helper()
		body, err := command(source, args...).CombinedOutput()
		checkSecrets(args[0]+" "+args[1]+" stdout/stderr", body)
		return string(body), err
	}

	// Without the private CA the same endpoint must fail certificate
	// verification. Bound server selection so this fails promptly in CI.
	noCA := *u
	q := noCA.Query()
	q.Del("tlsCAFile")
	q.Set("serverSelectionTimeoutMS", "2000")
	noCA.RawQuery = q.Encode()
	importArgs := []string{"import", "database", "--online", "--yes", "-d", f.source.Name(), "-p", f.project}
	body, err := run(noCA.String(), importArgs...)
	require.Error(t, err)
	require.Contains(t, body, "unknown authority")
	body, err = run(badLogin.String(), importArgs...)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(body), "authenticat")

	// A flag overrides even a bad environment URI, with both credentials
	// and TLS options reaching the driver. Subsequent commands use the env.
	body, err = run(badLogin.String(), append(importArgs, "--uri", uri)...)
	require.NoError(t, err, "%s", body)
	f.requireMainEqualsSource(t, 0, nil, "online import over TLS")
	plan := f.pendingSourceEdit(t, ctx)
	body, err = run(uri, "merge", "check", plan.ID.Hex())
	require.NoError(t, err, "%s", body)

	follow := command(uri, "source", "follow", "-p", f.project)
	var output bytes.Buffer
	follow.Stdout, follow.Stderr = &output, &output
	require.NoError(t, follow.Start())
	// Reap the child even if an assertion stops the test before shutdown.
	defer func() {
		if follow.ProcessState == nil {
			_ = follow.Process.Kill()
			_ = follow.Wait()
		}
		checkSecrets("source follow stdout/stderr", output.Bytes())
	}()
	require.Eventually(t, func() bool { return f.status(t).State == "running" }, 20*time.Second, 100*time.Millisecond)

	apply := []string{"merge", "apply", plan.ID.Hex(), "--write-back", "--approve", approvalFor(t, plan)}
	body, err = run(badLogin.String(), apply...)
	require.Error(t, err)
	require.Contains(t, strings.ToLower(body), "authenticat")
	body, err = run(uri, apply...)
	require.NoError(t, err, "%s", body)
	f.waitCaughtUp(t)
	f.requireMainEqualsSource(t, 0, nil, "write-back followed over TLS")
	require.Equal(t, int32(50), f.mainState(t, 0)["catalog"]["item-01"]["stock"])
	saved := f.plan(t, plan)
	require.Equal(t, merge.StatusApplied, saved.Status)
	require.NotNil(t, saved.WriteBack)
	require.NotNil(t, saved.WriteBack.SettledAt)
	require.NoError(t, follow.Process.Signal(os.Interrupt))
	require.NoError(t, follow.Wait())
	body, err = run(uri, "source", "status", "-p", f.project, "-o", "json")
	require.NoError(t, err, "%s", body)
	require.Contains(t, body, f.source.Name())
	require.NotContains(t, body, "tlsCAFile")

	logs, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NotEmpty(t, logs, "driver debug logging must be active")
	checkSecrets("MongoDB driver debug log", logs)
	metadata := f.services.Client.Database(f.metadataName)
	names, err := metadata.ListCollectionNames(ctx, bson.D{})
	require.NoError(t, err)
	require.Contains(t, names, "wal_sources")
	for _, name := range names {
		cursor, err := metadata.Collection(name).Find(ctx, bson.D{})
		require.NoError(t, err)
		for cursor.Next(ctx) {
			checkSecrets(name, cursor.Current)
		}
		require.NoError(t, cursor.Err())
		require.NoError(t, cursor.Close(ctx))
	}
}
