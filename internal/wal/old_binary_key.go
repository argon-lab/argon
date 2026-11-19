package wal

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Older writers indexed an empty subtype-2 value as four zero payload bytes,
// even inside compound IDs. Both keys can share that index entry. The raw
// document image identifies which key a historical put or delete belongs to.
func oldBinaryIndexKey(key string) (string, bool) {
	if !strings.HasPrefix(key, documentKeyPrefix) {
		return "", false
	}
	id, err := DocumentIDValue(key)
	if err != nil {
		return "", false
	}
	old, affected := oldBinaryIndexValue(id)
	if !affected {
		return "", false
	}
	return DocumentIDString(old), true
}

func oldBinaryIndexValue(value any) (any, bool) {
	switch v := value.(type) {
	case primitive.Binary:
		if v.Subtype == bson.TypeBinaryBinaryOld {
			zeros := []byte{0, 0, 0, 0}
			if len(v.Data) == 0 || bytes.Equal(v.Data, zeros) {
				v.Data = zeros
				return v, true
			}
		}
	case bson.D:
		var result bson.D
		for i, field := range v {
			if x, affected := oldBinaryIndexValue(field.Value); affected {
				if result == nil {
					result = slices.Clone(v)
				}
				result[i].Value = x
			}
		}
		if result != nil {
			return result, true
		}
	case bson.A:
		var result bson.A
		for i := range v {
			if x, affected := oldBinaryIndexValue(v[i]); affected {
				if result == nil {
					result = slices.Clone(v)
				}
				result[i] = x
			}
		}
		if result != nil {
			return result, true
		}
	case primitive.CodeWithScope:
		if scope, affected := oldBinaryIndexValue(v.Scope); affected {
			v.Scope = scope
			return v, true
		}
	}
	return value, false
}

// IsSharedOldBinaryKey reports whether pre-upgrade writers could have stored
// several different IDs under this envelope. The four-zero BSON wire value
// ends in seven zero bytes, hence at least eight consecutive base64 A's.
func IsSharedOldBinaryKey(key string) bool {
	if !strings.HasPrefix(key, documentKeyPrefix) || !strings.Contains(key, "AAAAAAAA") {
		return false
	}
	alias, affected := oldBinaryIndexKey(key)
	return affected && alias == key
}

const exactOldBinaryVersion = 2

// AmbiguousOldBinaryError identifies an image-less delete recorded under a
// key that the writer could have computed from either BSON ID.
type AmbiguousOldBinaryError struct {
	BranchID, Collection, Key string
	LSN                       int64
}

// OldBinaryRecovery describes the available recovery paths for uncertain keys.
const OldBinaryRecovery = "For a followed branch, stop following and run `argon source resync -p <project> --force`. " +
	"For an ordinary branch, use `argon time-travel query` to check a readable LSN before the affected history. " +
	"Fork there with `argon restore branch` or rewind the existing branch with `argon restore reset`. " +
	"To start from a fresh copy, `argon import database` imports the database into a new project and leaves the original branch in place."

func (e *AmbiguousOldBinaryError) Error() string {
	return fmt.Sprintf("branch %q, collection %q, key %q at LSN %d: history recorded before the upgrade cannot tell an empty old-binary _id from a four-zero-byte one. %s", e.BranchID, e.Collection, e.Key, e.LSN, OldBinaryRecovery)
}

// CheckStoredKey reports whether an entry can rely on its envelope when
// there is no image or its image does not supply the ID.
func (e *Entry) CheckStoredKey() error {
	if e.DocumentKeyVersion < exactOldBinaryVersion && IsSharedOldBinaryKey(e.DocumentID) {
		return &AmbiguousOldBinaryError{BranchID: e.BranchID, Collection: e.Collection, Key: e.DocumentID, LSN: e.LSN}
	}
	return nil
}

// CheckOldBinaryDeletes checks the history covered by a pre-upgrade snapshot.
// A delete could have removed the last frame, leaving an empty snapshot.
func (s *Service) CheckOldBinaryDeletes(branch *Branch, collection string, from, to int64) error {
	ctx := context.Background()
	cursor, err := s.collection.Find(ctx, bson.M{
		"branch_id": branch.ID, "collection": collection, "lsn": bson.M{"$gte": from, "$lte": to},
		"operation": OpDelete, "key_v": bson.M{"$not": bson.M{"$gte": exactOldBinaryVersion}}, "pre": bson.M{"$exists": false},
		"document_id": primitive.Regex{Pattern: "^~argon:bson:.*AAAAAAAA"},
	}, options.Find().SetProjection(bson.M{"_id": 0, "lsn": 1, "document_id": 1}))
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(ctx) }()
	for cursor.Next(ctx) {
		var e Entry
		if err := cursor.Decode(&e); err != nil {
			return err
		}
		if !branch.IsDiscardedForRead(e.LSN, to) && IsSharedOldBinaryKey(e.DocumentID) {
			return &AmbiguousOldBinaryError{BranchID: branch.ID, Collection: collection, Key: e.DocumentID, LSN: e.LSN}
		}
	}
	return cursor.Err()
}
