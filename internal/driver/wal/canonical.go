package wal

import (
	"fmt"
	"sort"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Decoded documents are Go maps, and marshalling a map serializes its keys
// in randomized iteration order — so comparing two logically identical
// documents by marshalling each of them is not stable. CanonicalBytes fixes
// an ordering (keys sorted, recursively) purely for comparison purposes.
//
// Note this makes document equality field-order-insensitive, which is a
// documented deviation from MongoDB (where {a:1,b:2} != {b:2,a:1} as a
// value): the original field order is already lost when a post-image is
// decoded into a map, so order-insensitive comparison is the only stable
// choice until reads run on a real mongod.

// CanonicalBytes returns a deterministic byte representation of a BSON value.
func CanonicalBytes(v interface{}) ([]byte, error) {
	canonical, err := canonicalizeValue(v)
	if err != nil {
		return nil, err
	}
	return bson.Marshal(bson.D{{Key: "v", Value: canonical}})
}

// Canonicalize converts a BSON value into its canonical form: documents
// become bson.D with recursively sorted keys, so marshalling the result is
// deterministic. Snapshot serialization depends on this for content-level
// chunk deduplication.
func Canonicalize(v interface{}) (interface{}, error) {
	return canonicalizeValue(v)
}

// CanonicalEqual reports whether two BSON values are equal under the
// canonical (sorted-key) representation.
func CanonicalEqual(a, b interface{}) (bool, error) {
	aBytes, err := CanonicalBytes(a)
	if err != nil {
		return false, err
	}
	bBytes, err := CanonicalBytes(b)
	if err != nil {
		return false, err
	}
	return string(aBytes) == string(bBytes), nil
}

func canonicalizeValue(v interface{}) (interface{}, error) {
	switch val := v.(type) {
	case bson.M:
		return canonicalizeMap(val)
	case map[string]interface{}:
		return canonicalizeMap(val)
	case bson.D:
		m := make(map[string]interface{}, len(val))
		for _, e := range val {
			m[e.Key] = e.Value
		}
		return canonicalizeMap(m)
	case []interface{}:
		return canonicalizeSlice(val)
	case primitive.A:
		return canonicalizeSlice(val)
	default:
		return v, nil
	}
}

func canonicalizeMap(m map[string]interface{}) (bson.D, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	doc := make(bson.D, 0, len(m))
	for _, k := range keys {
		cv, err := canonicalizeValue(m[k])
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		doc = append(doc, bson.E{Key: k, Value: cv})
	}
	return doc, nil
}

func canonicalizeSlice(s []interface{}) ([]interface{}, error) {
	out := make([]interface{}, len(s))
	for i, item := range s {
		cv, err := canonicalizeValue(item)
		if err != nil {
			return nil, fmt.Errorf("index %d: %w", i, err)
		}
		out[i] = cv
	}
	return out, nil
}
