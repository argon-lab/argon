package wal

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
)

// CheckpointHead serializes an idle capture checkpoint against data mutations
// without changing the logical branch version or invalidating reviewed plans.
// It must run in the same transaction as the resume-token update.
func (s *BranchService) CheckpointHead(ctx context.Context, branchID string, expected int64) error {
	result, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": branchID, "head_lsn": expected, "is_deleted": false},
		bson.M{"$inc": bson.M{"capture_fence": 1}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrBranchChanged
	}
	return nil
}
