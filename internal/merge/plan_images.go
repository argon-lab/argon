package merge

import (
	"fmt"
	"slices"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	orderedPlanVersion = 3
	exactBytesVersion  = 1
)

// The BSON fields contain the images themselves. The maps serve callers that
// compare document values or use the ordinary JSON representation of a plan.
type changeBSON Change
type conflictBSON Conflict

func (c Change) MarshalBSON() ([]byte, error) {
	if err := encodeMissingImage(&c.DocumentRaw, c.Document, "document", &c.SortedImages); err != nil {
		return nil, err
	}
	if err := encodeMissingImage(&c.BeforeRaw, c.Before, "before", &c.SortedImages); err != nil {
		return nil, err
	}
	return bson.Marshal(changeBSON(c))
}

func (c *Change) UnmarshalBSON(data []byte) error {
	if err := bson.Unmarshal(data, (*changeBSON)(c)); err != nil {
		return err
	}
	if err := decodeImage(c.DocumentRaw, &c.Document); err != nil {
		return err
	}
	return decodeImage(c.BeforeRaw, &c.Before)
}

func (c Conflict) MarshalBSON() ([]byte, error) {
	if err := encodeMissingImage(&c.BaseRaw, c.Base, "base", &c.SortedImages); err != nil {
		return nil, err
	}
	if err := encodeMissingImage(&c.OursRaw, c.Ours, "ours", &c.SortedImages); err != nil {
		return nil, err
	}
	if err := encodeMissingImage(&c.TheirsRaw, c.Theirs, "theirs", &c.SortedImages); err != nil {
		return nil, err
	}
	return bson.Marshal(conflictBSON(c))
}

func (c *Conflict) UnmarshalBSON(data []byte) error {
	if err := bson.Unmarshal(data, (*conflictBSON)(c)); err != nil {
		return err
	}
	if err := decodeImage(c.BaseRaw, &c.Base); err != nil {
		return err
	}
	if err := decodeImage(c.OursRaw, &c.Ours); err != nil {
		return err
	}
	return decodeImage(c.TheirsRaw, &c.Theirs)
}

func encodeMissingImage(raw *bson.Raw, doc bson.M, name string, sorted *[]string) error {
	if *raw == nil && doc != nil {
		image, err := wal.MarshalDocument(doc)
		if err != nil {
			return err
		}
		*raw = image
		if !slices.Contains(*sorted, name) {
			*sorted = append(slices.Clone(*sorted), name)
		}
	}
	return nil
}

func decodeImage(raw bson.Raw, doc *bson.M) error {
	if raw == nil {
		*doc = nil
		return nil
	}
	return bson.Unmarshal(raw, doc)
}

