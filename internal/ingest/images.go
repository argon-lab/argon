package ingest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// When an application creates a collection, its first writes can reach the
// change stream before this service has enabled pre- and post-images on it,
// and MongoDB then leaves the images out of those events. The states are not
// lost: within its snapshot history window MongoDB returns any document as it
// stood at an earlier cluster time. The ingester asks for exactly the states
// the event would have carried. Nothing is reconstructed from update
// descriptions, so a recovered image is as exact as one MongoDB attached.

// errSnapshotGone reports that MongoDB can no longer return the requested
// point in time.
var errSnapshotGone = errors.New("the change is outside MongoDB's snapshot history")

// Server errors that mean a snapshot read at this time will not succeed
// later either. Anything else is left to the caller's retry.
var snapshotGoneCodes = map[int32]bool{
	2:   true, // BadValue
	9:   true, // FailedToParse
	13:  true, // Unauthorized
	72:  true, // InvalidOptions
	123: true, // NotAReplicaSet
	148: true, // ReadConcernMajorityNotEnabled
	239: true, // SnapshotTooOld
	246: true, // SnapshotUnavailable
}

// documentAt returns a document exactly as MongoDB held it at a cluster
// time, or nil when it did not exist then.
func documentAt(ctx context.Context, physical *mongo.Database, collection string, id bson.RawValue, at primitive.Timestamp) (bson.Raw, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var reply struct {
		Cursor struct {
			FirstBatch []bson.Raw `bson:"firstBatch"`
		} `bson:"cursor"`
	}
	err := physical.RunCommand(ctx, bson.D{
		{Key: "find", Value: collection},
		{Key: "filter", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "$eq", Value: id}}}}},
		{Key: "limit", Value: 1},
		{Key: "singleBatch", Value: true},
		{Key: "readConcern", Value: bson.D{
			{Key: "level", Value: "snapshot"},
			{Key: "atClusterTime", Value: at},
		}},
	}).Decode(&reply)
	if err != nil {
		var command mongo.CommandError
		if errors.As(err, &command) && snapshotGoneCodes[command.Code] {
			return nil, fmt.Errorf("%w: %s", errSnapshotGone, command.Message)
		}
		return nil, err
	}
	if len(reply.Cursor.FirstBatch) == 0 {
		return nil, nil
	}
	return reply.Cursor.FirstBatch[0], nil
}

// previousTimestamp is the latest cluster time strictly before at. Reading
// there shows the deployment just before whatever committed at at.
func previousTimestamp(at primitive.Timestamp) (primitive.Timestamp, bool) {
	switch {
	case at.I > 0:
		return primitive.Timestamp{T: at.T, I: at.I - 1}, true
	case at.T > 0:
		return primitive.Timestamp{T: at.T - 1, I: math.MaxUint32}, true
	default:
		return primitive.Timestamp{}, false
	}
}

// timeStep remembers the documents changed at the cluster time being
// processed. A snapshot read shows a document after, or just before,
// everything that committed at one cluster time, so within a transaction it
// gives a change's pre-image only if the change is the document's first, and
// its post-image only if it is the last.
type timeStep struct {
	at   primitive.Timestamp
	docs map[string]stepDoc
}

// stepDoc is a document's latest change at the current cluster time.
type stepDoc struct {
	// post is the document after that change when the event carried it; the
	// next change at this time has exactly this as its pre-image.
	post bson.Raw
	// readBack marks a post-image read from a snapshot, which is only right
	// if no later change to the document commits at the same time.
	readBack bool
}

// earlier returns a document's previous change at this cluster time.
func (t *timeStep) earlier(at primitive.Timestamp, document string) (stepDoc, bool) {
	if at != t.at {
		t.at = at
		clear(t.docs)
	}
	change, ok := t.docs[document]
	return change, ok
}

func (t *timeStep) record(document string, change stepDoc) {
	if t.docs == nil {
		t.docs = make(map[string]stepDoc)
	}
	t.docs[document] = change
}

// recoveredImages are the states read back for one event.
type recoveredImages struct {
	post bson.Raw
	pre  bson.Raw
}

// recoverImages reads back the images MongoDB did not attach to an event.
// The post-image of an update is required and its absence is an error; a
// pre-image is best effort, as it was before recovery existed, because undo
// already reports a missing one.
func recoverImages(ctx context.Context, physical *mongo.Database, raw bson.Raw, event *changeEvent, needPost, needPre bool) (recoveredImages, error) {
	var images recoveredImages
	id, err := raw.LookupErr("documentKey", "_id")
	if err != nil || event.ClusterTime.IsZero() {
		return images, errSnapshotGone
	}
	if needPost {
		post, err := documentAt(ctx, physical, event.NS.Collection, id, event.ClusterTime)
		if err != nil {
			return images, err
		}
		if len(post) == 0 {
			return images, fmt.Errorf("%w: the document is absent at its own change time", errSnapshotGone)
		}
		images.post = post
	}
	if needPre {
		before, ok := previousTimestamp(event.ClusterTime)
		if !ok {
			return images, nil
		}
		pre, err := documentAt(ctx, physical, event.NS.Collection, id, before)
		if err != nil {
			if errors.Is(err, errSnapshotGone) {
				return images, nil
			}
			return images, err
		}
		images.pre = pre
	}
	return images, nil
}

// documentKey identifies a document within a physical database for timeStep.
func documentKey(collection string, raw bson.Raw) string {
	id, err := raw.LookupErr("documentKey", "_id")
	if err != nil {
		return collection + "\x00"
	}
	return collection + "\x00" + string(rune(id.Type)) + string(id.Value)
}

// hasDuplicateFields reports whether a document, at any depth, names a field
// twice. MongoDB 7.0 and 8.x attach such a post-image after an update that
// moves a field: the field appears at its old position with its old value and
// again at the end with the new one, while the stored document has it once.
func hasDuplicateFields(document bson.Raw) bool {
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
		value := element.Value()
		switch value.Type {
		case bson.TypeEmbeddedDocument:
			if hasDuplicateFields(value.Document()) {
				return true
			}
		case bson.TypeArray:
			if items, err := value.Array().Values(); err == nil {
				for _, item := range items {
					if item.Type == bson.TypeEmbeddedDocument && hasDuplicateFields(item.Document()) {
						return true
					}
					if item.Type == bson.TypeArray && arrayHasDuplicateFields(item.Array()) {
						return true
					}
				}
			}
		}
	}
	return false
}

func arrayHasDuplicateFields(array bson.Raw) bool {
	items, err := array.Values()
	if err != nil {
		return false
	}
	for _, item := range items {
		if item.Type == bson.TypeEmbeddedDocument && hasDuplicateFields(item.Document()) {
			return true
		}
		if item.Type == bson.TypeArray && arrayHasDuplicateFields(item.Array()) {
			return true
		}
	}
	return false
}
