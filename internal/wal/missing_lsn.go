package wal

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// LastMissingVisibleLSN returns the highest visible, unoccupied project LSN in
// [from, min(to, bound)], or zero when all such positions are accounted for.
// Any retained row occupies its LSN, even a control row or another branch's
// write. A gap may be an unused reservation or a reclaimed write.
func (s *Service) LastMissingVisibleLSN(branch *Branch, from, to, bound int64) (int64, error) {
	from = max(from, 1)
	next := min(to, bound)
	if next < from {
		return 0, nil
	}

	// Effective ends are in ascending order. As the cursor moves backwards,
	// pass each range once, using the read position to decide its visibility.
	ranges := branch.EffectiveDiscardedRanges()
	previousVisible := func(pos int64) int64 {
		for len(ranges) > 0 {
			r := ranges[len(ranges)-1]
			if r.To < pos {
				break
			}
			ranges = ranges[:len(ranges)-1]
			if r.To < to && r.From <= pos {
				pos = r.From - 1
			}
		}
		return pos
	}
	next = previousVisible(next)
	if next < from {
		return 0, nil
	}

	ctx := context.Background()
	filter := bson.M{"project_id": branch.ProjectID, "lsn": bson.M{"$gte": from, "$lte": next}}
	opts := options.Find().SetHint("project_id_1_lsn_1").SetSort(bson.D{{Key: "lsn", Value: -1}}).
		SetProjection(bson.M{"_id": 0, "lsn": 1}).SetBatchSize(512)
	cursor, err := s.collection.Find(ctx, filter, opts)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	for cursor.Next(ctx) {
		var row struct {
			LSN int64 `bson:"lsn"`
		}
		if err := cursor.Decode(&row); err != nil {
			return 0, err
		}
		if row.LSN < next {
			return next, nil
		}
		if row.LSN == next {
			next = previousVisible(next - 1)
			if next < from {
				return 0, nil
			}
		}
	}
	if err := cursor.Err(); err != nil {
		return 0, err
	}
	return next, nil
}
