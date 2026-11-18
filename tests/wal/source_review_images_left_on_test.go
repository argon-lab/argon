package wal_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func reproImagesOn(t *testing.T, f *followFixture) map[string]bool {
	t.Helper()
	specs, err := f.source.ListCollectionSpecifications(context.Background(), bson.D{})
	require.NoError(t, err)
	on := map[string]bool{}
	for _, spec := range specs {
		enabled, _ := spec.Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK()
		on[spec.Name] = enabled
	}
	return on
}

// The project name is reserved by another unpublished import, so BeginImport
// fails; --enable-source-images has already run collMod on the source.
func TestOnlineImport_ReservedNameLeavesTheSourceUnaltered(t *testing.T) {
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		ctx := context.Background()
		f := newFollowFixture(t, sourceURI)
		f.seedSmall(t)
		_, err := f.source.Collection("orders").InsertOne(ctx, bson.M{"_id": 1})
		require.NoError(t, err)
		for name, on := range reproImagesOn(t, f) {
			require.False(t, on, "images on before the import: %s", name)
		}

		_, err = f.services.Projects.BeginImport(ctx, f.project)
		require.NoError(t, err)

		_, err = f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
			URI: sourceURI, Database: f.source.Name(), Project: f.project, EnableSourceImages: true,
		})
		require.Error(t, err)
		t.Logf("import error: %v", err)
		after := reproImagesOn(t, f)
		t.Logf("source collections with images enabled after the FAILED import: %v", after)
		t.Logf("error mentions images/source collections: %v",
			strings.Contains(err.Error(), "image") || strings.Contains(err.Error(), "catalog"))
		for name, on := range after {
			require.False(t, on, "failed import left changeStreamPreAndPostImages enabled on source collection %s, and the error does not say so", name)
		}
	})
}

// The operator interrupts (Ctrl-C) right after the collMod ran.
func TestOnlineImport_InterruptedImageChangeIsReported(t *testing.T) {
	f := newFollowFixture(t, metadataDeploymentURI)
	f.seedSmall(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			specs, err := f.source.ListCollectionSpecifications(context.Background(), bson.D{})
			if err == nil && len(specs) > 0 {
				if on, _ := specs[0].Options.Lookup("changeStreamPreAndPostImages", "enabled").BooleanOK(); on {
					cancel()
					return
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	_, err := f.services.ImportDatabaseOnline(ctx, walcli.OnlineImport{
		URI: f.sourceURI, Database: f.source.Name(), Project: f.project, EnableSourceImages: true,
	})
	if err == nil {
		t.Skip("the import finished before the interrupt landed")
	}
	t.Logf("import error: %v", err)
	_, lookupErr := f.services.Projects.GetProjectByName(f.project)
	t.Logf("project after failure: %v", lookupErr)
	after := reproImagesOn(t, f)
	t.Logf("source collections with images enabled after the INTERRUPTED import: %v", after)
	// An interrupted collMod may already have taken effect, and Argon does
	// not undo it. What it owes the operator is to say so.
	for name, on := range after {
		if on {
			require.Contains(t, err.Error(), name, "the source was altered; the error must name the collection")
			require.Regexp(t, "may have been made|is still enabled", err.Error())
		}
	}
}
