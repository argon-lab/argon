package merge

import (
	"context"
	"encoding/json"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// UniqueCheckResult describes collisions in the proposed final state.
type UniqueCheckResult struct {
	Collisions []UniqueCollision      `json:"collisions"`
	NotChecked []UncheckedUniqueIndex `json:"not_checked"`
}

type UniqueCollision struct {
	Collection string          `json:"collection"`
	Index      string          `json:"index"`
	Key        json.RawMessage `json:"key"`
	PlanID     json.RawMessage `json:"plan_document_id"`
	OtherID    json.RawMessage `json:"other_document_id"`
	InPlan     bool            `json:"in_plan"`
}

type UncheckedUniqueIndex struct {
	Collection string `json:"collection"`
	Index      string `json:"index"`
	Reason     string `json:"reason"`
}

// CheckUniqueIndexes evaluates the same saved BSON images selected for write-back.
func (p *Plan) CheckUniqueIndexes(ctx context.Context, database *mongo.Database, strategy string) (*UniqueCheckResult, error) {
	changes, _, err := resolveChanges(p, strategy)
	if err != nil {
		return nil, err
	}
	if err := prepareWriteBack(p, changes); err != nil {
		return nil, err
	}
	return checkUniqueIndexes(ctx, database, changes)
}

// Both output formats show the same ordered, canonical Extended JSON values.
func (r *UniqueCheckResult) addCollision(collection, index string, key bson.Raw, planID, otherID bson.RawValue, inPlan bool) error {
	raw, err := bson.Marshal(bson.D{
		{Key: "key", Value: key}, {Key: "plan_document_id", Value: planID},
		{Key: "other_document_id", Value: otherID},
	})
	if err != nil {
		return err
	}
	var fields bson.D
	if err := wal.UnmarshalDocument(raw, &fields); err != nil {
		return err
	}
	data, err := bson.MarshalExtJSON(extendedJSONValue(fields, "", nil), true, false)
	if err != nil {
		return err
	}
	c := UniqueCollision{Collection: collection, Index: index, InPlan: inPlan}
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	r.Collisions = append(r.Collisions, c)
	return nil
}
