package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// argon undo --merge <plan> --project <other>: the plan belongs to another
// project. The CLI builds the plan first (onBranch "") and checks the
// project afterwards, so the refusal describes the other project's plan
// (here, that it is pending) instead of saying it is not in this project.
func TestRound2_CLIUndoMergeOtherProjectChecksProjectFirst(t *testing.T) {
	const uri = "mongodb://localhost:27017/?replicaSet=rs0"
	metadata := "argon_cli_round2_" + primitive.NewObjectID().Hex()
	t.Setenv("ARGON_METADATA_DB", metadata)
	services, err := walcli.NewServicesAt(uri, metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = services.Client.Database(metadata).Drop(context.Background()) })
	ctx := context.Background()
	a, err := services.Projects.CreateProject("r2-proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Projects.CreateProject("r2-proj-b"); err != nil {
		t.Fatal(err)
	}
	main, err := services.Branches.GetBranch(a.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Branches.CreateBranch(a.ID, "feature", main.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := services.WriterFor("r2-proj-a", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Put(ctx, "notes", bson.M{"_id": "n1"}); err != nil {
		t.Fatal(err)
	}
	feature, err := services.Branches.GetBranch(a.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := services.Merge.Preview(ctx, feature.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, err := invokeCLI(t, uri, "undo", "--merge", plan.ID.Hex(), "--project", "r2-proj-b", "--dry-run")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if strings.Contains(stderr, "pending") || !strings.Contains(stderr, "r2-proj-b") {
		t.Fatalf("the refusal describes project r2-proj-a's plan instead of naming r2-proj-b: %q", stderr)
	}
}
