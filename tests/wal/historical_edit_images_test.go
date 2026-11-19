package wal_test

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/argon-lab/argon/v2/internal/walwriter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestSource_CheckoutRepeatedCaptureField(t *testing.T) {
	const coll, key = "users", "m"
	initial := bson.D{
		{Key: "_id", Value: key}, {Key: "z", Value: "new"}, {Key: "b", Value: int32(10)},
		{Key: "pad", Value: strings.Repeat("x", 80)}, {Key: "n", Value: bson.D{{Key: "m", Value: true}}},
	}
	// v2.3.1 captured this update after the pipeline unset z and set it
	// again. The obsolete value remained at the front of the attached image.
	moved := append(append(bson.D{}, initial...), bson.E{Key: "z", Value: "back"}, bson.E{Key: "first", Value: int32(10)})
	sorted := bson.D{
		{Key: "_id", Value: key}, {Key: "b", Value: int32(10)}, {Key: "first", Value: int32(10)},
		initial[4], initial[3], {Key: "z", Value: "back"},
	}
	for _, mode := range []string{"WAL only", "older sorted snapshot"} {
		t.Run(mode, func(t *testing.T) {
			h := newHistoryFixture(t, "repeated-capture-field")
			ctx := t.Context()
			// Small images in v2.3.1 used binary subtype 0, a zero compression
			// tag, a uint32 payload length, and the original BSON bytes.
			image := func(doc bson.D) []byte {
				raw := mustMarshalBSON(doc)
				header := binary.LittleEndian.AppendUint32([]byte{0}, uint32(len(raw)))
				return append(header, raw...)
			}
			var at int64
			for _, event := range []struct {
				op   string
				post bson.D
			}{{"insert", initial}, {"update", moved}} {
				var err error
				at, err = wal.NewSequencer(h.db).Reserve(h.project, 1)
				require.NoError(t, err)
				now := time.Now()
				row := bson.M{
					"v": 2, "lsn": at, "timestamp": now, "project_id": h.project, "branch_id": h.main.ID,
					"operation": "put", "collection": coll, "document_id": key, "key_v": 1,
					"post": image(event.post), "actor": "ingest",
					"metadata": bson.M{"capture_operation": event.op, "capture_wall_time": now},
				}
				if event.op == "update" {
					row["pre"] = image(initial)
				}
				_, err = h.db.Collection("wal_log").InsertOne(ctx, row)
				require.NoError(t, err)
			}
			require.NoError(t, h.branches.UpdateBranchHead(h.main.ID, at))
			if mode == "older sorted snapshot" {
				manifest := insertReleasedSnapshot(t, h.db, h.main, coll, at, key, sorted)
				_, err := h.db.Collection("wal_snapshots").UpdateByID(ctx, manifest, bson.M{"$set": bson.M{"exact_old_binary_keys": true}})
				require.NoError(t, err)
			}
			child, err := h.branches.CreateBranch(h.project, "sandbox", h.main.ID)
			require.NoError(t, err)
			dropPhysical(t, h.db.Client(), child.ID)
			checkouts := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
			info, err := checkouts.Checkout(ctx, child.ID)
			require.NoError(t, err)
			physical := h.db.Client().Database(info.PhysicalDB).Collection(coll)
			raw, err := physical.FindOne(ctx, bson.M{"_id": key}).Raw()
			require.NoError(t, err)
			var fields bson.D
			require.NoError(t, bson.Unmarshal(raw, &fields))
			var values []any
			for _, field := range fields {
				if field.Key == "z" {
					values = append(values, field.Value)
				}
			}
			assert.Equal(t, []any{"back"}, values)
			assert.Equal(t, mustMarshalBSON(sorted), raw, "name order after decoding the repeated field")
			current, err := physical.CountDocuments(ctx, bson.M{"z": "back"})
			require.NoError(t, err)
			assert.EqualValues(t, 1, current)
			obsolete, err := physical.CountDocuments(ctx, bson.M{"z": "new"})
			require.NoError(t, err)
			assert.Zero(t, obsolete)
		})
	}
}

func TestSource_WritesBeforeLaterLegacySnapshot(t *testing.T) {
	h := newHistoryFixture(t, "earlier-fork")
	ctx := t.Context()
	const collection, key = "items", "q"
	initial := bson.M{"_id": key, "rev": "A"}
	fork, err := h.writer.Put(ctx, collection, initial)
	require.NoError(t, err)
	require.EqualValues(t, 2, fork)
	children := make(map[string]*wal.Branch)
	for _, name := range []string{"checkout", "put", "delete"} {
		children[name], err = h.branches.CreateBranch(h.project, name, h.main.ID)
		require.NoError(t, err)
		require.Equal(t, fork, children[name].BaseLSN)
	}

	at, err := h.writer.Put(ctx, collection, bson.M{"_id": key, "rev": "B"})
	require.NoError(t, err)
	require.EqualValues(t, 6, at)
	// The v2.1.5 manifest has ranges_applied but no visibility_rules.
	replaced := bson.D{{Key: "_id", Value: key}, {Key: "rev", Value: "B"}}
	manifest := insertReleasedSnapshot(t, h.db, h.main, collection, at, key, replaced)
	_, err = h.db.Collection("wal_snapshots").UpdateByID(ctx, manifest, bson.M{"$unset": bson.M{"visibility_rules": ""}})
	require.NoError(t, err)
	floor, err := h.wal.ReclaimFloor(h.main.ID, collection)
	require.NoError(t, err)
	require.Zero(t, floor)
	rows, err := h.wal.GetDocumentHistory(h.main.ID, collection, key, fork, at)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Positive(t, row.DocumentKeyVersion)
	}
	child := children["checkout"]
	doc, err := h.mat.MaterializeDocumentAtLSN(child, collection, key, fork)
	require.NoError(t, err)
	require.Equal(t, initial, doc)
	dropPhysical(t, h.db.Client(), child.ID)
	checkouts := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
	info, err := checkouts.Checkout(ctx, child.ID)
	require.NoError(t, err)
	physical := h.db.Client().Database(info.PhysicalDB).Collection(collection)
	raw, err := physical.FindOne(ctx, bson.M{"_id": key}).Raw()
	require.NoError(t, err)
	require.Equal(t, rows[0].PostImage, raw)

	for _, op := range []string{"put", "delete"} {
		t.Run(op, func(t *testing.T) {
			child := children[op]
			writer := walwriter.New(h.wal, h.branches, h.mat, child)
			var lsn int64
			if op == "put" {
				lsn, err = writer.Put(ctx, collection, bson.M{"_id": key, "rev": "child"})
				require.NoError(t, err)
			} else {
				var deleted bool
				lsn, deleted, err = writer.Delete(ctx, collection, key)
				require.NoError(t, err)
				require.True(t, deleted)
			}
			events, err := h.wal.GetDocumentHistory(child.ID, collection, key, lsn, lsn)
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, initial, decodeEditImage(t, events[0].PreImage))
		})
	}
}
