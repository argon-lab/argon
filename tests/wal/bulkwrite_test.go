package wal_test

import (
	"context"
	"errors"
	"testing"

	driverwal "github.com/argon-lab/argon/internal/driver/wal"
	"github.com/argon-lab/argon/internal/materializer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestInterceptor_BulkWrite(t *testing.T) {
	db := setupTestDB(t)
	_, _, _, interceptor := newInterceptorFixture(t, db, "bulk-project", "main")
	ctx := context.Background()

	t.Run("Mixed models accumulate real counts", func(t *testing.T) {
		upsert := true
		models := []mongo.WriteModel{
			&mongo.InsertOneModel{Document: bson.M{"_id": "b1", "n": int32(1), "team": "red"}},
			&mongo.InsertOneModel{Document: bson.M{"_id": "b2", "n": int32(2), "team": "red"}},
			&mongo.InsertOneModel{Document: bson.M{"_id": "b3", "n": int32(3), "team": "blue"}},
			&mongo.UpdateOneModel{
				Filter: bson.M{"_id": "b1"},
				Update: bson.M{"$inc": bson.M{"n": int32(10)}},
			},
			&mongo.UpdateManyModel{
				Filter: bson.M{"team": "red"},
				Update: bson.M{"$set": bson.M{"seen": true}},
			},
			&mongo.ReplaceOneModel{
				Filter:      bson.M{"_id": "b3"},
				Replacement: bson.M{"replaced": true},
			},
			&mongo.UpdateOneModel{
				Filter: bson.M{"_id": "b4"},
				Update: bson.M{"$set": bson.M{"n": int32(4)}},
				Upsert: &upsert,
			},
			&mongo.DeleteOneModel{Filter: bson.M{"_id": "b2"}},
		}

		result, err := interceptor.BulkWrite(ctx, "bulk", models, true)
		require.NoError(t, err)

		assert.Equal(t, int64(3), result.InsertedCount)
		assert.Equal(t, int64(4), result.MatchedCount, "b1 + two reds + b3 replace")
		assert.Equal(t, int64(4), result.ModifiedCount)
		assert.Equal(t, int64(1), result.DeletedCount)
		assert.Equal(t, int64(1), result.UpsertedCount)
		assert.Equal(t, "b4", result.UpsertedIDs[6], "upserted ID keyed by model index")

		// A later model must see the state produced by an earlier one.
		state, err := interceptor.FindMatches("bulk", bson.M{}, false)
		require.NoError(t, err)
		assert.Len(t, state, 3, "b1, b3(replaced), b4 remain")

		matches, err := interceptor.FindMatches("bulk", bson.M{"_id": "b1"}, true)
		require.NoError(t, err)
		require.Len(t, matches, 1)
		assert.EqualValues(t, 11, matches[0]["n"], "update saw the insert from the same bulk")
		assert.Equal(t, true, matches[0]["seen"])
	})

	t.Run("Ordered bulk stops at the first failure", func(t *testing.T) {
		models := []mongo.WriteModel{
			&mongo.InsertOneModel{Document: bson.M{"_id": "o1"}},
			&mongo.InsertOneModel{Document: bson.M{"_id": "o1"}}, // duplicate
			&mongo.InsertOneModel{Document: bson.M{"_id": "o2"}}, // must not run
		}

		result, err := interceptor.BulkWrite(ctx, "ordered", models, true)
		require.Error(t, err)

		var bulkErr *driverwal.BulkWriteError
		require.True(t, errors.As(err, &bulkErr))
		assert.Equal(t, 1, bulkErr.Index, "failure reported at the duplicate's index")
		assert.Equal(t, int64(1), result.InsertedCount, "partial result before the failure")

		state, err := interceptor.FindMatches("ordered", bson.M{}, false)
		require.NoError(t, err)
		assert.Len(t, state, 1, "o2 was never attempted")
	})

	t.Run("Unordered bulk continues past failures", func(t *testing.T) {
		models := []mongo.WriteModel{
			&mongo.InsertOneModel{Document: bson.M{"_id": "u1"}},
			&mongo.InsertOneModel{Document: bson.M{"_id": "u1"}}, // duplicate
			&mongo.InsertOneModel{Document: bson.M{"_id": "u2"}}, // still runs
		}

		result, err := interceptor.BulkWrite(ctx, "unordered", models, false)
		require.Error(t, err)
		assert.Equal(t, int64(2), result.InsertedCount, "both non-failing inserts applied")

		state, err := interceptor.FindMatches("unordered", bson.M{}, false)
		require.NoError(t, err)
		assert.Len(t, state, 2)
	})

	t.Run("Nil and unknown models are rejected", func(t *testing.T) {
		_, err := interceptor.BulkWrite(ctx, "bad", []mongo.WriteModel{nil}, true)
		require.Error(t, err)

		_, err = interceptor.BulkWrite(ctx, "bad", nil, true)
		require.Error(t, err, "empty bulks are rejected like the driver does")
	})
}

func TestCollection_BulkWrite(t *testing.T) {
	db := setupTestDB(t)
	walService, branchService, branch, _ := newInterceptorFixture(t, db, "bulk-coll-project", "main")
	mat := materializer.NewService(walService, branchService)
	collection := driverwal.NewCollection("events", branch, walService, branchService, mat)
	ctx := context.Background()

	result, err := collection.BulkWrite(ctx, []mongo.WriteModel{
		&mongo.InsertOneModel{Document: bson.M{"_id": "e1", "kind": "a"}},
		&mongo.InsertOneModel{Document: bson.M{"_id": "e2", "kind": "b"}},
		&mongo.DeleteManyModel{Filter: bson.M{"kind": "a"}},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.InsertedCount)
	assert.Equal(t, int64(1), result.DeletedCount)

	count, err := collection.CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}
