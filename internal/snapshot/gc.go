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
// Chunk reclamation is a two-step check (drop manifests, then delete
// chunks that no remaining manifest references). A snapshot being created
// concurrently could in principle re-reference a chunk between the check
// and the delete; the next snapshot of that branch simply re-uploads the
// chunk (content addressing makes that harmless but wasteful), and proper
// epoch-based GC arrives with WAL segment retention.
func (s *Service) CleanupBranch(ctx context.Context, branchID string) (manifestsRemoved, chunksRemoved int64, err error) {
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
	chunkRes, err := s.chunks.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": orphaned}})
	if err != nil {
		return manifestsRemoved, 0, fmt.Errorf("failed to delete orphaned chunks: %w", err)
	}
	return manifestsRemoved, chunkRes.DeletedCount, nil
}
