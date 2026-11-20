package wal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrHistoryReclaimed reports that a read would need WAL entries garbage
// collection has deleted. GC keeps heads, forks, pins and every position
// inside its retention window reconstructible; an older position may not
// be. Returning this error is the alternative to replaying across the hole
// and presenting a state that silently lacks the reclaimed writes.
var ErrHistoryReclaimed = errors.New("history reclaimed by garbage collection")

// ErrOldBinarySnapshotReclaimed means a pre-upgrade snapshot could have
// collapsed old-binary IDs and its covered history cannot establish the state.
var ErrOldBinarySnapshotReclaimed = errors.New("pre-upgrade snapshot cannot establish the old-binary document keys")

// reclaimFloorsCollection records, per (branch, collection), the highest
// LSN at or below which GC may have deleted data entries.
const reclaimFloorsCollection = "wal_reclaim_floors"

func reclaimFloorID(branchID, collection string) bson.D {
	return bson.D{{Key: "branch_id", Value: branchID}, {Key: "collection", Value: collection}}
}

// The prefix also gives the empty name a nonempty MongoDB field name.
func reclaimGateKey(collection string) string {
	return "c" + base64.RawURLEncoding.EncodeToString([]byte(collection))
}

type reclaimGates map[string]int64

// Gates are optional hints. Discard the whole map if any member is malformed.
func (g *reclaimGates) UnmarshalBSONValue(t bsontype.Type, data []byte) error {
	*g = nil
	doc, ok := (bson.RawValue{Type: t, Value: data}).DocumentOK()
	if !ok {
		return nil
	}
	elems, err := doc.Elements()
	if err != nil {
		return err
	}
	gates := make(reclaimGates, len(elems))
	for _, elem := range elems {
		n, ok := elem.Value().Int64OK()
		if !ok {
			return nil
		}
		gates[elem.Key()] = n
	}
	*g = gates
	return nil
}

// ReclaimGate is the greatest cutoff published on this branch record for a collection.
// Older garbage collectors can advance the floor without advancing the gate,
// and can forget a deleted branch's floors without clearing its gates.
func (b *Branch) ReclaimGate(collection string) int64 {
	if len(b.ReclaimGates) == 0 || b.IsDeleted {
		return 0
	}
	return b.ReclaimGates[reclaimGateKey(collection)]
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
	floor := s.db.Collection(reclaimFloorsCollection)
	filter := bson.M{"_id": reclaimFloorID(branchID, collection)}
	maximum := bson.M{"lsn": lsn}
	writeFloor := func(ctx context.Context) (*mongo.UpdateResult, error) {
		return floor.UpdateOne(ctx, filter, bson.M{"$max": maximum}, options.Update().SetUpsert(true))
	}

	// An oversized key cannot fit even on an empty branch. Inside a caller's
	// transaction, a refused optional write would abort their other work.
	var err error
	if base64.RawURLEncoding.EncodedLen(len(collection)) >= 16<<20 || inFloorTransaction(ctx) {
		_, err = writeFloor(ctx)
	} else {
		_, err = s.withFloorTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
			recorded, err := writeFloor(sc)
			if err != nil {
				return nil, err
			}
			gate, err := s.db.Collection("wal_branches").UpdateOne(sc,
				bson.M{"_id": branchID, "is_deleted": false,
					"$expr": bson.M{"$in": bson.A{bson.M{"$type": "$reclaim_gates"}, bson.A{"missing", "object"}}}},
				bson.M{"$max": bson.M{"reclaim_gates." + reclaimGateKey(collection): lsn}},
			)
			if err != nil {
				return nil, err
			}
			if gate.ModifiedCount == 0 || recorded.UpsertedCount != 0 || recorded.ModifiedCount != 0 {
				return nil, nil
			}

			// The floor already covered this LSN. A newly published gate
			// still needs a real write to conflict with a concurrent Forget.
			_, err = floor.UpdateOne(sc, filter, bson.M{"$currentDate": bson.M{"gate_updated": bson.M{"$type": "timestamp"}}})
			return nil, err
		})
		if err != nil && ctx.Err() == nil {
			// The gate is optional. Whatever stopped the paired write, the
			// aborted transaction raised nothing, so record the floor alone,
			// as releases without a gate did. Only that write can fail GC.
			_, err = writeFloor(ctx)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to record reclaim floor for %s: %w", collection, err)
	}
	return nil
}

func inFloorTransaction(ctx context.Context) bool {
	session, ok := mongo.SessionFromContext(ctx).(mongo.XSession) //nolint:staticcheck
	return ok && session.ClientSession().TransactionRunning()
}

func (s *Service) withFloorTransaction(ctx context.Context, fn func(mongo.SessionContext) (interface{}, error)) (interface{}, error) {
	if session := mongo.SessionFromContext(ctx); session != nil {
		if inFloorTransaction(ctx) {
			return fn(mongo.NewSessionContext(ctx, session))
		}
		return session.WithTransaction(ctx, fn)
	}
	return s.WithTransaction(ctx, fn)
}

// ReclaimFloor returns the highest LSN at or below which data entries of a
// collection on a branch may have been garbage collected, or 0 if none
// were. Databases collected only by releases up to 2.1.5 have no floors:
// those releases did not record them.
func (s *Service) ReclaimFloor(branchID, collection string) (int64, error) {
	return s.ReclaimFloorContext(context.Background(), branchID, collection)
}

// ReclaimFloorContext reads one collection's floor within the caller's deadline.
func (s *Service) ReclaimFloorContext(ctx context.Context, branchID, collection string) (int64, error) {
	var floor struct {
		LSN int64 `bson:"lsn"`
	}
	err := s.db.Collection(reclaimFloorsCollection).FindOne(ctx,
		bson.M{"_id": reclaimFloorID(branchID, collection)}).Decode(&floor)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read reclaim floor: %w", err)
	}
	return floor.LSN, nil
}

// LatestMissingLSN returns the highest unoccupied project LSN in [from, to],
// or zero if every position is present. The project/LSN index includes rows
// for other branches and collections as well as non-data operations.
func (s *Service) LatestMissingLSN(projectID string, from, to int64) (int64, error) {
	ctx := context.Background()
	filter := bson.M{"project_id": projectID, "lsn": bson.M{"$gte": from, "$lte": to}}
	opts := options.Find().SetSort(bson.D{{Key: "lsn", Value: -1}}).SetProjection(bson.M{"_id": 0, "lsn": 1})
	cursor, err := s.collection.Find(ctx, filter, opts)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	for cursor.Next(ctx) {
		at, ok := cursor.Current.Lookup("lsn").AsInt64OK()
		if !ok {
			return 0, fmt.Errorf("invalid WAL LSN in project %s", projectID)
		}
		if at < to {
			return to, nil
		}
		to--
	}
	if err := cursor.Err(); err != nil {
		return 0, err
	}
	if to >= from {
		return to, nil
	}
	return 0, nil
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
	_, err := s.withFloorTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if _, err := s.db.Collection("wal_branches").UpdateOne(sc,
			bson.M{"_id": branchID}, bson.M{"$unset": bson.M{"reclaim_gates": ""}},
		); err != nil {
			return nil, err
		}
		return s.db.Collection(reclaimFloorsCollection).DeleteMany(sc, bson.M{"_id.branch_id": branchID})
	})
	return err
}
