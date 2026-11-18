package cmd

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestCLIOnlineImportPreviewAndDryRun(t *testing.T) {
	uri := os.Getenv("ARGON_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set ARGON_TEST_MONGODB_URI to run the MongoDB import checks")
	}
	id := primitive.NewObjectID().Hex()
	metadata := "argon_cli_import_meta_" + id
	t.Setenv("ARGON_METADATA_DB", metadata)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("argon_cli_import_source_" + id)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = db.Drop(cleanup)
		_ = client.Database(metadata).Drop(cleanup)
		_ = client.Disconnect(cleanup)
	})
	for _, name := range []string{"catalog", "config.flags"} {
		if _, err := db.Collection(name).InsertOne(ctx, bson.M{"_id": 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateView(ctx, "in_stock", "catalog", mongo.Pipeline{}); err != nil {
		t.Fatal(err)
	}

	previewCollections := func(flags ...string) []string {
		t.Helper()
		args := []string{"import", "preview", "--uri", uri, "--database", db.Name(), "--output", "json"}
		stdout, stderr, err := invokeCLI(t, uri, append(args, flags...)...)
		if err != nil {
			t.Fatalf("preview %v: %v %s", flags, err, stderr)
		}
		var preview ImportPreview
		if err := json.Unmarshal([]byte(stdout), &preview); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, collection := range preview.Collections {
			names = append(names, collection.Name)
		}
		slices.Sort(names)
		return names
	}
	if names := previewCollections(); !slices.Equal(names, []string{"catalog"}) {
		t.Fatalf("default preview listed %v", names)
	}
	names := previewCollections("--online")
	if !slices.Equal(names, []string{"catalog", "config.flags"}) {
		t.Fatalf("online preview listed %v", names)
	}

	stdout, stderr, err := invokeCLI(t, uri, "import", "database", "--online", "--dry-run", "--enable-source-images",
		"--uri", uri, "--database", db.Name(), "--project", "inventory", "--output", "json")
	if err != nil {
		t.Fatalf("dry-run: %v %s", err, stderr)
	}
	var result ImportResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	slices.Sort(result.Collections)
	if !result.Online || !slices.Equal(names, result.Collections) || result.ImportedDocs != 2 || result.ProjectID != "" {
		t.Fatalf("unexpected dry-run result %+v", result)
	}

	metadataCollections, err := client.Database(metadata).ListCollectionNames(ctx, bson.D{})
	if err != nil || len(metadataCollections) != 0 {
		t.Fatalf("unexpected metadata collections %v after dry-run, error %v", metadataCollections, err)
	}
	specs, err := db.ListCollectionSpecifications(ctx, bson.M{"name": bson.M{"$in": names}})
	if err != nil || len(specs) != 2 {
		t.Fatalf("unexpected source collections %v after dry-run, error %v", specs, err)
	}
	for _, spec := range specs {
		enabled, _ := spec.Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
		if enabled {
			t.Errorf("dry-run enabled source images on %q", spec.Name)
		}
	}
}
