package wal

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const documentKeyPrefix = "~argon:bson:"

// DocumentIDString keeps ordinary string IDs and ObjectIDs human-readable,
// and uses a lossless BSON envelope for every ID which could collide with
// another BSON type. The prefix is itself escaped when it occurs in a string.
func DocumentIDString(id interface{}) string {
	switch v := id.(type) {
	case primitive.ObjectID:
		return v.Hex()
	case string:
		_, hexErr := primitive.ObjectIDFromHex(v)
		if v != "" && hexErr != nil && !strings.HasPrefix(v, documentKeyPrefix) && !strings.HasPrefix(v, "{") {
			return v
		}
	}
	raw, err := MarshalDocument(bson.D{{Key: "i", Value: id}})
	if err != nil {
		return fmt.Sprintf("%sunsupported:%T:%v", documentKeyPrefix, id, id)
	}
	return documentKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// RawDocumentID returns the exact typed value in a WAL key. Modern BSON
// envelopes already contain its bytes, including the order of compound IDs.
func RawDocumentID(key string) (bson.RawValue, error) {
	if strings.HasPrefix(key, documentKeyPrefix) {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, documentKeyPrefix))
		if err != nil {
			return bson.RawValue{}, fmt.Errorf("invalid BSON document key: %w", err)
		}
		return bson.Raw(raw).LookupErr("i")
	}
	id, err := DocumentIDValue(key)
	if err != nil {
		return bson.RawValue{}, err
	}
	t, data, err := bson.MarshalValue(id)
	return bson.RawValue{Type: t, Value: data}, err
}

// DocumentIDValue reverses a current key without guessing the type of an
// escaped string. Legacy extended-JSON numeric keys are accepted for plans
// created before typed keys were introduced.
func DocumentIDValue(key string) (interface{}, error) {
	if strings.HasPrefix(key, documentKeyPrefix) {
		raw, err := RawDocumentID(key)
		if err != nil {
			return nil, err
		}
		var id any
		err = UnmarshalValue(raw, &id)
		return id, err
	}
	if oid, err := primitive.ObjectIDFromHex(key); err == nil {
		return oid, nil
	}
	if strings.HasPrefix(key, "{") {
		var wrapped struct {
			ID any `bson:"i"`
		}
		if err := bson.UnmarshalExtJSON([]byte(`{"i":`+key+`}`), true, &wrapped); err == nil {
			return wrapped.ID, nil
		}
	}
	return key, nil
}

// LegacyDocumentIDString is only for reading pre-key_v history.
func LegacyDocumentIDString(id interface{}) string {
	switch v := id.(type) {
	case primitive.ObjectID:
		return v.Hex()
	case string:
		return v
	default:
		raw, err := bson.MarshalExtJSON(bson.M{"i": id}, true, false)
		if err != nil {
			return fmt.Sprint(id)
		}
		return string(raw[5 : len(raw)-1])
	}
}

// NormalizeDocumentKey upgrades an entry using its authoritative document
// image. A delete without an image must already carry a current key.
func (e *Entry) NormalizeDocumentKey() error {
	if !e.IsData() {
		return nil
	}
	image := e.PostImage
	if e.Operation == OpDelete {
		image = e.PreImage
	}
	e.DocumentKeyVersion = 1
	if len(image) > 0 {
		id, err := documentIDFromImage(image, keyRegistry)
		if errors.Is(err, errOldBinaryKey) {
			id, err = DocumentIDFromImage(image)
			e.DocumentKeyVersion = exactOldBinaryVersion
		}
		if err == nil {
			e.DocumentID = DocumentIDString(id)
			return nil
		}
		if image.Validate() != nil {
			return err
		}
	}
	e.DocumentKeyVersion = storedKeyVersion(e.DocumentID)
	return nil
}

// An image-less delete supplies only the stored key. For a canonical scalar
// envelope its first twelve bytes identify the BSON type and binary subtype,
// regardless of the size of a string or binary payload.
func storedKeyVersion(key string) int {
	encoded, ok := strings.CutPrefix(key, documentKeyPrefix)
	if !ok {
		return 1
	}
	var header [12]byte
	n, err := base64.RawURLEncoding.Decode(header[:], []byte(encoded[:min(len(encoded), 16)]))
	if err != nil || n < 7 {
		return 1
	}
	if header[5] == 'i' && header[6] == 0 {
		switch bsontype.Type(header[4]) {
		case bson.TypeBinary:
			if n < 12 || header[11] != bson.TypeBinaryBinaryOld {
				return 1
			}
		case bson.TypeEmbeddedDocument, bson.TypeArray, bson.TypeCodeWithScope:
			// The binary can occur anywhere inside the compound value.
		default:
			return 1
		}
	}
	value, err := RawDocumentID(key)
	if err == nil && IsOldBinaryID(value) {
		return exactOldBinaryVersion
	}
	return 1
}

// IsOldBinaryKey examines the BSON type in a stored key envelope.
func IsOldBinaryKey(key string) bool {
	return storedKeyVersion(key) == exactOldBinaryVersion
}

// IsOldBinaryID detects subtype 2 at any depth of an _id, including a
// compound key. Scalar values of other types need no decoding.
func IsOldBinaryID(value bson.RawValue) bool {
	switch value.Type {
	case bson.TypeBinary:
		if len(value.Value) < 5 || value.Value[4] != bson.TypeBinaryBinaryOld {
			return false
		}
	case bson.TypeEmbeddedDocument, bson.TypeArray, bson.TypeCodeWithScope:
	default:
		return false
	}
	var id any
	return errors.Is(value.UnmarshalWithRegistry(keyRegistry, &id), errOldBinaryKey)
}

// DocumentIDFromImage preserves embedded-document field order, unlike
// decoding the enclosing document into bson.M and reading its _id field.
func DocumentIDFromImage(image bson.Raw) (interface{}, error) {
	return documentIDFromImage(image, orderedRegistry)
}

func documentIDFromImage(image bson.Raw, registry *bsoncodec.Registry) (any, error) {
	value, err := image.LookupErr("_id")
	if err != nil {
		return nil, fmt.Errorf("document image has no _id: %w", err)
	}
	var id interface{}
	if err := value.UnmarshalWithRegistry(registry, &id); err != nil {
		return nil, err
	}
	return id, nil
}
