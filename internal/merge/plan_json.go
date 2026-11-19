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
func (p *Plan) CLIJSON() (json.RawMessage, error) {
	out := struct {
		*Plan
		Changes   []json.RawMessage `json:"changes"`
		Conflicts []json.RawMessage `json:"conflicts"`
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
	for _, image := range images {
		if p.FormatVersion >= orderedPlanVersion {
			if image.raw != nil {
				fields = append(fields, bson.E{Key: image.name, Value: image.raw})
			}
			continue
		}
		if image.doc == nil {
			continue
		}
		doc := image.doc
		if _, ok := doc["_id"]; ok && p.FormatVersion >= 2 {
			doc = maps.Clone(doc)
			doc["_id"] = id
		}
		fields = append(fields, bson.E{Key: image.name, Value: doc})
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
	var paths []string
	count := func(images []planImage) error {
		for _, image := range images {
			if image.raw == nil {
				continue
			}
			var doc bson.D
			if err := bson.Unmarshal(image.raw, &doc); err != nil {
				return err
			}
			extendedJSONValue(doc, image.name, &paths)
		}
		return nil
	}
	for _, c := range p.Changes {
		if err := count(c.cliImages()); err != nil {
			return 0, err
		}
	}
	for _, c := range p.Conflicts {
		if err := count(c.cliImages()); err != nil {
			return 0, err
		}
	}
	return len(paths), nil
}
