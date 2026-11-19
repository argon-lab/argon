package wal

import (
	"bytes"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
