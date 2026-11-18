package wal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSource_ConnectRefusesForeignDatabase(t *testing.T) {
	ctx := context.Background()
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	bad, err := f.services.Sandbox.Create(ctx, main.ProjectID, main.ID, "tampered", time.Hour)
	require.NoError(t, err)
	healthy, err := f.services.Sandbox.Create(ctx, main.ProjectID, main.ID, "healthy", time.Hour)
	require.NoError(t, err)

	_, err = f.services.Client.Database(f.metadataName).Collection("wal_branches").UpdateOne(ctx,
		bson.M{"_id": bad.BranchID},
		bson.M{"$set": bson.M{"physical_db": f.source.Name(), "state": "live"}})
	require.NoError(t, err)

	bin := filepath.Join(t.TempDir(), "argon")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "../../cli"
	out, err := build.CombinedOutput()
	require.NoError(t, err, "building the CLI: %s", out)
	env := append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)

	for _, output := range []string{"table", "json"} {
		t.Run(output, func(t *testing.T) {
			cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, bin, "connect", "-p", f.project, "-b", "tampered", "--output", output)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			assert.Error(t, err)
			assert.Contains(t, string(out), "refusing to use it")
			assert.NotContains(t, string(out), "/"+f.source.Name())
		})
	}

	t.Run("console sandbox list", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		console := exec.CommandContext(cmdCtx, bin, "console", "--no-browser", "--port", fmt.Sprint(port))
		console.Env = env
		require.NoError(t, console.Start())
		defer func() {
			_ = console.Process.Signal(os.Interrupt)
			_ = console.Wait()
		}()

		url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/projects/%s/sandboxes", port, f.project)
		client := &http.Client{Timeout: time.Second}
		var resp *http.Response
		require.Eventually(t, func() bool {
			resp, err = client.Get(url)
			return err == nil
		}, 30*time.Second, 100*time.Millisecond)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var body struct {
			Sandboxes []struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
				ConnectionString json.RawMessage `json:"connection_string"`
			} `json:"sandboxes"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		connections := map[string]json.RawMessage{}
		for _, sandbox := range body.Sandboxes {
			connections[sandbox.Branch.Name] = sandbox.ConnectionString
		}
		require.Contains(t, connections, "tampered")
		assert.Empty(t, connections["tampered"])
		assert.Contains(t, string(connections["healthy"]), "/"+healthy.PhysicalDB)
	})
}
