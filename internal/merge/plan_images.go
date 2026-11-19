package merge

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/argon-lab/argon/v2/internal/source"
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	orderedPlanVersion = 3
	provenanceVersion  = 1
)

// The BSON fields contain the images themselves. The maps serve callers that
// compare document values or use the ordinary JSON representation of a plan.
type changeBSON Change
type conflictBSON Conflict

func (c Change) MarshalBSON() ([]byte, error) {
	if err := c.encodeImages(imagesAsStored); err != nil {
		return nil, err
	}
	key, err := c.documentKey()
	if err != nil {
		return nil, err
	}
	if !key.IsZero() {
		c.ID = key
	}
	return wal.MarshalDocument(changeBSON(c))
}

// Keep the auxiliary key's original BSON on load and re-save. A Go caller
// may instead supply or edit ID, including an ordered compound key.
func (c *Change) documentKey() (bson.RawValue, error) {
	if !c.storedKey.IsZero() {
		var saved any
		if err := wal.UnmarshalValue(c.storedKey, &saved); err != nil {
			return bson.RawValue{}, err
		}
		if reflect.DeepEqual(saved, c.ID) {
			return c.storedKey, nil
		}
	}
	if c.ID == nil {
		return bson.RawValue{}, nil
	}
	raw, err := wal.MarshalDocument(bson.D{{Key: "key", Value: c.ID}})
	if err != nil {
		return bson.RawValue{}, err
	}
	return raw.LookupErr("key")
}

func (c *Change) encodeImages(mode imageEncoding) error {
	images := planImageEncoder{&c.SortedImages, mode}
	if err := images.encode("document", &c.DocumentRaw, &c.Document); err != nil {
		return err
	}
	return images.encode("before", &c.BeforeRaw, &c.Before)
}

func (c *Change) UnmarshalBSON(data []byte) error {
	*c = Change{}
	if err := wal.UnmarshalDocument(data, (*changeBSON)(c)); err != nil {
		return err
	}
	c.storedKey = bson.Raw(data).Lookup("document_key")
	c.storedKey.Value = slices.Clone(c.storedKey.Value)
	if c.storedKey.Type == bson.TypeNull {
		// A typed nil keeps the field present in both BSON and JSON.
		c.ID = (*primitive.Null)(nil)
	}
	return c.decodeStoredImages()
}

func (c *Change) decodeStoredImages() error {
	if err := decodeImage(c.DocumentRaw, &c.Document); err != nil {
		return err
	}
	return decodeImage(c.BeforeRaw, &c.Before)
}

func (c Conflict) MarshalBSON() ([]byte, error) {
	if err := c.encodeImages(imagesAsStored); err != nil {
		return nil, err
	}
	return bson.Marshal(conflictBSON(c))
}

func (c *Conflict) encodeImages(mode imageEncoding) error {
	images := planImageEncoder{&c.SortedImages, mode}
	if err := images.encode("base", &c.BaseRaw, &c.Base); err != nil {
		return err
	}
	if err := images.encode("ours", &c.OursRaw, &c.Ours); err != nil {
		return err
	}
	return images.encode("theirs", &c.TheirsRaw, &c.Theirs)
}

func (c *Conflict) UnmarshalBSON(data []byte) error {
	*c = Conflict{}
	if err := bson.Unmarshal(data, (*conflictBSON)(c)); err != nil {
		return err
	}
	return c.decodeStoredImages()
}

func (c *Conflict) decodeStoredImages() error {
	if err := decodeImage(c.BaseRaw, &c.Base); err != nil {
		return err
	}
	if err := decodeImage(c.OursRaw, &c.Ours); err != nil {
		return err
	}
	return decodeImage(c.TheirsRaw, &c.Theirs)
}

type imageEncoding int

const (
	imagesAsStored imageEncoding = iota
	ordinaryImages
	followedImages
)

type planImageEncoder struct {
	sorted *[]string
	mode   imageEncoding
}

func (e planImageEncoder) encode(name string, raw *bson.Raw, doc *bson.M) error {
	fromMap := *raw == nil && *doc != nil
	if fromMap {
		image, err := wal.MarshalDocument(*doc)
		if err != nil {
			return err
		}
		*raw = image
	}
	if e.mode == ordinaryImages {
		image, rebuilt, err := ordinaryImage(*raw)
		if err != nil {
			return err
		}
		if rebuilt {
			*raw = image
			if err := decodeImage(image, doc); err != nil {
				return err
			}
			fromMap = true
		}
	}
	if fromMap {
		markSorted(e.sorted, name)
	}
	if e.mode == followedImages && *raw != nil && (name == "document" || name == "theirs") {
		image, err := withIDFirst(*raw)
		if err != nil {
			return err
		}
		*raw = image
	}
	return nil
}

