package wal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/argon-lab/argon/v2/internal/merge"
	"github.com/argon-lab/argon/v2/pkg/walcli"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// Production replaces a document after review with one that differs only in
// field order. MongoDB treats that as a different value; write-back should
// see a conflict and write nothing.
func TestWriteBack_ReorderedFieldsAreAConflict(t *testing.T) {
	t.Setenv("ARGON_EXPERIMENTAL_SOURCE", "1")
	cases := []struct {
		name      string
		reordered bson.D
	}{
		{"nested", bson.D{{Key: "k", Value: bson.D{{Key: "b", Value: int32(2)}, {Key: "a", Value: int32(1)}}}, {Key: "n", Value: int32(1)}}},
		{"top-level", bson.D{{Key: "n", Value: int32(1)}, {Key: "k", Value: bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}}}},
	}
	forEachSourceDeployment(t, func(t *testing.T, sourceURI string) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				f := newFollowFixture(t, sourceURI)
				coll := f.source.Collection("orders")
				_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: "ord"},
					{Key: "k", Value: bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}}, {Key: "n", Value: int32(1)}})
				require.NoError(t, err)
				f.importOnline(t, walcli.OnlineImport{})
				stop := f.follow(t, f.services, walcli.FollowSourceOptions{})
				info, sandboxDB := f.agentSandbox(t, "agent-run")
				_, err = sandboxDB.Collection("orders").UpdateOne(ctx, bson.M{"_id": "ord"}, bson.M{"$set": bson.M{"n": int32(2)}})
				require.NoError(t, err)
				plan := f.preview(t, info.BranchID)
				require.Len(t, plan.Changes, 1)

				// Production writes after the review.
				res, err := coll.ReplaceOne(ctx, bson.M{"_id": "ord"}, tc.reordered)
				require.NoError(t, err)
				require.EqualValues(t, 1, res.ModifiedCount, "MongoDB itself counts the reorder as a modification")
				// The pre-review value no longer matches in the server's own equality.
				n, err := coll.CountDocuments(ctx, bson.M{"k": bson.D{{Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)}}})
				require.NoError(t, err)
				t.Logf("documents still matching k == {a:1,b:2} after production's write: %d", n)
				var prod bson.D
				require.NoError(t, coll.FindOne(ctx, bson.M{"_id": "ord"}).Decode(&prod))
				t.Logf("source before write-back: %v", prod)

				_, wbErr := f.services.WriteBackMerge(ctx, plan.ID, "", f.sourceURI, approvalFor(t, plan))
				var after bson.D
				require.NoError(t, coll.FindOne(ctx, bson.M{"_id": "ord"}).Decode(&after))
				t.Logf("write-back error: %v", wbErr)
				t.Logf("source after write-back:  %v", after)
				_ = stop()
				var stale *merge.ConflictError
				require.True(t, errors.As(wbErr, &stale), "want a conflict because the document changed since review, got %v", wbErr)
				require.Equal(t, prod, after, "the source must be untouched")
			})
		}
	})
}
