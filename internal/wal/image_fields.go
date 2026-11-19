package wal

import (
	"bytes"
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

// ImageRole identifies the stored bytes being considered for editing.
type ImageRole uint8

const (
	PostImage ImageRole = iota
	SnapshotImage
)

// DecodedImage carries the read's value and the BSON that supplied it.
// The decoder borrows the input bytes read-only. The caller keeps them
// unchanged until the read finishes. Use a DecodedImage through its pointer.
type DecodedImage struct {
	raw   bson.Raw
	value bson.M
	key   string
	idErr error
}

// DecodeImage uses the first top-level _id, including its compound BSON order.
// Key is available even on a value decoding error, so a reader can report an
// ambiguous legacy envelope before a malformed post-image.
func DecodeImage(raw bson.Raw) (*DecodedImage, error) {
	id, idErr := DocumentIDFromImage(raw)
	d := &DecodedImage{raw: raw, idErr: idErr}
	if idErr == nil {
		d.key = DocumentIDString(id)
	}
	if err := UnmarshalDocument(raw, &d.value); err != nil {
		d.raw = nil
		return d, err
	}
	if idErr == nil {
		d.value["_id"] = id
	}
	return d, nil
}

// DecodedValue wraps a value that has no original BSON.
func DecodedValue(value bson.M) *DecodedImage {
	return &DecodedImage{value: value}
}

func (d *DecodedImage) Key() (string, error) { return d.key, d.idErr }

// TakeValue releases the mutable map. Subsequent edits must compare any proposed
// source independently, since this map can now have different contents.
func (d *DecodedImage) TakeValue() bson.M {
	d.raw = nil
	return d.value
}

// TrustedImage releases the original bytes once, if their encoding is canonical
// in the stored field order and the image is trusted for its role.
func (d *DecodedImage) TrustedImage(role ImageRole, entry *Entry) (bson.Raw, *DecodedImage) {
	if d == nil || d.raw == nil || d.idErr != nil || (role == PostImage && (entry == nil || entry.UnverifiedPostImage())) {
		return nil, nil
	}
	raw := d.raw
	d.raw = nil
	if !canonicalEditImage(raw) {
		return nil, nil
	}
	return raw, d
}

func canonicalEditImage(raw bson.Raw) bool {
	// Decode into ordered documents at every depth, including code scopes.
	// Re-encoding checks every value and array index, but preserves repeated
	// names, so those must also be checked independently.
	var fields bson.D
	if err := UnmarshalDocument(raw, &fields); err != nil {
		return false
	}
	encoded, err := bson.Marshal(fields)
	return err == nil && bytes.Equal(raw, encoded) && !HasDuplicateImageFields(raw, true)
}

// NormalizeMergeImage collapses fields for ordinary merge plans. Refuse a
// compound _id whose own fields would change the encoded document key.
func NormalizeMergeImage(raw bson.Raw) (bson.Raw, bool, error) {
	if raw == nil || !HasDuplicateImageFields(raw, true) {
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
		compoundKey := root && key == "_id"
		if value.Type == bson.TypeEmbeddedDocument || value.Type == bson.TypeArray {
			nested, rebuilt, err := collapseFields(value.Value, value.Type == bson.TypeArray, false)
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

type imageFieldScan struct {
	arrayKeys bool
}

func (s *imageFieldScan) hasDuplicateFields(document bson.Raw) bool {
	elements, err := document.Elements()
	if err != nil {
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
		_, scope, ok := value.CodeWithScopeOK()
		return ok && s.hasDuplicateFields(scope)
	}
	return false
}
