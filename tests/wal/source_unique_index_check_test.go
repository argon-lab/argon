package wal_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func uniquePlanFixture(t *testing.T) (*followFixture, string) {
	t.Helper()
	f, branch := planFixture(t)
	t.Setenv("MONGODB_URI", metadataDeploymentURI)
	t.Setenv("ARGON_METADATA_DB", f.metadataName)
	t.Setenv(walcli.SourceURIEnv, f.sourceURI)
	return f, branch
}

func reportedStringID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var id string
	require.NoError(t, json.Unmarshal(raw, &id))
	return id
}

func TestWriteBack_CheckUniqueCollisions(t *testing.T) {
	f, branch := uniquePlanFixture(t)
	ctx := t.Context()
	catalog := f.source.Collection("catalog")
	_, err := catalog.UpdateByID(ctx, "item-00", bson.M{"$set": bson.M{"stock": int32(5)}})
	require.NoError(t, err)
	_, err = catalog.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "name", Value: 1}, {Key: "stock", Value: -1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "batch", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)},
	})
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.PutMany(ctx, "catalog", []bson.M{
		{"_id": "item-01", "name": "Item 0", "stock": float64(5)},
		{"_id": "added-a", "name": "A", "batch": int32(5)},
		{"_id": "added-b", "name": "B", "batch": float64(5)},
		{"_id": "added-c", "name": "C", "batch": "5"},
	})
	require.NoError(t, err)
	plan := f.preview(t, branch)
	plans := f.services.Client.Database(f.metadataName).Collection("wal_merge_plans")
	saved := func() bson.Raw {
		t.Helper()
		raw, err := plans.FindOne(ctx, bson.M{"_id": plan.ID}).Raw()
		require.NoError(t, err)
		return raw
	}
	before := saved()
	digest := approvalFor(t, plan)
	audit := f.audit(t)
	report, err := walcli.CheckMergeUnique(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Empty(t, report.NotChecked)
	require.Len(t, report.Collisions, 3)
	byIndex := map[string]merge.UniqueCollision{}
	for _, clash := range report.Collisions {
		require.Equal(t, "catalog", clash.Collection)
		byIndex[clash.Index] = clash
	}
	for _, index := range []string{"name_1", "name_1_stock_-1"} {
		require.Contains(t, byIndex, index)
		clash := byIndex[index]
		require.False(t, clash.InPlan)
		require.Equal(t, "item-01", reportedStringID(t, clash.PlanID))
		require.Equal(t, "item-00", reportedStringID(t, clash.OtherID))
	}
	require.JSONEq(t, `{"name":"Item 0"}`, string(byIndex["name_1"].Key))
	require.JSONEq(t, `{"name":"Item 0","stock":{"$numberDouble":"5.0"}}`, string(byIndex["name_1_stock_-1"].Key))
	require.Contains(t, byIndex, "batch_1")
	pair := byIndex["batch_1"]
	require.True(t, pair.InPlan)
	require.ElementsMatch(t, []string{"added-a", "added-b"}, []string{reportedStringID(t, pair.PlanID), reportedStringID(t, pair.OtherID)})

	_, err = walcli.CheckMergeUnique(ctx, plan.ID, "", metadataDeploymentURI)
	require.ErrorContains(t, err, "different deployment")
	cmd := exec.CommandContext(ctx, buildSourceCLI(t), "merge", "check", plan.ID.Hex(), "--output", "json")
	body, err := cmd.Output()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.NotZero(t, exit.ExitCode())
	var output struct {
		Collisions []map[string]any `json:"collisions"`
	}
	require.NoError(t, json.Unmarshal(body, &output), "%s, stderr: %s", body, exit.Stderr)
	require.Len(t, output.Collisions, 3)
	require.Contains(t, output.Collisions, map[string]any{
		"collection": "catalog", "index": "name_1_stock_-1",
		"key":              map[string]any{"name": "Item 0", "stock": map[string]any{"$numberDouble": "5.0"}},
		"plan_document_id": "item-01", "other_document_id": "item-00", "in_plan": false,
	})
	body, err = exec.CommandContext(ctx, buildSourceCLI(t), "merge", "check", plan.ID.Hex()).Output()
	require.ErrorAs(t, err, &exit)
	require.Contains(t, string(body), `{"name":"Item 0","stock":{"$numberDouble":"5.0"}}`)
	require.Contains(t, string(body), `"added-a" (in plan)`)
	require.Equal(t, before, saved())
	require.Equal(t, digest, approvalFor(t, f.plan(t, plan)))
	require.Empty(t, audit.operations(t))
}

