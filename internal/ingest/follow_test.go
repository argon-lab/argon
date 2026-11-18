package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func testDatabase(t *testing.T) *mongo.Database {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database(fmt.Sprintf("argon_follow_unit_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		_ = db.Drop(context.Background())
		_ = client.Disconnect(context.Background())
	})
	return db
}

// The stream position is read from resume tokens, which MongoDB documents as
// opaque. This pins the one property relied on, against the server in use:
// an event's token decodes to that event's cluster time.
func TestTokenTimeMatchesEventClusterTime(t *testing.T) {
	db := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := db.Watch(ctx, mongo.Pipeline{}, options.ChangeStream().SetMaxAwaitTime(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close(context.Background()) }()
	if at, ok := tokenTime(stream.ResumeToken()); !ok || at.T < 1_000_000_000 {
		t.Fatalf("the stream's opening token does not decode to a cluster time: %v %v", at, ok)
	}
	session, err := db.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(ctx)
	if _, err := db.Collection("events").InsertOne(ctx, bson.M{"_id": 1}); err != nil {
		t.Fatal(err)
	}
	// A transaction's events share one cluster time and so must their tokens.
	if _, err := session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		for id := 2; id <= 4; id++ {
			if _, err := db.Collection("events").InsertOne(sc, bson.M{"_id": id}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := 0
	var previous primitive.Timestamp
	for seen < 4 && ctx.Err() == nil {
		if !stream.TryNext(ctx) {
			continue
		}
		var event changeEvent
		if err := bson.Unmarshal(stream.Current, &event); err != nil {
			t.Fatal(err)
		}
		if event.OperationType != "insert" {
			continue
		}
		seen++
		token, _ := stream.Current.Lookup("_id").DocumentOK()
		at, ok := tokenTime(token)
		if !ok || !at.Equal(event.ClusterTime) {
			t.Fatalf("token decodes to %v, the event's cluster time is %v", at, event.ClusterTime)
		}
		if at.Before(previous) {
			t.Fatalf("token times went backwards: %v after %v", at, previous)
		}
		previous = at
	}
	if seen != 4 {
		t.Fatalf("saw %d of 4 inserts", seen)
	}
	// With nothing more to return, the stream's token keeps up with the
	// oplog: it must reach a write made to another database.
	other := db.Client().Database(db.Name() + "_other")
	defer func() { _ = other.Drop(context.Background()) }()
	if _, err := other.Collection("x").InsertOne(ctx, bson.M{"_id": 1}); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		if stream.TryNext(ctx) {
			continue
		}
		if at, ok := tokenTime(stream.ResumeToken()); ok && at.After(previous) {
			return
		}
	}
	t.Fatal("the idle stream's token never moved past the last event")
}

func TestTokenTimeRejectsUnknownFormats(t *testing.T) {
	for _, token := range []bson.Raw{
		nil,
		mustRaw(bson.M{"_data": "zz"}),
		mustRaw(bson.M{"_data": "00112233445566778899"}),
		mustRaw(bson.M{"_data": primitive.Binary{Data: []byte{0x82, 1, 2, 3, 4, 5, 6, 7, 8}}}),
		mustRaw(bson.M{"other": "826AFD125B0000000100"}),
	} {
		if at, ok := tokenTime(token); ok {
			t.Fatalf("token %v decoded to %v", token, at)
		}
	}
}

func mustRaw(doc bson.M) bson.Raw {
	raw, err := bson.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

// A saved position that the source's oplog no longer holds cannot be resumed.
// MongoDB reports that as ChangeStreamHistoryLost; a follower must end as
// degraded, with the reason stored, and must not retry forever or report a
// clean stop. (A fresh replica set never loses history, so the server's
// error is supplied here rather than provoked.)
func TestFollowReportsLostOplogHistoryAsDegraded(t *testing.T) {
	db := testDatabase(t)
	service := NewService(db.Client(), db, nil, nil)
	for _, lost := range []mongo.CommandError{
		{Code: 286, Name: "ChangeStreamHistoryLost", Message: "Resume of change stream was not possible, as the resume point may no longer be in the oplog."},
		{Code: 280, Name: "ChangeStreamFatalError", Message: "cannot resume stream; the resume token was not found."},
	} {
		branchID := primitive.NewObjectID().Hex()
		if _, err := db.Collection("wal_branches").InsertOne(context.Background(), wal.Branch{ID: branchID}); err != nil {
			t.Fatal(err)
		}
		attempts := 0
		cfg := runConfig{actor: sourceActor, redact: func(text string) string { return strings.ReplaceAll(text, "hunter2", "[redacted]") }}
		cfg.stream = func(context.Context, *running) error {
			attempts++
			return fmt.Errorf("failed to open the change stream on source database shop for mongodb://app:hunter2@db: %w", lost)
		}
		run, ctx, _ := service.register(context.Background(), branchID, cfg)
		err := service.execute(ctx, branchID, cfg, run, true)
		var gap *CaptureError
		if !errors.As(err, &gap) {
			t.Fatalf("%s must be a capture gap, got %v", lost.Name, err)
		}
		if attempts != 1 {
			t.Fatalf("%s was retried %d times; lost history does not come back", lost.Name, attempts)
		}
		status, err := service.SourceStatusFor(context.Background(), branchID)
		if err != nil || status == nil {
			t.Fatalf("no stored status: %v", err)
		}
		if status.State != "degraded" || !strings.Contains(status.Error, lost.Message) {
			t.Fatalf("stored status is %q: %q", status.State, status.Error)
		}
		if strings.Contains(status.Error, "hunter2") {
			t.Fatalf("the stored error contains a credential: %q", status.Error)
		}
	}
}