func markSorted(sorted *[]string, name string) {
	if sorted != nil && !slices.Contains(*sorted, name) {
		*sorted = append(slices.Clone(*sorted), name)
	}
}

func decodeImage(raw bson.Raw, doc *bson.M) error {
	if raw == nil {
		return nil
	}
	*doc = nil
	return wal.UnmarshalDocument(raw, doc)
}

// PreparePlan computes a proposal with the ordered BSON images that SavePlan
// will persist. CLI callers can encode their output before saving the plan.
func (s *Service) PreparePlan(ctx context.Context, sourceBranchID string) (*Plan, error) {
	plan, err := s.Compute(sourceBranchID)
	if err != nil {
		return nil, err
	}
	if err := s.preparePlanImages(ctx, plan); err != nil {
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

func (s *Service) preparePlanImages(ctx context.Context, plan *Plan) error {
	branch, target, err := s.endpoints(plan.SourceBranchID)
	if err != nil {
		return err
	}
	if target.ID != plan.TargetBranchID || branch.ProjectID != plan.ProjectID {
		return fmt.Errorf("merge endpoints changed; run preview again")
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
	if err := s.readPlanImages(branch, plan.BaseLSN+1, plan.SourceHead, theirs, target.IsLive() || target.IsFollower(), target.IsFollower()); err != nil {
		return err
	}
	if err := s.readPlanImages(target, 0, plan.TargetHead, ours, false, target.IsFollower()); err != nil {
		return err
	}
	if err := s.readPlanImages(branch, 0, plan.BaseLSN, base, false, target.IsFollower()); err != nil {
		return err
	}
	plan.FollowedDatabase, plan.FollowedReplicaSetID = "", ""
	if target.IsFollower() {
		binding, err := source.ReadBinding(ctx, s.plans.Database(), target.ID)
		if err != nil {
			return err
		}
		if binding == nil || binding.Database != target.Follows || binding.ProjectID != plan.ProjectID {
			return fmt.Errorf("source binding for branch %s changed during preview; run preview again", target.Name)
		}
		plan.FollowedDatabase = binding.Database
		plan.FollowedReplicaSetID = binding.ReplicaSetID
	}
	plan.FormatVersion = orderedPlanVersion
	plan.ProvenanceVersion = provenanceVersion
	return nil
}

func (s *Service) readPlanImages(branch *wal.Branch, fromLSN, toLSN int64, requests imageRequests, idFirst, preserveDuplicates bool) error {
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
			if err := image.store(entries[image.key], idFirst, preserveDuplicates); err != nil {
				return fmt.Errorf("merge image %s for %s/%s: %w", image.name, collection, image.key, err)
			}
		}
	}
	return nil
}

func (image imageRequest) store(entry *wal.Entry, idFirst, preserveDuplicates bool) error {
	id, err := wal.DocumentIDValue(image.key)
	if err != nil {
		return err
	}
	// The typed key preserves the order of a compound ID in the map view.
	image.doc["_id"] = id
	var raw bson.Raw
	// Followed targets retain all fields so write-back can identify duplicates.
	// Ordinary merges publish the single value selected by the materializer.
	if entry != nil && entry.Operation == wal.OpPut && len(entry.PostImage) > 0 &&
		entry.Metadata["capture_images"] != "attached_unverified" {
		var doc bson.M
		if err := wal.UnmarshalDocument(entry.PostImage, &doc); err != nil {
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
		raw, err = wal.MarshalDocument(image.doc)
		if err != nil {
			return err
		}
		markSorted(image.sorted, image.name)
	}
	if !preserveDuplicates {
		var rebuilt bool
		raw, rebuilt, err = ordinaryImage(raw)
		if err != nil {
			return err
		}
		if rebuilt {
			markSorted(image.sorted, image.name)
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

// Only pre-existing checked-out plans can require this comparison. Earlier
// writers marked before-images after sorting and deduplicating even code scopes.
func legacySortedBefore(doc bson.M, id any) (bson.Raw, error) {
	sorted := legacySortedValue(doc).(bson.M)
	sorted["_id"] = id
	return wal.MarshalDocument(sorted)
}

func legacySortedValue(value any) any {
	switch v := value.(type) {
	case bson.M:
		doc := make(bson.M, len(v))
		for key, val := range v {
			doc[key] = legacySortedValue(val)
		}
		return doc
	case bson.D:
		doc := make(bson.M, len(v))
		for _, field := range v {
			doc[field.Key] = legacySortedValue(field.Value)
		}
		return doc
	case bson.A:
		items := make(bson.A, len(v))
		for i := range v {
			items[i] = legacySortedValue(v[i])
		}
		return items
	case primitive.CodeWithScope:
		v.Scope = legacySortedValue(v.Scope)
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
		if field.Key() == "_id" {
			if i == 0 {
				return raw, nil
			}
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
	return s.readPlanImages(source, source.BaseLSN+1, source.HeadLSN, requests, false, false)
}
