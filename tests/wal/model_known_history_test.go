package wal_test

import (
	"reflect"
	"testing"

	"github.com/argon-lab/argon/v2/internal/checkout"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// This table is the complete list of known exceptions. Each case checks
// collection replay and allows just one specific incorrect point result.
// An error, a different value, or a matching failure during an ordinary
// generated sequence still fails. A fixed implementation also passes.
func TestCorrectnessKnownHistory(t *testing.T) {
	const q = "q"
	a := bson.M{"_id": q, "v": "A"}
	za := bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: int32(2)}}
	az := bson.D{za[1], za[0]}
	for _, tc := range []struct {
		name      string
		issue     int
		condition string
		id        any
		envelope  string
		known     bson.M
	}{
		{"154/opaque", 154, "untyped put with opaque envelope, no indexed prior revision", q, "opaque-envelope", nil},
		{"154/compound", 154, "untyped put with reversed compound-ID envelope, no indexed prior revision", za, wal.LegacyDocumentIDString(az), nil},
		{"155", 155, "covered revision removed, older row survives at or below recorded floor", q, "", a},
		{"168", 168, "covered revision removed, older row survives without a recorded floor", q, "", a},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHistoryFixture(t, "known-history")
			ctx := t.Context()
			b := bson.M{"_id": tc.id, "v": "B"}
			if tc.issue != 154 {
				_, err := h.writer.Put(ctx, suiteCollection, a)
				require.NoError(t, err)
			}
			at, err := h.writer.Put(ctx, suiteCollection, b)
			require.NoError(t, err)
			if tc.issue == 154 {
				require.NoError(t, rewriteSuiteLegacyKey(h, h.main.ID, at, tc.envelope))
			} else {
				_, err = h.snapshots.CreateSnapshot(ctx, h.main.ID, at)
				require.NoError(t, err)
				if tc.issue == 155 {
					require.NoError(t, h.wal.RecordReclaimFloor(ctx, h.main.ID, suiteCollection, at))
				}
				res, err := h.db.Collection("wal_log").DeleteOne(ctx, bson.M{"branch_id": h.main.ID, "lsn": at})
				require.NoError(t, err)
				require.EqualValues(t, 1, res.DeletedCount)
			}
			branch := h.reload()
			full, err := h.mat.MaterializeCollection(branch, suiteCollection)
			require.NoError(t, err)
			key := wal.DocumentIDString(tc.id)
			require.Equal(t, map[string]bson.M{key: b}, full)
			point, err := h.mat.MaterializeDocument(branch, suiteCollection, key)
			require.NoError(t, err)
			if tc.issue == 154 {
				dropPhysical(t, h.db.Client(), branch.ID)
				checkouts := checkout.NewService(h.db.Client(), h.db, h.branches, h.mat)
				info, err := checkouts.Checkout(ctx, branch.ID)
				require.NoError(t, err)
				require.EqualValues(t, 1, info.Documents)
				raw, err := h.db.Client().Database(info.PhysicalDB).Collection(suiteCollection).FindOne(ctx, bson.M{"_id": tc.id}).Raw()
				require.NoError(t, err)
				want, err := bson.Marshal(bson.D{{Key: "_id", Value: tc.id}, {Key: "v", Value: "B"}})
				require.NoError(t, err)
				require.Equal(t, bson.Raw(want), raw)
			}
			if reflect.DeepEqual(point, b) {
				return
			}
			require.Equal(t, tc.known, point, "%s: unexpected point-read divergence", tc.condition)
			t.Logf("known #%d (%s), LSN %d: want %#v, got %#v", tc.issue, tc.condition, at, b, point)
		})
	}
}
