package merge

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CLIJSON renders BSON document values as canonical Extended JSON v2 while
// retaining the ordinary JSON types of the plan's metadata.
func (p *Plan) CLIJSON(targetFollowed bool) (json.RawMessage, error) {
	out := struct {
		*Plan
		Changes        []json.RawMessage `json:"changes"`
		Conflicts      []json.RawMessage `json:"conflicts"`
		ApprovalDigest string            `json:"approval_digest,omitempty"`
	}{Plan: p}
	if p.Changes != nil {
		out.Changes = make([]json.RawMessage, 0, len(p.Changes))
	}
	for _, c := range p.Changes {
		fields := bson.D{}
		if c.Delete {
			fields = append(fields, bson.E{Key: "delete", Value: true})
		}
		item, err := p.itemJSON(c.Collection, c.DocumentID, c.ID, fields, c.SortedImages, c.cliImages())
		if err != nil {
			return nil, err
		}
		out.Changes = append(out.Changes, item)
	}
	if p.Conflicts != nil {
		out.Conflicts = make([]json.RawMessage, 0, len(p.Conflicts))
	}
	for _, c := range p.Conflicts {
		item, err := p.itemJSON(c.Collection, c.DocumentID, nil, nil, c.SortedImages, c.cliImages())
		if err != nil {
			return nil, err
		}
		out.Conflicts = append(out.Conflicts, item)
	}
	if targetFollowed && p.FollowedDatabase != "" && p.Status == StatusPending && p.approvalEligible() {
		digest, err := ApprovalDigest(p)
		if err != nil {
			return nil, err
		}
		out.ApprovalDigest = digest
	}
	return json.Marshal(out)
}

type planImage struct {
	name string
	doc  bson.M
	raw  bson.Raw
}

func (c Change) cliImages() []planImage {
	return []planImage{{"document", c.Document, c.DocumentRaw}, {"before", c.Before, c.BeforeRaw}}
}

func (c Conflict) cliImages() []planImage {
	return []planImage{{"base", c.Base, c.BaseRaw}, {"ours", c.Ours, c.OursRaw}, {"theirs", c.Theirs, c.TheirsRaw}}
}

// Loaded plans have the persisted BSON even when their format predates
// ordered images. In-memory legacy plans can still carry only a map.
func (image planImage) bsonForCLI(format int, key string) (bson.Raw, error) {
	if image.raw != nil || image.doc == nil || format >= orderedPlanVersion {
		return image.raw, nil
	}
	doc := image.doc
	if _, ok := doc["_id"]; ok && format >= 2 {
		id, err := wal.DocumentIDValue(key)
		if err != nil {
			return nil, err
		}
		doc = maps.Clone(doc)
		doc["_id"] = id
	}
	return wal.MarshalDocument(doc)
}

func (p *Plan) itemJSON(collection, key string, documentKey any, fields bson.D, sortedImages []string, images []planImage) (json.RawMessage, error) {
	var id any = key
	if p.FormatVersion >= 2 {
		var err error
		id, err = wal.DocumentIDValue(key)
		if err != nil {
			return nil, fmt.Errorf("document %s/%s: %w", collection, key, err)
		}
	} else if documentKey != nil {
		id = documentKey
	} else {
		// The string alone is ambiguous in older plans. Use a typed image
		// when there is one and keep the stored string otherwise.
		for _, image := range images {
			if value, ok := image.doc["_id"]; ok {
				id = value
				break
			}
		}
	}
	fields = append(fields, bson.E{Key: "collection", Value: collection}, bson.E{Key: "document_id", Value: id})
	if documentKey != nil {
		fields = append(fields, bson.E{Key: "document_key", Value: id})
	}
	if len(sortedImages) > 0 {
		fields = append(fields, bson.E{Key: "sorted_images", Value: sortedImages})
	}
	var exact bson.D
	for _, image := range images {
		raw, err := image.bsonForCLI(p.FormatVersion, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		markers, err := imageExactBytes(raw, image.name)
		if err != nil {
			return nil, err
		}
		exact = append(exact, markers...)
		fields = append(fields, bson.E{Key: image.name, Value: raw})
	}
	raw, err := wal.MarshalDocument(fields)
	if err != nil {
		return nil, err
	}
	var item bson.D
	if err := bson.Unmarshal(raw, &item); err != nil {
		return nil, err
	}

	var paths []string
	for i, field := range item {
		// Only image paths are marked. IDs can also contain DBPointers.
		var markers *[]string
		switch field.Key {
		case "document", "before", "base", "ours", "theirs":
			markers = &paths
		}
		item[i].Value = extendedJSONValue(field.Value, field.Key, markers)
	}
	if len(paths) > 0 {
		item = append(item, bson.E{Key: "literal_dollar_paths", Value: paths})
	}
	if len(exact) > 0 {
		item = append(item, bson.E{Key: "exact_bytes", Value: exact})
	}
	return bson.MarshalExtJSON(item, true, false)
}

var pathEscapes = strings.NewReplacer(`\`, `\\`, ".", `\.`)

// extendedJSONValue walks a freshly decoded ordered image. Mark BSON documents
// before constructing any Extended JSON wrappers, including inside code scopes.
func extendedJSONValue(value any, path string, paths *[]string) any {
	switch v := value.(type) {
	case bson.D:
		marked := false
		for i, field := range v {
			if paths != nil && !marked && strings.HasPrefix(field.Key, "$") {
				*paths = append(*paths, path)
				marked = true
			}
			v[i].Value = extendedJSONValue(field.Value, path+"."+pathEscapes.Replace(field.Key), paths)
		}
	case bson.A:
		for i, element := range v {
			v[i] = extendedJSONValue(element, path+"."+strconv.Itoa(i), paths)
		}
	case primitive.CodeWithScope:
		v.Scope = extendedJSONValue(v.Scope, path+".$scope", paths)
		return v
	case primitive.DBPointer:
		// The driver's DBPointer writer interpolates the namespace without
		// escaping it. An ordinary string element gets full JSON escaping.
		return bson.D{{Key: "$dbPointer", Value: bson.D{
			{Key: "$ref", Value: v.DB}, {Key: "$id", Value: v.Pointer},
		}}}
	}
	return value
}

// LiteralDollarPathCount counts all marked paths across the prepared images.
func (p *Plan) LiteralDollarPathCount() (int, error) {
	return p.countImagePaths(func(raw bson.Raw, name string) (int, error) {
		var doc bson.D
		if err := bson.Unmarshal(raw, &doc); err != nil {
			return 0, err
		}
		var paths []string
		extendedJSONValue(doc, name, &paths)
		return len(paths), nil
	})
}

func (p *Plan) countImagePaths(count func(bson.Raw, string) (int, error)) (int, error) {
	total := 0
	visit := func(key string, images []planImage) error {
		for _, image := range images {
			raw, err := image.bsonForCLI(p.FormatVersion, key)
			if err != nil {
				return err
			}
			if raw == nil {
				continue
			}
			n, err := count(raw, image.name)
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	}
	for _, c := range p.Changes {
		if err := visit(c.DocumentID, c.cliImages()); err != nil {
			return 0, err
		}
	}
	for _, c := range p.Conflicts {
		if err := visit(c.DocumentID, c.cliImages()); err != nil {
			return 0, err
		}
	}
	return total, nil
}
