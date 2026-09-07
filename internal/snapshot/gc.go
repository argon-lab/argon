package snapshot

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
)

// CleanupBranch removes a deleted branch's snapshot manifests and any
// chunks no other manifest references.
//
// This is safe for regularly deleted branches because deletion refuses
// branches with children — nobody's ancestry chain can reach the removed
// snapshots. Force-deleted branches (which may still anchor descendants)
// must not be cleaned up; descendants keep reading the ancestor's
// snapshots through the chain.
//
// Publication and reclamation share a durable lock across processes. A
// surviving manifest can never be published between reference checking
// and chunk deletion, including when the chunk store is external to MongoDB.
func (s *Service) CleanupBranch(ctx context.Context, branchID string) (manifestsRemoved, chunksRemoved int64, err error) {
	unlock, err := s.lockPublication(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()
	// Collect the chunk IDs this branch's manifests reference.
	cursor, err := s.manifests.Find(ctx, bson.M{"branch_id": branchID})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to list snapshots for branch %s: %w", branchID, err)
	}
	candidates := make(map[string]bool)
	var manifests []Snapshot
	if err := cursor.All(ctx, &manifests); err != nil {
		return 0, 0, fmt.Errorf("failed to load snapshots for branch %s: %w", branchID, err)
	}
	for _, m := range manifests {
		for _, id := range m.ChunkIDs {
			candidates[id] = true
		}
	}
	if len(manifests) == 0 {
		return 0, 0, nil
	}

	res, err := s.manifests.DeleteMany(ctx, bson.M{"branch_id": branchID})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to delete snapshot manifests: %w", err)
	}
	manifestsRemoved = res.DeletedCount

	// Keep chunks still referenced by any surviving manifest.
	candidateIDs := make([]string, 0, len(candidates))
	for id := range candidates {
		candidateIDs = append(candidateIDs, id)
	}
	stillUsed, err := s.manifests.Distinct(ctx, "chunk_ids", bson.M{"chunk_ids": bson.M{"$in": candidateIDs}})
	if err != nil {
		return manifestsRemoved, 0, fmt.Errorf("failed to check chunk references: %w", err)
	}
	for _, v := range stillUsed {
		if id, ok := v.(string); ok {
			delete(candidates, id)
		}
	}

	if len(candidates) == 0 {
		return manifestsRemoved, 0, nil
	}
	orphaned := make([]string, 0, len(candidates))
	for id := range candidates {
		orphaned = append(orphaned, id)
	}
	// Through the store interface, so filesystem and S3 backends reclaim
	// their objects too — not just the MongoDB collection.
	if err := s.store.Delete(ctx, orphaned); err != nil {
		return manifestsRemoved, 0, fmt.Errorf("failed to delete orphaned chunks: %w", err)
	}
	return manifestsRemoved, int64(len(orphaned)), nil
}
