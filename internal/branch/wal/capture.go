package wal

import (
	"context"
	"errors"

	"github.com/argon-lab/argon/v2/internal/wal"
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

// ErrNoLongerFollows reports that a branch a follower was writing to has
// been detached from its source (or re-bound) since the follower started.
var ErrNoLongerFollows = errors.New("branch no longer follows this source")

// FollowerSetHead is CompareAndSetHead for the follower of an external
// database. Besides the observed head it requires that the branch still
// follows that database and is not checked out, in the same transaction as
// the entries. A branch detached by another process is therefore never
// written to again: the detach and this update touch the same document, so
// one of them loses.
func (s *BranchService) FollowerSetHead(ctx context.Context, branchID, follows string, expected, next int64) error {
	result, err := s.collection.UpdateOne(ctx, followerFilter(branchID, follows, expected),
		bson.M{"$set": bson.M{"head_lsn": next}, "$inc": bson.M{"mutation_version": 1}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return s.followerMiss(ctx, branchID, follows)
	}
	return nil
}

// FollowerCheckpoint is CheckpointHead with the same conditions.
func (s *BranchService) FollowerCheckpoint(ctx context.Context, branchID, follows string, expected int64) error {
	result, err := s.collection.UpdateOne(ctx, followerFilter(branchID, follows, expected),
		bson.M{"$inc": bson.M{"capture_fence": 1}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return s.followerMiss(ctx, branchID, follows)
	}
	return nil
}

func followerFilter(branchID, follows string, head int64) bson.M {
	return bson.M{"_id": branchID, "head_lsn": head, "is_deleted": false,
		"follows": follows, "state": bson.M{"$ne": wal.BranchStateLive}}
}

// followerMiss says why a follower's write found no branch to apply to.
func (s *BranchService) followerMiss(ctx context.Context, branchID, follows string) error {
	var branch wal.Branch
	err := s.collection.FindOne(ctx, bson.M{"_id": branchID}).Decode(&branch)
	if err != nil || branch.IsDeleted || branch.Follows != follows || branch.IsLive() {
		return ErrNoLongerFollows
	}
	return ErrBranchChanged
}
