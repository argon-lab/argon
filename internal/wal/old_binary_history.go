package wal

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ReclaimedOldBinaryError identifies a visible interval of a pre-upgrade
// snapshot for which the collection's WAL can no longer establish exact keys.
type ReclaimedOldBinaryError struct {
	BranchID   string `bson:"branch_id"`
	Collection string `bson:"collection"`
	From       int64  `bson:"from"`
	To         int64  `bson:"to"`
}

func (e *ReclaimedOldBinaryError) Error() string {
	return fmt.Sprintf("branch %q, collection %q: %v. The document keys are unknown across LSNs %d through %d because of %v. %s", e.BranchID, e.Collection, ErrOldBinarySnapshotReclaimed, e.From, e.To, ErrHistoryReclaimed, OldBinaryRecovery)
}

func (e *ReclaimedOldBinaryError) Unwrap() []error {
	return []error{ErrOldBinarySnapshotReclaimed, ErrHistoryReclaimed}
}

// OldBinaryHistory summarizes a branch's retained keys once for all snapshot
// positions. Deletes are ordered by LSN for discarded-range visibility checks.
type OldBinaryHistory struct {
	Evidence bool
	Deletes  []AmbiguousOldBinaryError
}

// ScanOldBinaryHistory examines the (branch, collection, document_id, lsn)
// index. A current key is base64 of BSON {i: value}: after the four length
// bytes, a type byte of 03, 04, 05 or 0f followed by 'i' gives the group
// [AQgw][NRV9]p. For scalar binary (Vp), byte 11 is the subtype. The
// next eight base64 characters end in [AEIMQUYcgkosw048]C for subtype 2.
// Ordinary scalar IDs, including UUIDs, need no WAL document fetch.
// Before typed keys, binary and compound IDs were stored as Extended JSON
// or Go values formatted by the schema-v1 writer. Only their images can
// distinguish an actual binary ID from a string with that spelling.
func (s *Service) ScanOldBinaryHistory(ctx context.Context, branchID, collection string) (OldBinaryHistory, error) {
	filter := bson.M{
		"branch_id": branchID, "collection": collection,
		"document_id": bson.M{"$in": bson.A{
			primitive.Regex{Pattern: "^" + documentKeyPrefix + ".{5}[AQgw][NR9]p"},
			primitive.Regex{Pattern: "^" + documentKeyPrefix + ".{5}[AQgw]Vp.{6}[AEIMQUYcgkosw048]C"},
			primitive.Regex{Pattern: `^\{`}, primitive.Regex{Pattern: `^\[`}, primitive.Regex{Pattern: `^map\[`},
		}},
	}
	// Return image bytes only for entries from before typed keys. For newer
	// entries the envelope supplies the type and only pre-image presence
	// matters. Use the document index to filter before fetching WAL documents.
	legacy := bson.M{"$lt": bson.A{bson.M{"$ifNull": bson.A{"$key_v", 0}}, 1}}
	projection := bson.M{
		"_id": 0, "document_id": 1, "lsn": 1, "operation": 1, "key_v": 1,
		"has_pre": bson.M{"$ne": bson.A{bson.M{"$type": "$pre"}, "missing"}},
	}
	for _, field := range []string{"pre", "post", "compressed_document", "compressed_old_document"} {
		projection[field] = bson.M{"$cond": bson.A{legacy, "$" + field, "$$REMOVE"}}
	}
	cursor, err := s.collection.Find(ctx, filter, options.Find().
		SetProjection(projection).SetSort(bson.D{{Key: "lsn", Value: 1}}).
		SetHint("branch_id_1_collection_1_document_id_1_lsn_1"))
	if err != nil {
		return OldBinaryHistory{}, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	var history OldBinaryHistory
	for cursor.Next(ctx) {
		if err := ctx.Err(); err != nil {
			return OldBinaryHistory{}, err
		}
		var row oldBinaryHistoryRow
		if err := cursor.Decode(&row); err != nil {
			return OldBinaryHistory{}, err
		}
		if !history.Evidence {
			history.Evidence, err = row.hasOldBinaryID(s.compressor)
			if err != nil {
				return OldBinaryHistory{}, err
			}
		}
		if row.Operation == OpDelete && !row.HasPre && row.DocumentKeyVersion < exactOldBinaryVersion && IsSharedOldBinaryKey(row.DocumentID) {
			history.Deletes = append(history.Deletes, AmbiguousOldBinaryError{BranchID: branchID, Collection: collection, Key: row.DocumentID, LSN: row.LSN})
		}
	}
	return history, cursor.Err()
}

type oldBinaryHistoryRow struct {
	Entry       `bson:",inline"`
	HasPre      bool   `bson:"has_pre"`
	Document    []byte `bson:"compressed_document"`
	OldDocument []byte `bson:"compressed_old_document"`
}

func (row *oldBinaryHistoryRow) hasOldBinaryID(compressor *Compressor) (bool, error) {
	if row.DocumentKeyVersion != 0 {
		return IsOldBinaryKey(row.DocumentID), nil
	}
	for _, image := range [][]byte{row.CompressedPreImage, row.CompressedPostImage, row.OldDocument} {
		if len(image) == 0 {
			continue
		}
		raw, err := compressor.Decompress(image)
		if err != nil {
			return false, err
		}
		if IsOldBinaryID(bson.Raw(raw).Lookup("_id")) {
			return true, nil
		}
	}
	if len(row.Document) == 0 {
		return false, nil
	}
	raw, err := compressor.Decompress(row.Document)
	if err != nil {
		return false, err
	}
	if row.Operation == LegacyOpUpdate {
		return IsOldBinaryID(bson.Raw(raw).Lookup("filter", "_id")), nil
	}
	return IsOldBinaryID(bson.Raw(raw).Lookup("_id")), nil
}

// HasLiveOldBinaryID checks only an Argon checkout. Other branches derive
// their entire current state from the snapshots and WAL already inspected.
func (s *Service) HasLiveOldBinaryID(ctx context.Context, branch *Branch, collection string) (bool, error) {
	if !branch.IsLive() {
		return false, nil
	}
	name, err := branch.OwnDatabase()
	if err != nil {
		return false, err
	}
	filter := bson.M{"_id": bson.M{"$type": bson.A{"binData", "object", "javascriptWithScope"}}}
	cursor, err := s.db.Client().Database(name).Collection(collection).Find(ctx, filter,
		options.Find().SetProjection(bson.M{"_id": 1}).SetHint("_id_"))
	if err != nil {
		return false, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	for cursor.Next(ctx) {
		if IsOldBinaryID(cursor.Current.Lookup("_id")) {
			return true, nil
		}
	}
	return false, cursor.Err()
}
