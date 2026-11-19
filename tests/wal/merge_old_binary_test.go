package wal_test

import (
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/internal/sandbox"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestWriteBack_OrdinaryMergeOldBinary(t *testing.T) {
	for _, target := range []string{"wal", "checkout"} {
		for _, mode := range []string{"linear", "diverged"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				db := setupTestDB(t)
				const project = "merge-old-binary"
				var f *snapshotFixture
				var physical *ingestFixture
				var main *wal.Branch
				var err error
				if target == "checkout" {
					physical = newIngestFixtureAt(t, project, db)
					stop := physical.startIngester(t)
					defer stop()
					f = physical.snapshotFixture
					main, err = f.branches.GetBranchByID(physical.branchID)
				} else {
					f = newSnapshotFixture(t, db)
					main, err = f.branches.CreateBranch(project, "main", "")
				}
				require.NoError(t, err)
				child, err := f.branches.CreateBranch(project, "edits", main.ID)
				require.NoError(t, err)
				const collection, id = "catalog", "packet"
				writer := walwriter.New(f.wal, f.branches, f.mat, child)
				// The four actual zero bytes distinguish a previously inflated
				// stored value from an empty payload in an existing WAL.
				payloads := map[string][]byte{"empty": {}, "four": {0, 0, 0, 0}, "data": {8, 9, 10}}
				doc := bson.M{"_id": id}
				for field, payload := range payloads {
					doc[field] = primitive.Binary{Subtype: 2, Data: payload}
				}
				lsn, err := writer.Put(ctx, collection, doc)
				require.NoError(t, err)
				original, err := f.wal.GetEntry(project, lsn)
				require.NoError(t, err)
				writes := 0
				strategy := ""
				if mode == "diverged" {
					strategy = merge.StrategyTheirs
					if physical == nil {
						_, err = walwriter.New(f.wal, f.branches, f.mat, main).Put(ctx, collection, bson.M{"_id": id})
					} else {
						_, err = physical.physical.Collection(collection).InsertOne(ctx, bson.M{"_id": id})
						writes++
					}
					require.NoError(t, err)
					if physical != nil {
						physical.waitForEntries(t, collection, writes)
					}
				}
				service := merge.NewService(db, f.wal, f.branches, f.mat, db.Client())
				p, err := service.Preview(ctx, child.ID)
				require.NoError(t, err)
				if strategy != "" {
					require.Len(t, p.Conflicts, 1)
					require.Equal(t, doc, p.Conflicts[0].Theirs)
				} else {
					require.Len(t, p.Changes, 1)
					require.Equal(t, doc, p.Changes[0].Document)
				}
				result, err := service.Apply(ctx, p.ID, strategy)
				require.NoError(t, err)
				require.Equal(t, 1, result.Applied)
				images := []bson.Raw{original.PostImage}
				if physical != nil {
					physical.waitForEntries(t, collection, writes+1)
					stored, err := physical.physical.Collection(collection).FindOne(ctx, bson.M{"_id": id}).Raw()
					require.NoError(t, err)
					images = append(images, stored)
				}
				main, err = f.branches.GetBranchByID(main.ID)
				require.NoError(t, err)
				rows, err := f.wal.GetDocumentHistory(main.ID, collection, id, p.TargetHead+1, main.HeadLSN)
				require.NoError(t, err)
				require.Len(t, rows, 1)
				images = append(images, rows[0].PostImage)
				state, err := f.matFull.MaterializeCollection(main, collection)
				require.NoError(t, err)
				require.Equal(t, doc, state[id])
				for _, raw := range images {
					var decoded bson.M
					require.NoError(t, wal.UnmarshalDocument(raw, &decoded))
					require.Equal(t, doc, decoded)
					for field, payload := range payloads {
						subtype, data := raw.Lookup(field).Binary()
						require.Equal(t, byte(2), subtype)
						require.Equal(t, payload, data)
					}
				}
				if target == "wal" && mode == "linear" {
					checkBinarySnapshotAndRestore(t, db, f, main, collection, id, doc)
				}
			})
		}
	}
}

func checkBinarySnapshotAndRestore(t *testing.T, db *mongo.Database, f *snapshotFixture, main *wal.Branch, collection, id string, doc bson.M) {
	t.Helper()
	ctx := t.Context()
	snaps, err := f.snapshots.CreateSnapshot(ctx, main.ID, main.HeadLSN)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	restored, err := f.restore.CreateBranchAtLSN(main.ProjectID, main.ID, "restored", main.HeadLSN)
	require.NoError(t, err)
	state, err := f.timeTravel.MaterializeAtLSN(restored, collection, restored.HeadLSN)
	require.NoError(t, err)
	require.Equal(t, doc, state[id])

	boxes := sandbox.NewService(f.branches, checkout.NewService(db.Client(), db, f.branches, f.mat))
	info, err := boxes.Adopt(ctx, restored.ID, sandbox.DefaultTTL)
	require.NoError(t, err)
	defer func() { require.NoError(t, boxes.Discard(ctx, restored.ID)) }()
	raw, err := db.Client().Database(info.PhysicalDB).Collection(collection).FindOne(ctx, bson.M{"_id": id}).Raw()
	require.NoError(t, err)
	var decoded bson.M
	require.NoError(t, wal.UnmarshalDocument(raw, &decoded))
	require.Equal(t, doc, decoded)
}
