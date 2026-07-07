package wal

import (
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// DocumentIDString converts a document _id into the canonical string key
// used throughout the WAL (Entry.DocumentID and materialized state maps).
//
// ObjectIDs map to their 24-character hex form and strings map to
// themselves, matching what users naturally pass to document-history APIs.
// Every other BSON type is rendered as canonical extended JSON so that,
// for example, the int32 5, the int64 5 and the string "5" cannot collide
// on the same key and silently merge two different documents during replay.
func DocumentIDString(id interface{}) string {
	switch v := id.(type) {
	case primitive.ObjectID:
		return v.Hex()
	case string:
		return v
	default:
		wrapped, err := bson.MarshalExtJSON(bson.M{"i": id}, true, false)
		if err != nil {
			// No BSON representation at all; fall back to Go formatting.
			return fmt.Sprintf("%v", id)
		}
		// Strip the {"i": ... } wrapper: {"i":X} -> X
		s := string(wrapped)
		if len(s) > 6 {
			return s[5 : len(s)-1]
		}
		return s
	}
}