func TestWriteBack_CheckUniqueConflictStrategy(t *testing.T) {
	f, branch := uniquePlanFixture(t)
	ctx := t.Context()
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.Put(ctx, "catalog", bson.M{"_id": "new-item", "name": "Item 0"})
	require.NoError(t, err)
	stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
	catalog := f.source.Collection("catalog")
	_, err = catalog.InsertOne(ctx, bson.M{"_id": "new-item", "name": "Added in production"})
	require.NoError(t, err)
	f.waitCaughtUp(t)
	require.NoError(t, stop())
	_, err = catalog.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true),
	})
	require.NoError(t, err)
	plan := f.preview(t, branch)
	require.Len(t, plan.Conflicts, 1)
	_, err = walcli.CheckMergeUnique(ctx, plan.ID, "", f.sourceURI)
	require.ErrorContains(t, err, "pass a strategy")
	ours, err := walcli.CheckMergeUnique(ctx, plan.ID, "ours", f.sourceURI)
	require.NoError(t, err)
	require.Empty(t, ours.Collisions)
	theirs, err := walcli.CheckMergeUnique(ctx, plan.ID, "theirs", f.sourceURI)
	require.NoError(t, err)
	require.Len(t, theirs.Collisions, 1)
	require.Equal(t, "new-item", reportedStringID(t, theirs.Collisions[0].PlanID))
	require.Equal(t, "item-00", reportedStringID(t, theirs.Collisions[0].OtherID))
}

func TestWriteBack_CheckUniqueVacatedAndSkipped(t *testing.T) {
	f, branch := uniquePlanFixture(t)
	ctx := t.Context()
	catalog := f.source.Collection("catalog")
	_, err := catalog.UpdateByID(ctx, "item-05", bson.M{"$set": bson.M{"sources": bson.A{bson.M{"label": "used"}}}})
	require.NoError(t, err)
	_, err = catalog.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "spare", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)},
		{Keys: bson.D{{Key: "partial", Value: 1}}, Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"active": true})},
		{Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true).SetCollation(&options.Collation{Locale: "en"}).SetName("localized")},
		{Keys: bson.D{{Key: "aliases.label", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)},
		{Keys: bson.D{{Key: "sources.label", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)},
	})
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, deleted, err := writer.Delete(ctx, "catalog", "item-00")
	require.NoError(t, err)
	require.True(t, deleted)
	_, err = writer.PutMany(ctx, "catalog", []bson.M{
		{"_id": "take-deleted", "name": "Item 0", "sources": bson.M{"label": "used"}},
		{"_id": "item-01", "name": "renamed"},
		{"_id": "take-replaced", "name": "Item 1", "aliases": bson.A{bson.M{"label": "new"}}},
		{"_id": "item-02", "name": "Item 2", "stock": 42},
	})
	require.NoError(t, err)
	plan := f.preview(t, branch)
	report, err := walcli.CheckMergeUnique(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Empty(t, report.Collisions)
	reasons := map[string]string{}
	for _, skip := range report.NotChecked {
		require.Equal(t, "catalog", skip.Collection)
		reasons[skip.Index] = strings.ToLower(skip.Reason)
	}
	require.Len(t, reasons, 4)
	require.Contains(t, reasons["partial_1"], "partial")
	require.Contains(t, reasons["localized"], "collation")
	require.Contains(t, reasons["aliases.label_1"], "array")
	require.Contains(t, reasons["sources.label_1"], "array")

	cmd := exec.CommandContext(ctx, buildSourceCLI(t), "merge", "check", plan.ID.Hex(), "--uri", f.sourceURI)
	body, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", body)
	require.Contains(t, strings.ToLower(string(body)), "not checked")
	for index := range reasons {
		require.Contains(t, string(body), index)
	}
}

func TestWriteBack_CheckUniqueMissingAndNull(t *testing.T) {
	f, branch := uniquePlanFixture(t)
	ctx := t.Context()
	catalog := f.source.Collection("catalog")
	_, err := catalog.UpdateByID(ctx, "item-00", bson.M{"$unset": bson.M{"name": ""}})
	require.NoError(t, err)
	_, err = catalog.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "region", Value: 1}, {Key: "part", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)},
	})
	require.NoError(t, err)
	writer, err := f.services.WriterFor(f.project, "edits")
	require.NoError(t, err)
	_, err = writer.PutMany(ctx, "catalog", []bson.M{
		{"_id": "missing", "region": nil},
		{"_id": "null", "name": nil, "part": nil},
		{"_id": "absent", "name": "New"},
	})
	require.NoError(t, err)
	plan := f.preview(t, branch)
	report, err := walcli.CheckMergeUnique(ctx, plan.ID, "", f.sourceURI)
	require.NoError(t, err)
	require.Empty(t, report.NotChecked)
	var sourceIDs, sharedIndexes []string
	for _, clash := range report.Collisions {
		if clash.InPlan {
			require.ElementsMatch(t, []string{"missing", "null"}, []string{reportedStringID(t, clash.PlanID), reportedStringID(t, clash.OtherID)})
			sharedIndexes = append(sharedIndexes, clash.Index)
		} else {
			require.Equal(t, "name_1", clash.Index)
			require.JSONEq(t, `{"name":null}`, string(clash.Key))
			require.Equal(t, "item-00", reportedStringID(t, clash.OtherID))
			sourceIDs = append(sourceIDs, reportedStringID(t, clash.PlanID))
		}
	}
	require.ElementsMatch(t, []string{"name_1", "region_1_part_1"}, sharedIndexes)
	require.ElementsMatch(t, []string{"null", "missing"}, sourceIDs)
}
