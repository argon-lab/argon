package wal

import (
	"bytes"
	"context"
	"github.com/argon-lab/argon/v2/internal/dberr"
	"log"

	"go.mongodb.org/mongo-driver/bson"
)

// FormatGuardValidator is the wal_log validator that refuses data entries
// without a typed document key. Every release from 2.0.2 writes key_v >= 1
// on every data entry; 2.0.1 and earlier never do. Readers assume that a
// branch's untyped entries all precede its first typed one (see
// HasLegacyKeyedEntries), which holds for an upgrade but not when an old
// engine, such as a capture process left running, appends after a newer one.
// The validator makes MongoDB refuse those appends instead.
//
// Control entries (branch, project, merge and resync records) carry no key
// and are not affected. With validationLevel "moderate", rows that already
// fail the rule, such as untyped history written before the upgrade, can
// still be updated: the schema-v1 migration rewrites rows in place.
func FormatGuardValidator() bson.D {
	return bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "operation", Value: bson.D{{Key: "$nin", Value: bson.A{
			string(OpPut), string(OpDelete), string(LegacyOpInsert), string(LegacyOpUpdate)}}}}},
		bson.D{{Key: "key_v", Value: bson.D{{Key: "$gte", Value: 1}}}},
	}}}
}

// ensureFormatGuard installs FormatGuardValidator on wal_log unless the
// collection already has it. It never blocks start-up: without the privilege
// to change collection options, or when an operator has set a different
// validator, it logs why the guard is missing and returns nil.
func (s *Service) ensureFormatGuard(ctx context.Context) error {
	want, err := bson.Marshal(FormatGuardValidator())
	if err != nil {
		return err
	}
	specs, err := s.db.ListCollectionSpecifications(ctx, bson.M{"name": s.collection.Name()})
	if err != nil {
		log.Printf("WAL format guard not checked: listing %s failed: %v", s.collection.Name(), err)
		return nil
	}
	if len(specs) == 1 {
		current, _ := specs[0].Options.Lookup("validator").DocumentOK()
		switch {
		case bytes.Equal(current, want):
			return nil
		case len(current) > 0 && !bytes.Equal(current, emptyDocument):
			log.Printf("WAL format guard not installed: %s already has a different validator; releases up to 2.0.1 can still append to it", s.collection.Name())
			return nil
		}
	}
	err = s.db.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: s.collection.Name()},
		{Key: "validator", Value: FormatGuardValidator()},
		{Key: "validationLevel", Value: "moderate"},
		{Key: "validationAction", Value: "error"},
	}).Err()
	if dberr.Unauthorized(err) {
		log.Printf("WAL format guard not installed: the engine's MongoDB user may not run collMod on %s.%s; releases up to 2.0.1 can still append to it", s.db.Name(), s.collection.Name())
		return nil
	}
	if err != nil {
		log.Printf("WAL format guard not installed on %s: %v", s.collection.Name(), err)
	}
	return nil
}

var emptyDocument = func() []byte { b, _ := bson.Marshal(bson.D{}); return b }()
