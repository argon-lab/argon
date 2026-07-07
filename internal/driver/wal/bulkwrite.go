package wal

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/mongo"
)

// BulkWriteResult mirrors mongo.BulkWriteResult: aggregate counts plus the
// upserted IDs keyed by the index of the model that produced them.
type BulkWriteResult struct {
	InsertedCount int64
	MatchedCount  int64
	ModifiedCount int64
	DeletedCount  int64
	UpsertedCount int64
	UpsertedIDs   map[int64]interface{}
}

// BulkWriteError reports which model of a bulk operation failed. Callers
// still receive the partial result accumulated before (and, for unordered
// bulks, around) the failure, matching driver behavior.
type BulkWriteError struct {
	Index int
	Err   error
}

func (e *BulkWriteError) Error() string {
	return fmt.Sprintf("bulk write model %d: %v", e.Index, e.Err)
}

func (e *BulkWriteError) Unwrap() error { return e.Err }

// BulkWrite executes a sequence of write models against one collection.
//
// Models always execute sequentially — each model must resolve against the
// state produced by the ones before it (an update may target a document
// inserted two models earlier). `ordered` only controls error handling, as
// in MongoDB: ordered bulks stop at the first failing model, unordered
// bulks attempt every model and report the failures afterwards.
func (i *Interceptor) BulkWrite(ctx context.Context, collection string, models []mongo.WriteModel, ordered bool) (*BulkWriteResult, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("bulk write requires at least one model")
	}

	result := &BulkWriteResult{UpsertedIDs: make(map[int64]interface{})}
	var failures []error

	for idx, model := range models {
		err := i.applyBulkModel(ctx, collection, int64(idx), model, result)
		if err != nil {
			failure := &BulkWriteError{Index: idx, Err: err}
			if ordered {
				return result, failure
			}
			failures = append(failures, failure)
		}
	}

	if len(failures) > 0 {
		return result, errors.Join(failures...)
	}
	return result, nil
}

func (i *Interceptor) applyBulkModel(ctx context.Context, collection string, idx int64, model mongo.WriteModel, result *BulkWriteResult) error {
	switch m := model.(type) {
	case *mongo.InsertOneModel:
		if _, err := i.InsertOne(ctx, collection, m.Document); err != nil {
			return err
		}
		result.InsertedCount++

	case *mongo.UpdateOneModel:
		r, err := i.UpdateOne(ctx, collection, m.Filter, m.Update, boolValue(m.Upsert))
		if err != nil {
			return err
		}
		accumulateUpdate(result, r, idx)

	case *mongo.UpdateManyModel:
		r, err := i.UpdateMany(ctx, collection, m.Filter, m.Update, boolValue(m.Upsert))
		if err != nil {
			return err
		}
		accumulateUpdate(result, r, idx)

	case *mongo.ReplaceOneModel:
		r, err := i.ReplaceOne(ctx, collection, m.Filter, m.Replacement, boolValue(m.Upsert))
		if err != nil {
			return err
		}
		accumulateUpdate(result, r, idx)

	case *mongo.DeleteOneModel:
		r, err := i.DeleteOne(ctx, collection, m.Filter)
		if err != nil {
			return err
		}
		result.DeletedCount += r.DeletedCount

	case *mongo.DeleteManyModel:
		r, err := i.DeleteMany(ctx, collection, m.Filter)
		if err != nil {
			return err
		}
		result.DeletedCount += r.DeletedCount

	case nil:
		return fmt.Errorf("write model is nil")

	default:
		return fmt.Errorf("unsupported write model type %T", model)
	}
	return nil
}

func accumulateUpdate(result *BulkWriteResult, r *UpdateResult, idx int64) {
	result.MatchedCount += r.MatchedCount
	result.ModifiedCount += r.ModifiedCount
	result.UpsertedCount += r.UpsertedCount
	if r.UpsertedID != nil {
		result.UpsertedIDs[idx] = r.UpsertedID
	}
}

func boolValue(b *bool) bool {
	return b != nil && *b
}