// PreparePlan computes a proposal with the ordered BSON images that SavePlan
// will persist. CLI callers can encode their output before saving the plan.
func (s *Service) PreparePlan(sourceBranchID string) (*Plan, error) {
	plan, err := s.Compute(sourceBranchID)
	if err != nil {
		return nil, err
	}
	if err := s.preparePlanImages(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// Each image has a document in the computed state and a destination in the
// plan. Read a given side's keys together so a large plan does not require a
// separate history query for each key.
type imageRequest struct {
	key    string
	name   string
	doc    bson.M
	raw    *bson.Raw
	sorted *[]string
}

type imageRequests map[string][]imageRequest

func (r imageRequests) add(collection, key, name string, doc bson.M, raw *bson.Raw, sorted *[]string) {
	if doc != nil {
		r[collection] = append(r[collection], imageRequest{key, name, doc, raw, sorted})
	}
}

func (s *Service) preparePlanImages(plan *Plan) error {
	source, target, err := s.endpoints(plan.SourceBranchID)
	if err != nil {
		return err
	}

	theirs, ours, base := imageRequests{}, imageRequests{}, imageRequests{}
	for i := range plan.Changes {
		c := &plan.Changes[i]
		theirs.add(c.Collection, c.DocumentID, "document", c.Document, &c.DocumentRaw, &c.SortedImages)
		ours.add(c.Collection, c.DocumentID, "before", c.Before, &c.BeforeRaw, &c.SortedImages)
	}
	for i := range plan.Conflicts {
		c := &plan.Conflicts[i]
		theirs.add(c.Collection, c.DocumentID, "theirs", c.Theirs, &c.TheirsRaw, &c.SortedImages)
		ours.add(c.Collection, c.DocumentID, "ours", c.Ours, &c.OursRaw, &c.SortedImages)
		base.add(c.Collection, c.DocumentID, "base", c.Base, &c.BaseRaw, &c.SortedImages)
	}
	if err := s.readPlanImages(source, plan.BaseLSN+1, plan.SourceHead, theirs, target.IsLive() || target.IsFollower()); err != nil {
		return err
	}
	if err := s.readPlanImages(target, 0, plan.TargetHead, ours, false); err != nil {
		return err
	}
	if err := s.readPlanImages(source, 0, plan.BaseLSN, base, false); err != nil {
		return err
	}
	plan.FormatVersion = orderedPlanVersion
	plan.ExactBytesVersion = exactBytesVersion
	return nil
}

func (s *Service) readPlanImages(branch *wal.Branch, fromLSN, toLSN int64, requests imageRequests, idFirst bool) error {
	for collection, images := range requests {
		keys := make([]string, len(images))
		for i := range images {
			keys[i] = images[i].key
		}
		entries, err := s.materializer.DocumentImageEntries(branch, collection, keys, fromLSN, toLSN)
		if err != nil {
			return err
		}
		for _, image := range images {
			if err := image.store(entries[image.key], idFirst); err != nil {
				return fmt.Errorf("merge image %s for %s/%s: %w", image.name, collection, image.key, err)
			}
		}
	}
	return nil
}

func (image imageRequest) store(entry *wal.Entry, idFirst bool) error {
	id, err := wal.DocumentIDValue(image.key)
	if err != nil {
		return err
	}
	// The typed key preserves the order of a compound ID in the map view.
	image.doc["_id"] = id
	var raw bson.Raw
	// Save verified duplicates in ordered plans so the full document can be displayed.
	// Ordinary apply of older plans retains its historical single-value fallback.
	if entry != nil && entry.Operation == wal.OpPut && len(entry.PostImage) > 0 &&
		entry.Metadata["capture_images"] != "attached_unverified" &&
		(image.sorted != nil || !ingest.HasDuplicateFields(entry.PostImage)) {
		var doc bson.M
		if err := bson.Unmarshal(entry.PostImage, &doc); err != nil {
			return err
		}
		changed, err := docsUnequal(image.doc, doc)
		if err != nil {
			return err
		}
		storedID, err := wal.DocumentIDFromImage(entry.PostImage)
		if !changed && err == nil && wal.DocumentIDString(storedID) == image.key {
			raw = entry.PostImage
		}
	}
	if raw == nil {
		doc := image.doc
		if image.sorted != nil {
			// BSON code scopes retain ordered documents even after decoding
			// the enclosing image into a map. Sort those fields as well.
			doc = mapImageValue(doc).(bson.M)
			doc["_id"] = id
		}
		raw, err = wal.MarshalDocument(doc)
		if err != nil {
			return err
		}
		if image.sorted != nil {
			*image.sorted = append(*image.sorted, image.name)
		}
	}
	if idFirst {
		raw, err = withIDFirst(raw)
		if err != nil {
			return err
		}
	}
	*image.raw = raw
	return nil
}

// Convert every document in a fallback image to the value representation:
// a repeated field contributes its last value, and MarshalDocument orders
// the resulting maps. Arrays retain their positions.
func mapImageValue(value any) any {
	switch v := value.(type) {
	case bson.M:
		doc := make(bson.M, len(v))
		for key, val := range v {
			doc[key] = mapImageValue(val)
		}
		return doc
	case bson.D:
		doc := make(bson.M, len(v))
		for _, field := range v {
			doc[field.Key] = mapImageValue(field.Value)
		}
		return doc
	case bson.A:
		items := make(bson.A, len(v))
		for i := range v {
			items[i] = mapImageValue(v[i])
		}
		return items
	case primitive.CodeWithScope:
		v.Scope = mapImageValue(v.Scope)
		return v
	default:
		return v
	}
}

// MongoDB places _id first when it stores a replacement. Move the raw
// element without decoding any other BSON values.
func withIDFirst(raw bson.Raw) (bson.Raw, error) {
	fields, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	for i, field := range fields {
		if field.Key() == "_id" && i > 0 {
			result := make(bson.Raw, 0, len(raw))
			result = append(result, raw[:4]...)
			result = append(result, field...)
			for j, other := range fields {
				if i != j {
					result = append(result, other...)
				}
			}
			return append(result, 0), nil
		}
	}
	return raw, nil
}

// Older plans need their replacement order recovered at apply time.
func (s *Service) prepareApplyImages(source *wal.Branch, changes []Change) error {
	requests := imageRequests{}
	for i := range changes {
		c := &changes[i]
		id, err := documentIDValue(*c)
		if err != nil {
			return err
		}
		c.ID = id
		if c.Before != nil {
			c.Before["_id"] = id
			c.BeforeRaw, err = wal.MarshalDocument(c.Before)
			if err != nil {
				return err
			}
		}
		if !c.Delete {
			if c.Document == nil {
				return fmt.Errorf("merge plan has no replacement for %s/%s; run preview again", c.Collection, c.DocumentID)
			}
			requests.add(c.Collection, c.DocumentID, "document", c.Document, &c.DocumentRaw, nil)
		}
	}
	return s.readPlanImages(source, source.BaseLSN+1, source.HeadLSN, requests, false)
}
