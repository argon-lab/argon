package wal

import (
	"context"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// IssueSandboxUser holds the branch's write lock while issue runs. MongoDB
// requires createUser to run outside the transaction. The username already
// exists in durable metadata, even if this transaction never commits.
func (s *BranchService) IssueSandboxUser(ctx context.Context, branch *wal.Branch, issue func() error) error {
	filter := bson.M{"_id": branch.ID, "is_deleted": false, "state": wal.BranchStateLive,
		"physical_db": wal.PhysicalDBName(branch.ID), "sandbox_username": branch.SandboxUsername,
		"sandbox_revoking": bson.M{"$ne": true}}
	_, err := s.wal.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		res, err := s.collection.UpdateOne(sc, filter, bson.M{"$inc": bson.M{"mutation_version": 1}})
		if err != nil {
			return nil, err
		}
		if res.MatchedCount == 0 {
			return nil, ErrBranchChanged
		}
		return nil, issue()
	})
	return err
}

// BeginSandboxRevocation orders dropUser after in-flight issuance across
// processes. A sweep also checks expiry here in case Keep or Extend won.
func (s *BranchService) BeginSandboxRevocation(ctx context.Context, branch *wal.Branch, expiredBefore *time.Time) error {
	filter := bson.M{"_id": branch.ID, "is_deleted": false, "sandbox_username": branch.SandboxUsername}
	if expiredBefore != nil {
		// BSON dates have millisecond precision. Include the current
		// millisecond when the sweep began partway through it.
		filter["expires_at"] = bson.M{"$lte": expiredBefore.Add(-time.Nanosecond)}
	}
	res, err := s.collection.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"sandbox_revoking": true}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrBranchChanged
	}
	return nil
}
