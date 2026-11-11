package wal

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrHistoryReclaimed reports that a read would need WAL entries garbage
// collection has deleted. GC keeps heads, forks, pins and every position
// inside its retention window reconstructible; an older position may not
// be. Returning this error is the alternative to replaying across the hole
// and presenting a state that silently lacks the reclaimed writes.
var ErrHistoryReclaimed = errors.New("history reclaimed by garbage collection")

// reclaimFloorsCollection records, per (branch, collection), the highest
// LSN at or below which GC may have deleted data entries.
const reclaimFloorsCollection = "wal_reclaim_floors"

func reclaimFloorID(branchID, collection string) bson.D {
	return bson.D{{Key: "branch_id", Value: branchID}, {Key: "collection", Value: collection}}
}

// RecordReclaimFloor notes that data entries of a collection on a branch at
// or below lsn are about to be deleted. GC calls it before deleting, so a
// crash in between leaves a floor that is too high (reads refused that
// could have worked) rather than a hole nobody knows about. Floors only
// move up.
func (s *Service) RecordReclaimFloor(ctx context.Context, branchID, collection string, lsn int64) error {
	if lsn <= 0 {
		return nil
	}
	_, err := s.db.Collection(reclaimFloorsCollection).UpdateOne(ctx,
		bson.M{"_id": reclaimFloorID(branchID, collection)},
		bson.M{"$max": bson.M{"lsn": lsn}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to record reclaim floor for %s: %w", collection, err)
	}
	return nil
}

// ReclaimFloor returns the highest LSN at or below which data entries of a
// collection on a branch may have been garbage collected, or 0 if none
// were. Databases collected only by releases up to 2.1.5 have no floors:
// those releases did not record them.
func (s *Service) ReclaimFloor(branchID, collection string) (int64, error) {
	var floor struct {
		LSN int64 `bson:"lsn"`
	}
	err := s.db.Collection(reclaimFloorsCollection).FindOne(context.Background(),
		bson.M{"_id": reclaimFloorID(branchID, collection)}).Decode(&floor)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read reclaim floor: %w", err)
	}
	return floor.LSN, nil
}

// ReclaimFloors returns every recorded floor of a branch, keyed by
// collection. The map is empty when nothing was reclaimed.
func (s *Service) ReclaimFloors(branchID string) (map[string]int64, error) {
	ctx := context.Background()
	cursor, err := s.db.Collection(reclaimFloorsCollection).Find(ctx, bson.M{"_id.branch_id": branchID})
	if err != nil {
		return nil, fmt.Errorf("failed to read reclaim floors: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()
	floors := make(map[string]int64)
	for cursor.Next(ctx) {
		var doc struct {
			ID struct {
				Collection string `bson:"collection"`
			} `bson:"_id"`
			LSN int64 `bson:"lsn"`
		}
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("failed to decode reclaim floor: %w", err)
		}
		floors[doc.ID.Collection] = doc.LSN
	}
	return floors, cursor.Err()
}

// ForgetReclaimFloors drops a branch's floors. Only for a branch whose
// entire history is being deleted: nothing can read it afterwards.
func (s *Service) ForgetReclaimFloors(ctx context.Context, branchID string) error {
	_, err := s.db.Collection(reclaimFloorsCollection).DeleteMany(ctx, bson.M{"_id.branch_id": branchID})
	return err
}
