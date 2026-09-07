package wal

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
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
	raw, err := bson.Marshal(bson.D{{Key: "i", Value: orderedID(id)}})
	if err != nil {
		return fmt.Sprintf("%sunsupported:%T:%v", documentKeyPrefix, id, id)
	}
	return documentKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// DocumentIDValue reverses a current key without guessing the type of an
// escaped string. Legacy extended-JSON numeric keys are accepted for plans
// created before typed keys were introduced.
func DocumentIDValue(key string) (interface{}, error) {
	var raw []byte
	var err error
	var wrapped struct {
		ID interface{} `bson:"i"`
	}
	if strings.HasPrefix(key, documentKeyPrefix) {
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, documentKeyPrefix))
		if err == nil {
			err = bson.Unmarshal(raw, &wrapped)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid BSON document key: %w", err)
		}
		return wrapped.ID, nil
	}
	if oid, err := primitive.ObjectIDFromHex(key); err == nil {
		return oid, nil
	}
	if strings.HasPrefix(key, "{") {
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
	if len(image) > 0 {
		id, err := DocumentIDFromImage(image)
		if err == nil {
			e.DocumentID = DocumentIDString(id)
		} else if image.Validate() != nil {
			return err
		}
	}
	e.DocumentKeyVersion = 1
	return nil
}

// DocumentIDFromImage preserves embedded-document field order, unlike
// decoding the enclosing document into bson.M and reading its _id field.
func DocumentIDFromImage(image bson.Raw) (interface{}, error) {
	value, err := image.LookupErr("_id")
	if err != nil {
		return nil, fmt.Errorf("document image has no _id: %w", err)
	}
	var id interface{}
	if err := value.Unmarshal(&id); err != nil {
		return nil, err
	}
	return id, nil
}

func orderedID(value interface{}) interface{} {
	switch v := value.(type) {
	case bson.M:
		return orderedIDMap(v)
	case map[string]interface{}:
		return orderedIDMap(v)
	case bson.D:
		d := make(bson.D, len(v))
		for i, e := range v {
			d[i] = bson.E{Key: e.Key, Value: orderedID(e.Value)}
		}
		return d
	case bson.A:
		a := make(bson.A, len(v))
		for i, e := range v {
			a[i] = orderedID(e)
		}
		return a
	default:
		return value
	}
}

func orderedIDMap(m map[string]interface{}) bson.D {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := make(bson.D, 0, len(keys))
	for _, k := range keys {
		d = append(d, bson.E{Key: k, Value: orderedID(m[k])})
	}
	return d
}
