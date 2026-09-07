package snapshot

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// A lock deliberately has no TTL. Expiring a lock while an object-store
// delete is still executing could let another process publish a manifest
// for the same content address, then have that content deleted underneath
// it. MongoDB cannot fence a remote S3/filesystem deletion. After a crash,
// maintenance must stop snapshot/GC workers, inspect this owner, and remove
// the abandoned lock. This fails closed rather than risking snapshot loss.
func (s *Service) lockPublication(ctx context.Context) (func(), error) {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	owner := primitive.NewObjectID().Hex()
	hostname, _ := os.Hostname()
	timer := time.NewTicker(25 * time.Millisecond)
	defer timer.Stop()
	for {
		_, err := s.publicationLocks.InsertOne(waitCtx, bson.M{
			"_id": "publication", "owner": owner, "hostname": hostname,
			"pid": os.Getpid(), "started_at": time.Now(),
		})
		if err == nil {
			return func() {
				releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer releaseCancel()
				if _, err := s.publicationLocks.DeleteOne(releaseCtx, bson.M{"_id": "publication", "owner": owner}); err != nil {
					log.Printf("snapshot: publication lock %s could not be released: %v; maintenance is required", owner, err)
				}
			}, nil
		}
		if !mongo.IsDuplicateKeyError(err) {
			return nil, fmt.Errorf("acquire snapshot publication lock: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("snapshot publication is busy or a worker left an abandoned lock; inspect wal_snapshot_locks and stop all snapshot/GC workers before clearing it: %w", waitCtx.Err())
		case <-timer.C:
		}
	}
}
