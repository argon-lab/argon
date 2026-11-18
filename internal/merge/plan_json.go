package merge

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
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
		item, err := p.itemJSON(c.Collection, c.DocumentID, c.ID, fields,
			planImage{"document", c.Document}, planImage{"before", c.Before})
		if err != nil {
			return nil, err
		}
		out.Changes = append(out.Changes, item)
	}
	if p.Conflicts != nil {
		out.Conflicts = make([]json.RawMessage, 0, len(p.Conflicts))
	}
	for _, c := range p.Conflicts {
		item, err := p.itemJSON(c.Collection, c.DocumentID, nil, nil,
			planImage{"base", c.Base}, planImage{"ours", c.Ours}, planImage{"theirs", c.Theirs})
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
}

func (p *Plan) itemJSON(collection, key string, documentKey any, fields bson.D, images ...planImage) (json.RawMessage, error) {
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
	for _, image := range images {
		if image.doc == nil {
			continue
		}
		doc := image.doc
		if _, ok := doc["_id"]; ok && p.FormatVersion >= 2 {
			// Loading the image into a map loses the order of compound IDs.
			doc = maps.Clone(doc)
			doc["_id"] = id
		}
		fields = append(fields, bson.E{Key: image.name, Value: doc})
	}
	raw, err := wal.MarshalDocument(fields)
	if err != nil {
		return nil, err
	}
	return bson.MarshalExtJSON(raw, true, false)
}
