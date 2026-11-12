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

// timeStep remembers which documents changed at the cluster time being
// processed. A snapshot read shows a document after, or just before,
// everything that committed at one cluster time. That is one event's image
// only when the event is the document's sole change at that time, so a
// document changed twice in one transaction cannot be recovered this way.
type timeStep struct {
	at   primitive.Timestamp
	docs map[string]bool // document → whether one of its events was recovered
}

// admit records an event and reports whether its images can be trusted:
// either MongoDB attached them, or it is the document's only change at this
// cluster time.
func (t *timeStep) admit(at primitive.Timestamp, document string, recovering bool) bool {
	if at != t.at {
		t.at = at
		clear(t.docs)
	}
	recovered, seen := t.docs[document]
	if seen && (recovered || recovering) {
		return false
	}
	if t.docs == nil {
		t.docs = make(map[string]bool)
	}
	t.docs[document] = recovered || recovering
	return true
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
