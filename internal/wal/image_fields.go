package wal

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"go.mongodb.org/mongo-driver/bson"
)

// HasDuplicateImageFields checks capture images, including fields inside code
// scopes. With includeArrayKeys it also checks an array's stored indices.
func HasDuplicateImageFields(document bson.Raw, includeArrayKeys bool) bool {
	scan := imageFieldScan{arrayKeys: includeArrayKeys}
	return scan.hasDuplicateFields(document)
}

// NormalizeEditImage keeps the values used by decoded replay in their original
// field positions. A JavaScript code scope is a scalar and keeps its bytes.
// Replay uses the first _id verbatim, including the order of a compound key.
func NormalizeEditImage(raw bson.Raw) (bson.Raw, bool, error) {
	if raw == nil {
		return raw, false, nil
	}
	scan := imageFieldScan{arrayKeys: true, edit: true}
	repeated := scan.hasDuplicateFields(raw)
	if scan.needsDecode || repeated {
		// The duplicate scan stops at the first repeated field. Decode these
		// images in full so invalid values, even overwritten ones, produce
		// the same error as replay. Clean, ordinary documents stay raw.
		var doc bson.M
		if err := UnmarshalDocument(raw, &doc); err != nil {
			return nil, false, err
		}
	}
	if !repeated {
		return raw, false, nil
	}
	return collapseFields(raw, false, true, false)
}

// NormalizeMergeImage collapses fields for ordinary merge plans. Refuse a
// compound _id whose own fields would change the encoded document key.
func NormalizeMergeImage(raw bson.Raw) (bson.Raw, bool, error) {
	if raw == nil || !HasDuplicateImageFields(raw, true) {
		return raw, false, nil
	}
	return collapseFields(raw, false, true, true)
}

// A document keeps each key's first position and last value. Arrays expose
// all their values by position, even if the original BSON repeated an index.
func collapseFields(raw bson.Raw, array, root, rejectDuplicateID bool) (bson.Raw, bool, error) {
	elements, err := raw.Elements()
	if err != nil {
		return nil, false, err
	}
	fields := make(bson.D, 0, len(elements))
	positions := make(map[string]int, len(elements))
	changed := false
	for i, elem := range elements {
		key, value := elem.Key(), elem.Value()
		compoundKey := root && key == "_id"
		if (!compoundKey || rejectDuplicateID) && (value.Type == bson.TypeEmbeddedDocument || value.Type == bson.TypeArray) {
			nested, rebuilt, err := collapseFields(value.Value, value.Type == bson.TypeArray, false, rejectDuplicateID)
			if err != nil {
				return nil, false, err
			}
			if rebuilt && compoundKey {
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
			// first _id element. Preserve that identity in the image.
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

// Capture and write-back inspect code scopes too. Edit reads leave their
// fields alone, but check that the full image can be decoded.
type imageFieldScan struct {
	arrayKeys, edit, needsDecode bool
}

func (s *imageFieldScan) hasDuplicateFields(document bson.Raw) bool {
	if s.edit && (len(document) < 5 || uint64(binary.LittleEndian.Uint32(document)) != uint64(len(document)) || document[len(document)-1] != 0) {
		s.needsDecode = true
		return false
	}
	elements, err := document.Elements()
	if err != nil {
		s.needsDecode = true
		return false
	}
	var seen map[string]struct{}
	for i, element := range elements {
		key := element.Key()
		// Most documents are small: compare directly before paying for a map.
		if seen == nil {
			for _, earlier := range elements[:i] {
				if earlier.Key() == key {
					return true
				}
			}
			if i >= 16 {
				seen = make(map[string]struct{}, len(elements))
				for _, earlier := range elements[:i+1] {
					seen[earlier.Key()] = struct{}{}
				}
			}
		} else {
			if _, dup := seen[key]; dup {
				return true
			}
			seen[key] = struct{}{}
		}
		if s.valueHasDuplicateFields(element.Value()) {
			return true
		}
	}
	return false
}

func (s *imageFieldScan) valueHasDuplicateFields(value bson.RawValue) bool {
	switch value.Type {
	case bson.TypeEmbeddedDocument:
		return s.hasDuplicateFields(value.Value)
	case bson.TypeArray:
		if s.arrayKeys {
			return s.hasDuplicateFields(value.Value)
		}
		items, err := bson.Raw(value.Value).Values()
		if err == nil {
			for _, item := range items {
				if s.valueHasDuplicateFields(item) {
					return true
				}
			}
		}
	case bson.TypeCodeWithScope:
		if !s.edit {
			_, scope, ok := value.CodeWithScopeOK()
			return ok && s.hasDuplicateFields(scope)
		}
	}
	if s.edit && !s.needsDecode {
		s.needsDecode = needsValueDecode(value)
	}
	return false
}

// Raw.Elements checks type widths. A few scalar decoders also check their
// contents, such as the second length in an old binary or the scope of code.
func needsValueDecode(value bson.RawValue) bool {
	switch value.Type {
	case bson.TypeBinary:
		return len(value.Value) < 5 || value.Value[4] == bson.TypeBinaryBinaryOld
	case bson.TypeBoolean:
		return len(value.Value) != 1 || value.Value[0] > 1
	case bson.TypeString, bson.TypeSymbol, bson.TypeJavaScript:
		return len(value.Value) <= 4 || value.Value[len(value.Value)-1] != 0
	case bson.TypeCodeWithScope, bson.TypeDBPointer:
		return true
	}
	return false
}
