package wal_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/ingest"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const faultCaptureDocs = 5
const faultCaptureExit = 73

func TestFaultCaptureAfterStreamBatch(t *testing.T) {
	crashAndResumeCapture(t, false)
}

func TestFaultCaptureDuringWALBatch(t *testing.T) {
	crashAndResumeCapture(t, true)
}

// Run the watcher in its own process. Canceling Run would deliberately flush
// the changes it had already read, unlike an abrupt process exit.
func crashAndResumeCapture(t *testing.T, duringWAL bool) {
	t.Helper()
	if name := os.Getenv("ARGON_FAULT_CAPTURE_DB"); name != "" {
		runCrashingWatcher(t, name, os.Getenv("ARGON_FAULT_CAPTURE_BRANCH"), duringWAL)
		return
	}

	f := newIngestFixture(t, "fault-capture")
	// The race build writes, compresses, replays and verifies 20 MiB
	// of BSON while a second instrumented process is running.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		stop, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		require.NoError(t, f.ingest.Cancel(stop, f.branchID))
	})
	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	before, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)

	var payload []byte
	if duringWAL {
		// A native transaction is kept together, even across change-stream
		// replies. Its compressed WAL rows together exceed the driver's
		// 16 MiB insert batch target, while each document fits comfortably.
		payload = make([]byte, 4<<20)
		_, err := rand.New(rand.NewSource(26)).Read(payload)
		require.NoError(t, err)
	}
	docs := make([]any, faultCaptureDocs)
	for i := range docs {
		docs[i] = bson.D{{Key: "_id", Value: "change-" + strconv.Itoa(i)}, {Key: "n", Value: int32(i)},
			{Key: "payload", Value: primitive.Binary{Data: payload}}}
	}
	session, err := f.client.StartSession()
	require.NoError(t, err)
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (any, error) {
		return f.physical.Collection("users").InsertMany(sc, docs)
	})
	require.NoError(t, err)

	// Register cleanup before starting the child. Its monitor records each
	// transaction's session before its first command is sent. This works
	// even when the parent kills a stuck child or the child exits early.
	sessions := t.TempDir()
	t.Cleanup(func() {
		files, err := os.ReadDir(sessions)
		require.NoError(t, err)
		var ids bson.A
		for _, file := range files {
			id, err := hex.DecodeString(file.Name())
			require.NoError(t, err)
			ids = append(ids, bson.Raw(id))
		}
		if len(ids) > 0 {
			reap, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			err := f.client.Database("admin").RunCommand(reap, bson.D{{Key: "killSessions", Value: ids}}).Err()
			require.NoError(t, err)
		}
	})
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "ARGON_FAULT_CAPTURE_DB="+f.metaDB.Name(), "ARGON_FAULT_CAPTURE_BRANCH="+f.branchID,
		"ARGON_FAULT_CAPTURE_SESSIONS="+sessions)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "%s", out)
	require.Equal(t, faultCaptureExit, exit.ExitCode(), "watcher missed the interruption point: %s", out)

	stopped, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	require.Equal(t, before.HeadLSN, stopped.HeadLSN)
	checkpoint := func(head int64) bson.Raw {
		t.Helper()
		raw, err := f.metaDB.Collection("wal_ingest_state").FindOne(ctx, bson.M{"_id": f.branchID}).Raw()
		require.NoError(t, err)
		storedHead, ok := raw.Lookup("capture_status", "head_lsn").AsInt64OK()
		require.True(t, ok)
		require.Equal(t, head, storedHead)
		token, ok := raw.Lookup("resume_token").DocumentOK()
		require.True(t, ok)
		return token
	}
	saved := checkpoint(stopped.HeadLSN)
	checkCaptureResumeToken(t, ctx, f.physical, saved)

	// Query without a head bound as well: hidden, committed prefix rows are duplicates too.
	collection := f.metaDB.Collection("wal_log")
	filter := bson.M{"branch_id": f.branchID, "collection": "users", "lsn": bson.M{"$gt": before.HeadLSN}}
	count, err := collection.CountDocuments(ctx, filter)
	require.NoError(t, err)
	require.Zero(t, count)

	require.NoError(t, f.ingest.Start(ctx, f.branchID))
	require.NoError(t, f.ingest.Stop(ctx, f.branchID))
	after, err := f.branches.GetBranchByID(f.branchID)
	require.NoError(t, err)
	require.NotEqual(t, saved, checkpoint(after.HeadLSN))
	rows, err := f.wal.GetBranchEntries(f.branchID, "users", before.HeadLSN+1, after.HeadLSN)
	require.NoError(t, err)
	require.Len(t, rows, faultCaptureDocs)
	state, err := f.matFull.MaterializeCollection(after, "users")
	require.NoError(t, err)
	require.Len(t, state, faultCaptureDocs+1)
	require.Equal(t, bson.M{"_id": "seed", "n": int32(0)}, state["seed"])
	for i, row := range rows {
		id := "change-" + strconv.Itoa(i)
		require.Equal(t, id, row.DocumentID)
		require.Equal(t, rows[0].TxnID, row.TxnID)
		require.NotEmpty(t, row.TxnID)
		raw, err := bson.Marshal(docs[i])
		require.NoError(t, err)
		require.True(t, bytes.Equal(raw, row.PostImage), "WAL post-image for %s", id)
		var want bson.M
		require.NoError(t, bson.Unmarshal(raw, &want))
		require.True(t, reflect.DeepEqual(want, state[id]), "materialized value for %s", id)
	}
	count, err = collection.CountDocuments(ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, faultCaptureDocs, count)
}

