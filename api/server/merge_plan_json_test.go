package server

import (
	"context"
	"math"
	"net/http"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestWriteBackRESTPlanNonFiniteNumbers(t *testing.T) {
	ctx := t.Context()
	metadata := "argon_api_numbers_" + primitive.NewObjectID().Hex()
	svc, err := walcli.NewServicesAt("mongodb://localhost:27017", metadata)
	require.NoError(t, err)
	source := svc.Client.Database(metadata + "_source")
	t.Cleanup(func() {
		_ = source.Drop(context.Background())
		_ = svc.Client.Database(metadata).Drop(context.Background())
	})
	_, err = source.Collection("items").InsertOne(ctx, bson.M{"_id": "a", "value": math.Inf(-1)})
	require.NoError(t, err)
	_, err = svc.ImportDatabaseOnline(ctx, walcli.OnlineImport{URI: "mongodb://localhost:27017", Database: source.Name(), Project: "numbers"})
	require.NoError(t, err)
	main, err := svc.Branches.GetBranch(mustProjectID(t, svc, "numbers"), "main")
	require.NoError(t, err)
	branch, err := svc.Branches.CreateBranch(main.ProjectID, "edits", main.ID)
	require.NoError(t, err)
	writer, err := svc.WriterFor("numbers", branch.Name)
	require.NoError(t, err)
	_, err = writer.Put(ctx, "items", bson.M{"_id": "a", "value": bson.A{math.NaN(), bson.M{"max": math.Inf(1)}, 1.25}})
	require.NoError(t, err)
	router := NewRouter(svc)
	t.Cleanup(router.Shutdown)
	prefix := "/api/v1/projects/numbers/branches/edits/"
	check := func(t *testing.T, plan map[string]any) {
		t.Helper()
		require.NotContains(t, plan, "approval_digest")
		require.Len(t, plan["changes"], 1)
		change := plan["changes"].([]any)[0].(map[string]any)
		require.Equal(t, map[string]any{"$numberDouble": "-Infinity"}, change["before"].(map[string]any)["value"])
		require.Equal(t, []any{map[string]any{"$numberDouble": "NaN"}, map[string]any{"max": map[string]any{"$numberDouble": "Infinity"}}, 1.25}, change["document"].(map[string]any)["value"])
	}
	t.Run("diff", func(t *testing.T) {
		code, body := do(t, router, http.MethodGet, prefix+"diff", nil)
		require.Equal(t, http.StatusOK, code)
		check(t, body)
	})
	code, preview := do(t, router, http.MethodPost, prefix+"merge-preview", nil)
	require.Equal(t, http.StatusCreated, code)
	t.Run("preview", func(t *testing.T) { check(t, preview) })
	plans, err := svc.Merge.ListPlans(ctx, main.ProjectID)
	require.NoError(t, err)
	require.Len(t, plans, 1)
	t.Run("get", func(t *testing.T) {
		code, body := do(t, router, http.MethodGet, "/api/v1/merge-plans/"+plans[0].ID.Hex(), nil)
		require.Equal(t, http.StatusOK, code)
		check(t, body)
	})
	t.Run("list", func(t *testing.T) {
		code, body := do(t, router, http.MethodGet, "/api/v1/merge-plans?project=numbers", nil)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, body["plans"], 1)
		check(t, body["plans"].([]any)[0].(map[string]any))
	})
	t.Run("encoding error", func(t *testing.T) {
		_, err := writer.Put(ctx, "items", bson.M{"_id": "bad-date", "date": primitive.DateTime(math.MaxInt64)})
		require.NoError(t, err)
		code, body := do(t, router, http.MethodPost, prefix+"merge-preview", nil)
		require.Equal(t, http.StatusInternalServerError, code)
		require.Contains(t, body["error"], "argon merge list --output json")
	})
}
