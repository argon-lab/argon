package wal_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/mcpserver"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func consolePlanEndpoint(t *testing.T, f *followFixture, cli string) func(method, path string, status int) []byte {
	t.Helper()
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(reservation.Addr().(*net.TCPAddr).Port)
	require.NoError(t, reservation.Close())
	cmd := exec.CommandContext(t.Context(), cli, "console", "--port", port, "--no-browser")
	cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	client := &http.Client{Timeout: 10 * time.Second}
	origin := "http://127.0.0.1:" + port
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin+"/health", nil)
		require.NoError(t, err)
		res, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	}, 15*time.Second, 50*time.Millisecond)
	return func(method, path string, status int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, origin+"/api/v1"+path, nil)
		require.NoError(t, err)
		res, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, status, res.StatusCode, "%s", body)
		return body
	}
}

func TestWriteBackApprovalSurfaces(t *testing.T) {
	ctx := t.Context()
	f := newFollowFixture(t, reproSourceURI(t))
	catalog := f.source.Collection("catalog")
	decimal := func(payload uint64) primitive.Decimal128 {
		return primitive.NewDecimal128(0x7c00000000000000, payload)
	}
	key := bson.M{"_id": "item"}
	_, err := catalog.InsertOne(ctx, bson.D{
		{Key: "_id", Value: "item"}, {Key: "n", Value: int32(7)}, {Key: "d", Value: decimal(1)},
	})
	require.NoError(t, err)
	before, err := catalog.FindOne(ctx, key).Raw()
	require.NoError(t, err)
	f.importOnline(t, walcli.OnlineImport{})
	main := f.main(t)
	edits, err := f.services.Branches.CreateBranch(main.ProjectID, "edits", main.ID)
	require.NoError(t, err)
	w, err := f.services.WriterFor(f.project, edits.Name)
	require.NoError(t, err)
	_, err = w.Put(ctx, "catalog", bson.M{"_id": "item", "n": int64(7), "d": decimal(0)})
	require.NoError(t, err)

	cli := buildSourceCLI(t)
	rest := consolePlanEndpoint(t, f, cli)
	get := func(path string) []byte { return rest(http.MethodGet, path, http.StatusOK) }
	listPath := "/merge-plans?project=" + f.project
	command := "argon merge list -p " + f.project + " --output json"
	restBody := rest(http.MethodPost, "/projects/"+f.project+"/branches/edits/merge-preview", http.StatusCreated)
	type display struct {
		ID      primitive.ObjectID
		Digest  string `json:"approval_digest"`
		Changes []struct {
			Before, Document json.RawMessage
			Exact            map[string]json.RawMessage `json:"exact_bytes"`
		}
	}
	var restPlan display
	require.NoError(t, json.Unmarshal(restBody, &restPlan))
	require.Len(t, restPlan.Changes, 1)
	require.JSONEq(t, string(restPlan.Changes[0].Before), string(restPlan.Changes[0].Document))

	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"argon_merge_preview","arguments":{"project":%q,"branch":"edits"}}}`, f.project)
	var mcpOut bytes.Buffer
	mcp := mcpserver.New(f.services, strings.NewReader(request+"\n"), &mcpOut)
	require.NoError(t, mcp.Run(ctx))
	table := runMergeCLI(t, f, cli, "merge", "preview", "-p", f.project, "-b", edits.Name)
	tableList := runMergeCLI(t, f, cli, "merge", "list", "-p", f.project)
	all, err := f.services.Merge.ListPlans(ctx, main.ProjectID)
	require.NoError(t, err)
	require.Len(t, all, 3)

	// Compare each byte stream with every digest, including the one belonging
	// to the plan created by that particular surface.
	for _, body := range [][]byte{restBody, get("/merge-plans/" + restPlan.ID.Hex()), get(listPath), mcpOut.Bytes(), table, tableList} {
		require.Contains(t, string(body), command)
		for _, p := range all {
			digest, err := merge.ApprovalDigest(p)
			require.NoError(t, err)
			require.NotContains(t, string(body), digest)
		}
	}
	refuse := func(id primitive.ObjectID, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, cli, append([]string{"merge", "apply", id.Hex()}, args...)...)
		cmd.Env = append(os.Environ(), "MONGODB_URI="+metadataDeploymentURI, "ARGON_METADATA_DB="+f.metadataName,
			"ARGON_SOURCE_URI=")
		body, err := cmd.CombinedOutput()
		require.Error(t, err)
		require.Contains(t, string(body), "--approve")
		if len(args) > 0 && args[0] == "--write-back" {
			require.Contains(t, string(body), "argon merge list")
		}
		p, err := f.services.Merge.GetPlan(ctx, id)
		require.NoError(t, err)
		digest, err := merge.ApprovalDigest(p)
		require.NoError(t, err)
		require.NotContains(t, string(body), digest)
		require.Equal(t, merge.StatusPending, p.Status)
		require.Nil(t, p.WriteBack)
		got, err := catalog.FindOne(ctx, key).Raw()
		require.NoError(t, err)
		require.Equal(t, before, got)
	}
	for _, p := range all {
		refuse(p.ID, "--write-back")
	}
	refuse(restPlan.ID, "--write-back", "--approve", "malformed")
	refuse(restPlan.ID, "--write-back", "--approve", strings.Repeat("a", 64))
	refuse(restPlan.ID, "--approve=")

	var preview struct{ Plan display }
	body := runMergeCLI(t, f, cli, "merge", "preview", "-p", f.project, "-b", edits.Name, "--output", "json")
	require.NoError(t, json.Unmarshal(body, &preview))
	var list struct{ Plans []display }
	cliList := func() []byte {
		return runMergeCLI(t, f, cli, "merge", "list", "-p", f.project, "--output", "json")
	}
	require.NoError(t, json.Unmarshal(cliList(), &list))
	require.Len(t, list.Plans, 4)
	var approved string
	for _, item := range append(list.Plans, preview.Plan) {
		p, err := f.services.Merge.GetPlan(ctx, item.ID)
		require.NoError(t, err)
		want, err := merge.ApprovalDigest(p)
		require.NoError(t, err)
		require.Equal(t, want, item.Digest)
		require.Len(t, item.Changes, 1)
		require.Contains(t, string(item.Changes[0].Before), "$numberInt")
		require.Contains(t, string(item.Changes[0].Document), "$numberLong")
		require.Contains(t, item.Changes[0].Exact, "before.d")
		if item.ID == restPlan.ID {
			approved = item.Digest
		}
	}
	require.NotEmpty(t, approved)

	// The original digest must be checked before synchronizing the source
	// branch named by a restored plan, even when that branch no longer exists.
	_, err = f.services.Client.Database(f.metadataName).Collection("wal_merge_plans").UpdateByID(ctx, preview.Plan.ID,
		bson.M{"$set": bson.M{"source_branch_id": primitive.NewObjectID().Hex()}})
	require.NoError(t, err)
	refuse(preview.Plan.ID, "--write-back", "--approve", preview.Plan.Digest, "--uri", f.sourceURI)
	result := runMergeCLI(t, f, cli, "merge", "apply", restPlan.ID.Hex(), "--write-back", "--approve", approved, "-o", "json")
	after, err := catalog.FindOne(ctx, key).Raw()
	require.NoError(t, err)
	applied, err := f.services.Merge.GetPlan(ctx, restPlan.ID)
	require.NoError(t, err)
	require.Equal(t, applied.Changes[0].DocumentRaw, after)
	require.Equal(t, approved, applied.WriteBack.ApprovedDigest)
	for _, body := range [][]byte{result, get("/merge-plans/" + restPlan.ID.Hex()), get(listPath), cliList()} {
		require.NotContains(t, string(body), approved)
	}

	// Detachment also removes approval for pending plans saved before it.
	require.NoError(t, f.services.DetachSource(ctx, f.project))
	require.NotContains(t, string(cliList()), "approval_digest")
}