// Idle change-stream replies can legitimately advance the token before
// the first data batch. Ask MongoDB to resume at the actual saved token
// and prove the complete transaction is still strictly after it.
func checkCaptureResumeToken(t *testing.T, ctx context.Context, db *mongo.Database, token bson.Raw) {
	t.Helper()
	read, done := context.WithTimeout(ctx, 20*time.Second)
	defer done()
	// MongoDB requires the same stream options when resuming an opaque
	// token. Expanded events, in particular, change its encoding.
	opts := options.ChangeStream().SetResumeAfter(token).SetBatchSize(200).
		SetFullDocument(options.WhenAvailable).SetFullDocumentBeforeChange(options.WhenAvailable).
		SetShowExpandedEvents(true).SetMaxAwaitTime(500 * time.Millisecond)
	stream, err := db.Watch(read, mongo.Pipeline{}, opts)
	require.NoError(t, err)
	defer func() { _ = stream.Close(ctx) }()
	for i := 0; i < faultCaptureDocs; {
		require.True(t, stream.Next(read), "missing change-%d after saved checkpoint: %v", i, stream.Err())
		collection, _ := stream.Current.Lookup("ns", "coll").StringValueOK()
		if collection != "users" {
			continue // The previous drain's barrier can still be in the stream.
		}
		operation, _ := stream.Current.Lookup("operationType").StringValueOK()
		require.Equal(t, "insert", operation)
		id, _ := stream.Current.Lookup("documentKey", "_id").StringValueOK()
		require.Equal(t, "change-"+strconv.Itoa(i), id)
		i++
	}

}

func faultMongoClient(t *testing.T, monitor *event.CommandMonitor) *mongo.Client {
	t.Helper()
	client, err := mongo.Connect(t.Context(), options.Client().ApplyURI(metadataDeploymentURI).SetMonitor(monitor))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Disconnect(context.Background())) })
	return client
}

func runCrashingWatcher(t *testing.T, dbName, branchID string, duringWAL bool) {
	t.Helper()
	var firstWALInsert atomic.Int64
	monitor := &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			if start, _ := e.Command.Lookup("startTransaction").BooleanOK(); start {
				id := e.Command.Lookup("lsid").Document()
				path := filepath.Join(os.Getenv("ARGON_FAULT_CAPTURE_SESSIONS"), hex.EncodeToString(id))
				require.NoError(t, os.WriteFile(path, nil, 0600))
			}
			if !duringWAL || e.DatabaseName != dbName || e.CommandName != "insert" {
				return
			}
			name, _ := e.Command.Lookup("insert").StringValueOK()
			if name != "wal_log" {
				return
			}
			items, err := e.Command.Lookup("documents").Array().Values()
			if err == nil && len(items) > 0 && len(items) < faultCaptureDocs &&
				items[0].Document().Lookup("document_id").StringValue() == "change-0" {
				firstWALInsert.Store(e.RequestID)
			}
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if duringWAL {
				if e.CommandName == "insert" && firstWALInsert.Load() == e.RequestID {
					n, ok := e.Reply.Lookup("n").AsInt64OK()
					if ok && n > 0 && n < faultCaptureDocs && e.Reply.Lookup("writeErrors").Type == 0 {
						os.Exit(faultCaptureExit)
					}
				}
				return
			}
			for _, batch := range []string{"firstBatch", "nextBatch"} {
				arr, ok := e.Reply.Lookup("cursor", batch).ArrayOK()
				if !ok {
					continue
				}
				items, _ := arr.Values()
				for _, item := range items {
					doc, ok := item.DocumentOK()
					if ok {
						id, _ := doc.Lookup("documentKey", "_id").StringValueOK()
						if id == "change-0" {
							os.Exit(faultCaptureExit)
						}
					}
				}
			}
		},
	}
	client := faultMongoClient(t, monitor)
	db := client.Database(dbName)
	f := newSnapshotFixture(t, db)
	watcher := ingest.NewService(client, db, f.wal, f.branches)
	t.Fatalf("watcher returned before the injected crash: %v", watcher.Run(t.Context(), branchID))
}
