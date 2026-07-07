package wal

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Sequencer allocates monotonically increasing LSNs per project using an
// atomic MongoDB counter document, making allocation safe across any number
// of concurrent Argon processes. LSNs start at 1; 0 is reserved as the
// "before any operation" sentinel used by branch base pointers.
type Sequencer struct {
	counters *mongo.Collection
}

// maxReserveRetries bounds the retry loop that resolves the upsert race two
// processes can hit when allocating a project's very first LSN.
const maxReserveRetries = 5

// NewSequencer creates a sequencer backed by the wal_counters collection.
func NewSequencer(db *mongo.Database) *Sequencer {
	return &Sequencer{counters: db.Collection("wal_counters")}
}

// Reserve atomically reserves n consecutive LSNs for a project and returns
// the first one. If the caller fails after reserving (e.g. the WAL insert
// errors), the reserved LSNs simply become a gap in the sequence. Gaps are
// harmless: consumers rely on ordering, never on density, so reservations
// are never rolled back.
func (s *Sequencer) Reserve(projectID string, n int64) (int64, error) {
	if projectID == "" {
		return 0, fmt.Errorf("cannot reserve LSN: project ID is empty")
	}
	if n <= 0 {
		return 0, fmt.Errorf("cannot reserve %d LSNs: count must be positive", n)
	}

	ctx := context.Background()
	opts := options.FindOneAndUpdate().
		SetUpsert(true).
		SetReturnDocument(options.After)

	var lastErr error
	for attempt := 0; attempt < maxReserveRetries; attempt++ {
		var counter struct {
			Next int64 `bson:"next"`
		}
		err := s.counters.FindOneAndUpdate(ctx,
			bson.M{"_id": projectID},
			bson.M{"$inc": bson.M{"next": n}},
			opts,
		).Decode(&counter)
		if err == nil {
			return counter.Next - n + 1, nil
		}
		// Two processes upserting a project's first counter document can
		// race; the loser gets a duplicate key error and must retry, at
		// which point the document exists and $inc succeeds.
		if mongo.IsDuplicateKeyError(err) {
			lastErr = err
			continue
		}
		return 0, fmt.Errorf("failed to reserve LSNs for project %s: %w", projectID, err)
	}
	return 0, fmt.Errorf("failed to reserve LSNs for project %s after %d attempts: %w",
		projectID, maxReserveRetries, lastErr)
}

// Current returns the most recently allocated LSN for a project, or 0 if no
// LSN has been allocated yet. Note that the highest allocated LSN may not be
// durable in the WAL yet (or ever, if the writer failed after reserving), so
// Current is an upper bound on written LSNs, not a read barrier.
func (s *Sequencer) Current(projectID string) (int64, error) {
	ctx := context.Background()
	var counter struct {
		Next int64 `bson:"next"`
	}
	err := s.counters.FindOne(ctx, bson.M{"_id": projectID}).Decode(&counter)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read LSN counter for project %s: %w", projectID, err)
	}
	return counter.Next, nil
}
