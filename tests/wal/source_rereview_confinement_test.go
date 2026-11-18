package wal_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSource_ConfinementCountsWritesThroughSourceHandles(t *testing.T) {
	const imageHelper = `package images
import driver "go.mongodb.org/mongo-driver/mongo"
func EnablePrePostImages(db *driver.Database) {
	db.RunCommand()
}
`
	const resync = `package ingest
import (
	"go.mongodb.org/mongo-driver/mongo"
	"github.com/argon-lab/argon/v2/internal/images"
)
func (s *Service) ensurePrePostImages(db *mongo.Database) {
	images.EnablePrePostImages(db)
}
func (s *Service) resync(src, state *mongo.Database) {
	state.Collection("wal_ingest_state").UpdateOne()
	%s
}
%s
`
	const apply = `package merge
func ApplyToSource(target WriteBackTarget) {
	plans.UpdateOne()
	%s
}
`

	baseline := []string{
		"internal/images/images.go:EnablePrePostImages RunCommand",
		"internal/ingest/resync.go:ensurePrePostImages EnablePrePostImages",
		"internal/ingest/resync.go:resync UpdateOne",
		"internal/merge/writeback.go:ApplyToSource UpdateOne",
	}
	for _, example := range []struct {
		name, resyncCall, resyncDecl, applyCall, extraSite string
	}{
		{name: "baseline"},
		{
			name:       "second update in resync",
			resyncCall: `src.Collection("users").UpdateOne()`,
			extraSite:  "internal/ingest/resync.go:resync UpdateOne",
		},
		{
			name:       "image changes through a helper",
			resyncCall: `s.ensurePrePostImages(src)`,
			extraSite:  "internal/ingest/resync.go:resync ensurePrePostImages",
		},
		{
			name:       "drop index by key",
			resyncCall: `src.Collection("users").Indexes().DropOneWithKey()`,
			extraSite:  "internal/ingest/resync.go:resync DropOneWithKey",
		},
		{
			name:      "second update in write-back",
			applyCall: `target.Database.Collection("audit").UpdateOne()`,
			extraSite: "internal/merge/writeback.go:ApplyToSource UpdateOne",
		},
		{
			name:       "package-level function literal",
			resyncDecl: `var setImages = func(db *mongo.Database) { images.EnablePrePostImages(db) }`,
			extraSite:  "internal/ingest/resync.go:setImages EnablePrePostImages",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			root := t.TempDir()
			for name, contents := range map[string]string{
				"internal/images/images.go":   imageHelper,
				"internal/ingest/resync.go":   fmt.Sprintf(resync, example.resyncCall, example.resyncDecl),
				"internal/merge/writeback.go": fmt.Sprintf(apply, example.applyCall),
			} {
				path := filepath.Join(root, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
			}
			drops, sites, err := confinedSourceCalls(root)
			require.NoError(t, err)
			require.Empty(t, drops)
			want := baseline
			if example.extraSite != "" {
				want = append(want, example.extraSite)
			}
			require.ElementsMatch(t, want, sites)
		})
	}
}
