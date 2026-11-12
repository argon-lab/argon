package wal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// These tests cover collections the application creates itself. Their first
// writes reach the change stream before images are enabled, so the ingester
// reads the missing images back from MongoDB.
//
// The oracle is the database itself: after every write the test reads the
// document's stored bytes. The history capture records later must hold exactly
// those bytes as each change's post-image, and the bytes from before the
// write as its pre-image.

// change is one write as the database showed it.
type change struct {
	operation wal.OperationType
	id        string
	post      bson.Raw
	pre       bson.Raw
}

// recorder writes to a collection nothing has prepared and notes the truth.
type recorder struct {
	t       *testing.T
	c       *mongo.Collection
	current map[string]bson.Raw
	changes []change
}

func newRecorder(t *testing.T, c *mongo.Collection) *recorder {
	return &recorder{t: t, c: c, current: map[string]bson.Raw{}}
}

func (r *recorder) stored(ctx context.Context, id any) bson.Raw {
	r.t.Helper()
	raw, err := r.c.FindOne(ctx, bson.M{"_id": id}).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	require.NoError(r.t, err)
	return raw
}

// note records what a write did to one document.
func (r *recorder) note(ctx context.Context, id any) {
	r.t.Helper()
	key := wal.DocumentIDString(id)
	before, after := r.current[key], r.stored(ctx, id)
	if bytes.Equal(before, after) {
		return // the write changed nothing, so MongoDB emits no event
	}
	entry := change{operation: wal.OpPut, id: key, post: after, pre: before}
	if after == nil {
		entry.operation = wal.OpDelete
		delete(r.current, key)
	} else {
		r.current[key] = after
	}
	r.changes = append(r.changes, entry)
}

func (r *recorder) insert(doc bson.D) {
	r.t.Helper()
	ctx := context.Background()
	_, err := r.c.InsertOne(ctx, doc)
	require.NoError(r.t, err)
	r.note(ctx, doc[0].Value)
}

// update applies an update and reports whether MongoDB accepted it.
func (r *recorder) update(id, update any) bool {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.c.UpdateOne(ctx, bson.M{"_id": id}, update); err != nil {
		var rejected mongo.WriteException
		require.True(r.t, errors.As(err, &rejected), "unexpected failure: %v", err)
		return false
	}
	r.note(ctx, id)
	return true
}

func (r *recorder) replace(id any, doc bson.D) {
	r.t.Helper()
	ctx := context.Background()
	_, err := r.c.ReplaceOne(ctx, bson.M{"_id": id}, doc)
	require.NoError(r.t, err)
	r.note(ctx, id)
}

func (r *recorder) remove(id any) {
	r.t.Helper()
	ctx := context.Background()
	_, err := r.c.DeleteOne(ctx, bson.M{"_id": id})
	require.NoError(r.t, err)
	r.note(ctx, id)
}

// requireExactHistory compares the captured history with what the database
// showed, byte for byte, and returns how many entries were read back.
func (r *recorder) requireExactHistory(f *ingestFixture) (recovered int) {
	r.t.Helper()
	f.waitForEntries(r.t, r.c.Name(), len(r.changes))
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(r.t, err)
	entries, err := f.wal.GetBranchEntries(f.branchID, r.c.Name(), 0, branch.HeadLSN)
	require.NoError(r.t, err)
	require.Len(r.t, entries, len(r.changes))
	for i, want := range r.changes {
		got := entries[i]
		label := fmt.Sprintf("change %d (%s %s)", i, want.operation, want.id)
		require.Equal(r.t, want.operation, got.Operation, label)
		require.Equal(r.t, want.id, got.DocumentID, label)
		if !bytes.Equal(want.post, got.PostImage) {
			r.t.Fatalf("%s: post-image differs\n database: %s\n captured: %s", label, want.post, got.PostImage)
		}
		if !bytes.Equal(want.pre, got.PreImage) {
			r.t.Fatalf("%s: pre-image differs\n database: %s\n captured: %s", label, want.pre, got.PreImage)
		}
		if got.Metadata["capture_images"] == "snapshot" {
			recovered++
		}
	}
	state, err := f.matFull.MaterializeCollection(branch, r.c.Name())
	require.NoError(r.t, err)
	assert.Equal(r.t, f.physicalState(r.t, r.c.Name()), state)
	return recovered
}

