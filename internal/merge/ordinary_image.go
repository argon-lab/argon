package merge

import (
	"fmt"
	"strconv"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"go.mongodb.org/mongo-driver/bson"
)

// Ordinary plans publish the values exposed by materialization. An ordered
// code scope is a scalar here: its encoded fields, including repeats, survive.
func ordinaryImage(raw bson.Raw) (bson.Raw, bool, error) {
	if raw == nil || !ingest.HasDuplicateKeys(raw) {
		return raw, false, nil
	}
	return collapseFields(raw, false, true)
}

// A document keeps each key's first position and last value. Arrays expose
// all their values by position, even if the original BSON repeated an index.
func collapseFields(raw bson.Raw, array, root bool) (bson.Raw, bool, error) {
	elements, err := raw.Elements()
	if err != nil {
		return nil, false, err
	}
	fields := make(bson.D, 0, len(elements))
	positions := make(map[string]int, len(elements))
	changed := false
	for i, elem := range elements {
		key, value := elem.Key(), elem.Value()
		if value.Type == bson.TypeEmbeddedDocument || value.Type == bson.TypeArray {
			nested, rebuilt, err := collapseFields(value.Value, value.Type == bson.TypeArray, false)
			if err != nil {
				return nil, false, err
			}
			if rebuilt && root && key == "_id" {
				return nil, false, fmt.Errorf("duplicate fields in compound _id cannot be merged without changing the document key")
			}
			value.Value = nested
			changed = changed || rebuilt
		}
		if array {
			index := strconv.Itoa(i)
			changed = changed || key != index
			key = index
		}
		if pos, exists := positions[key]; exists {
			// History and materialization derive the document key from the
			// first _id element. Preserve that identity in the merged image.
			if !root || key != "_id" {
				fields[pos].Value = value
			}
			changed = true
		} else {
			positions[key] = len(fields)
			fields = append(fields, bson.E{Key: key, Value: value})
		}
	}
	if !changed {
		return raw, false, nil
	}
	result, err := bson.Marshal(fields)
	return result, true, err
}
