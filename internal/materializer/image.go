package materializer

import (
	"bytes"
	"sort"

	"github.com/argon-lab/argon/v2/internal/mongoexpr"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// EditImage checks a separately supplied candidate against a decoded value.
func EditImage(doc bson.M, raw bson.Raw, role wal.ImageRole, entry *wal.Entry) (bson.Raw, error) {
	if doc == nil {
		return nil, nil
	}
	candidate, err := wal.DecodeImage(raw)
	if err != nil {
		candidate = nil
	}
	return editImage(wal.DecodedValue(doc), candidate, role, entry)
}

// editImage decorates a decoded value with stored BSON field order.
//
// Decoded collection replay determines existence and value at the requested
// position. A stored WAL post-image or snapshot frame supplies its bytes only
// if it identifies the same document, decodes cleanly, is trusted for that role
// and matches the decoded value under the merge comparison, including _id.
// Repeated data fields keep their last value at their first position.
// The first top-level _id and JavaScript code scopes retain their BSON identity
// and order.
// attached_unverified applies only to the post-image. A follower's current
// image can supply order. Without a matching image, encode the decoded value
// with _id first and map fields in name order at each depth.
func editImage(authority, candidate *wal.DecodedImage, role wal.ImageRole, entry *wal.Entry) (bson.Raw, error) {
	if image, ok := matchingEditImage(authority, candidate, role, entry); ok {
		return image, nil
	}
	doc := authority.TakeValue()
	if doc == nil {
		return nil, nil
	}
	fields := make(bson.D, 0, len(doc))
	if id, ok := doc["_id"]; ok {
		fields = append(fields, bson.E{Key: "_id", Value: id})
	}
	start := len(fields)
	for k, v := range doc {
		if k != "_id" {
			fields = append(fields, bson.E{Key: k, Value: v})
		}
	}
	rest := fields[start:]
	sort.Slice(rest, func(i, j int) bool { return rest[i].Key < rest[j].Key })
	return wal.MarshalDocument(fields)
}

func matchingEditImage(authority, source *wal.DecodedImage, role wal.ImageRole, entry *wal.Entry) (bson.Raw, bool) {
	image, selected := source.TrustedImage(role, entry)
	if selected == nil {
		return nil, false
	}
	// The same decoder result and unchanged input bytes establish equality of
	// every value and _id. Independently selected or rebuilt images still
	// require a canonical comparison.
	if authority == selected {
		return image, true
	}
	doc, candidate := authority.TakeValue(), selected.TakeValue()
	expectedID, hasID := doc["_id"]
	if !hasID || wal.DocumentIDString(candidate["_id"]) != wal.DocumentIDString(expectedID) {
		return nil, false
	}
	expected, err := canonicalEditValue(doc)
	if err != nil {
		return nil, false
	}
	actual, err := canonicalEditValue(candidate)
	return image, err == nil && bytes.Equal(expected, actual)
}

// Use merge's BSON value comparison. Its canonicalization leaves JavaScript
// code scopes as ordered scalars. A scope decoded into a bson.M document
// can itself contain Go maps, which also need deterministic BSON encoding.
func canonicalEditValue(doc bson.M) (bson.Raw, error) {
	value, err := mongoexpr.Canonicalize(doc)
	if err != nil {
		return nil, err
	}
	return wal.MarshalDocument(value)
}