func TestIngest_NewCollectionHistoryIsExact(t *testing.T) {
	f := newIngestFixture(t, "ingest-images")
	ctx := context.Background()
	r := newRecorder(t, f.physical.Collection("orders"))

	// Every write lands before capture starts, so no event carries an image.
	// The updates are chosen for field order and document shape, which an
	// update description cannot convey.
	r.insert(bson.D{{Key: "_id", Value: "o1"}, {Key: "a", Value: int32(1)}, {Key: "b", Value: int32(2)},
		{Key: "c", Value: bson.D{{Key: "x", Value: int32(1)}, {Key: "y", Value: bson.A{int32(1), int32(2), int32(3)}}}},
		{Key: "dotted.name", Value: int32(1)}})
	for _, update := range []any{
		bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: int32(10)}, {Key: "z", Value: "new"}}}},
		bson.D{{Key: "$rename", Value: bson.D{{Key: "a", Value: "b"}}}},
		mongo.Pipeline{{{Key: "$replaceWith", Value: bson.D{{Key: "$mergeObjects", Value: bson.A{
			bson.D{{Key: "_id", Value: "$_id"}, {Key: "z", Value: "$z"}}, "$$ROOT"}}}}}},
		bson.D{{Key: "$push", Value: bson.D{{Key: "c.y", Value: int32(4)}}}, {Key: "$unset", Value: bson.D{{Key: "c.x", Value: ""}}}},
		bson.D{{Key: "$pop", Value: bson.D{{Key: "c.y", Value: int32(-1)}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "c.y.1", Value: int32(99)}, {Key: "n.m", Value: bson.D{{Key: "deep", Value: true}}}}}},
		// Moves a field: MongoDB logs it as an insert of a field that exists.
		mongo.Pipeline{{{Key: "$unset", Value: "z"}}, {{Key: "$set", Value: bson.D{{Key: "z", Value: "back"}, {Key: "first", Value: "$b"}}}}},
	} {
		require.True(t, r.update("o1", update), "update %v", update)
	}
	r.insert(bson.D{{Key: "_id", Value: int64(2)}, {Key: "state", Value: "new"}})
	r.update(int64(2), bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: "paid"}}}})
	r.remove(int64(2))
	r.replace("o1", bson.D{{Key: "replaced", Value: true}, {Key: "kept", Value: "no"}})
	compound := bson.D{{Key: "tenant", Value: "t"}, {Key: "n", Value: int32(1)}}
	r.insert(bson.D{{Key: "_id", Value: compound}, {Key: "v", Value: int32(1)}})
	r.update(compound, bson.D{{Key: "$inc", Value: bson.D{{Key: "v", Value: int32(1)}}}})
	r.remove(compound)

	// A transaction that changes each document once.
	for _, id := range []string{"t1", "t2", "t3"} {
		r.insert(bson.D{{Key: "_id", Value: id}, {Key: "n", Value: int32(1)}})
	}
	session, err := f.client.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if _, err := r.c.UpdateOne(sc, bson.M{"_id": "t1"}, bson.M{"$inc": bson.M{"n": int32(5)}}); err != nil {
			return nil, err
		}
		if _, err := r.c.UpdateOne(sc, bson.M{"_id": "t2"}, bson.M{"$set": bson.M{"moved": true}}); err != nil {
			return nil, err
		}
		_, err := r.c.DeleteOne(sc, bson.M{"_id": "t3"})
		return nil, err
	})
	require.NoError(t, err)
	for _, id := range []string{"t1", "t2", "t3"} {
		r.note(ctx, id)
	}

	stop := f.startIngester(t)
	recovered := r.requireExactHistory(f)
	stop()
	assert.GreaterOrEqual(t, recovered, 15, "updates, replaces and deletes on the new collection are read back")
}

func TestIngest_RandomWritesToNewCollectionAreExact(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			f := newIngestFixture(t, fmt.Sprintf("ingest-images-random-%d", seed))
			r := newRecorder(t, f.physical.Collection("docs"))
			random := rand.New(rand.NewSource(seed))
			fields := []string{"a", "b", "c", "d", "e"}
			field := func() string { return fields[random.Intn(len(fields))] }
			value := func() any {
				switch random.Intn(5) {
				case 0:
					return int32(random.Intn(100))
				case 1:
					return fmt.Sprintf("s%d", random.Intn(100))
				case 2:
					return bson.A{int32(random.Intn(9)), int32(random.Intn(9))}
				case 3:
					return bson.D{{Key: field(), Value: int32(random.Intn(9))}, {Key: field() + "2", Value: "v"}}
				default:
					return random.Intn(2) == 0
				}
			}
			set := func(pairs ...any) bson.D {
				var doc bson.D
				for i := 0; i < len(pairs); i += 2 {
					doc = append(doc, bson.E{Key: pairs[i].(string), Value: pairs[i+1]})
				}
				return doc
			}
			ids := []string{"d0", "d1", "d2"}
			for step := 0; step < 90; step++ {
				id := ids[random.Intn(len(ids))]
				seq := int32(step)
				if r.current[id] == nil {
					r.insert(set("_id", id, field(), value(), "seq", seq))
					continue
				}
				switch random.Intn(9) {
				case 0:
					r.remove(id)
				case 1:
					r.replace(id, set(field(), value(), "seq", seq, field()+"r", value()))
				case 2:
					r.update(id, set("$set", set(field(), value(), "seq", seq)))
				case 3:
					r.update(id, set("$unset", set(field(), ""), "$set", set("seq", seq)))
				case 4:
					r.update(id, set("$rename", set(field(), field()+"n"), "$set", set("seq", seq)))
				case 5:
					// Rejected when the field holds a non-array; both outcomes are valid.
					r.update(id, set("$push", set(field(), value()), "$set", set("seq", seq)))
				case 6:
					r.update(id, set("$set", set(field()+"."+field(), value(), "seq", seq)))
				case 7:
					// Rebuild the document with one field moved to the front.
					r.update(id, mongo.Pipeline{set("$replaceWith", set("$mergeObjects", bson.A{
						set("_id", "$_id", field(), value()), "$$ROOT", set("seq", seq)}))})
				default:
					r.update(id, mongo.Pipeline{set("$unset", field()), set("$set", set(field(), value(), "seq", seq))})
				}
			}

			stop := f.startIngester(t)
			recovered := r.requireExactHistory(f)
			stop()
			assert.Positive(t, recovered)
		})
	}
}

// The reported failure: capture is running, the application creates a
// collection and changes a document straight away.
func TestIngest_WritesRacingImageSetupAreCaptured(t *testing.T) {
	f := newIngestFixture(t, "ingest-images-race")
	stop := f.startIngester(t)

	const collections = 25
	recorders := make([]*recorder, collections)
	for i := range recorders {
		r := newRecorder(t, f.physical.Collection(fmt.Sprintf("race_%d", i)))
		recorders[i] = r
		r.insert(bson.D{{Key: "_id", Value: "o1"}, {Key: "total", Value: int32(5)}})
		r.update("o1", bson.D{{Key: "$set", Value: bson.D{{Key: "total", Value: int32(6)}}}})
		r.insert(bson.D{{Key: "_id", Value: "o2"}})
		r.remove("o2")
	}
	for _, r := range recorders {
		r.requireExactHistory(f)
	}
	stop() // fails the test if capture stopped with a gap
}

// A snapshot read cannot show the state between two changes that commit
// together. Capture must say so instead of recording a guess.
func TestIngest_RepeatedChangeInOneTransactionIsReportedNotGuessed(t *testing.T) {
	f := newIngestFixture(t, "ingest-images-txn")
	ctx := context.Background()
	fresh := f.physical.Collection("ledger")
	_, err := fresh.InsertOne(ctx, bson.M{"_id": "x", "n": int32(0)})
	require.NoError(t, err)

	session, err := f.client.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		if _, err := fresh.UpdateOne(sc, bson.M{"_id": "x"}, bson.M{"$inc": bson.M{"n": int32(1)}}); err != nil {
			return nil, err
		}
		_, err := fresh.UpdateOne(sc, bson.M{"_id": "x"}, bson.M{"$inc": bson.M{"n": int32(1)}})
		return nil, err
	})
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = f.ingest.Run(runCtx, f.branchID)
	var gap *ingest.CaptureError
	require.True(t, errors.As(err, &gap), "capture must stop with a reported gap, got %v", err)
	assert.Contains(t, gap.Message, "more than once at one cluster time")

	// Nothing from the transaction was recorded.
	branch, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	entries, err := f.wal.GetBranchEntries(f.branchID, "ledger", 0, branch.HeadLSN)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the insert that preceded the transaction")
}
